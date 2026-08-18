#!/usr/bin/env python3
"""Generate tokenizer offset fixtures for internal/privacy/tokenizer.

The Go tokenizer must agree with this reference EXACTLY. A one-character
disagreement means the NER detector reports spans that redact the wrong bytes —
silently, in a component whose entire value is being trustworthy. So the fixtures
are generated once, committed, and asserted against.

The hand-written CASES below are the readable core of the contract. They are not
enough on their own: the first implementation of the Go tokenizer passed all of
them while still disagreeing with the reference on any character whose bytes do
not merge, and on literal added tokens such as "<|endoftext|>". So a second,
seeded corpus of randomised and adversarial inputs is generated alongside them.

Usage:  uv run --with tokenizers scripts/gen-tokenizer-fixtures.py \
            <tokenizer.json> <out.json> [<extra-out.json>]
"""
import json
import random
import sys

from tokenizers import Tokenizer

CASES = [
    "",
    "hello world",
    "Contact ada@example.com or call +44 20 7946 0958.",
    "AKIAIOSFODNN7EXAMPLE",
    "Ada Lovelace lived at 12 Rue de Rivoli, Paris.",
    # Multi-byte, combining characters, and emoji: where offset bugs live.
    "héllo wörld",
    "é vs é",
    "emoji 😀 then text",
    "日本語のテキストです",
    # Whitespace shapes, which byte-level BPE encodes into tokens of their own.
    "  leading and trailing  ",
    "tabs\tand\nnewlines\r\n",
    "  ",
    # Code, which is what this filter actually sees most of.
    'const key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz";',
    "func main() {\n\tfmt.Println(x[0])\n}",
    # A long input, to exercise merge behaviour at scale.
    "lorem ipsum dolor sit amet " * 40,
]


def extra_cases() -> list[str]:
    """A seeded corpus: random unicode soup, plus inputs chosen to break offsets.

    Seeded so the file regenerates byte-for-byte, which is what makes it a
    contract rather than a snapshot of one run.
    """
    random.seed(1234)
    alpha = list("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 \t\n\r.,;:!?'\"-_/\\(){}[]<>@#$%^&*+=|~`")
    uni = list("éàüñçøßÅ日本語のテキストです中文한국어Привет😀🎉👍🏽ｱｲｳ✓→∑﷽ａｂｃ\u200b\u00a0\u0301é")
    words = ["hello", "world", "lorem", "ipsum", "sk-ant-api03", "AKIA", "Ada",
             "Lovelace", "func", "main", "import", "github.com/nicko170/aiproxy",
             "ada@example.com", "+44 20 7946 0958", "192.168.1.1",
             "<|endoftext|>", "<s>", "\\n", "'s", "'RE", "don't", "DON'T"]
    cases = []
    for _ in range(300):
        n = random.randint(0, 120)
        pool = alpha + (uni if random.random() < 0.5 else [])
        s = "".join(random.choice(pool) for _ in range(n))
        if random.random() < 0.5:
            s = " ".join([s] + random.sample(words, random.randint(1, 5)))
        cases.append(s)
    cases += words + ["", " ", "\n", "\r\n\r\n", "a" * 5000, "😀" * 200,
                      "     x", "\u0301" * 10, "ﬁ", "Ⅷ", "ｸﾞ"]
    # Input shapes this tool actually sees, added after a review pointed out the
    # corpus had none of them. They go at the end so the seeded cases above stay
    # byte-identical when the file is regenerated.
    #
    # Malformed UTF-8 is deliberately absent: a Python str cannot hold invalid
    # bytes and a JSON fixture cannot carry them, so there is no reference to
    # compare against. It is covered by TestEncodeHandlesMalformedUTF8 instead.
    cases += [
        # Unified diff hunks, which is most of what a coding agent sends.
        "@@ -1,4 +1,6 @@\n context line\n-removed = 1\n+added = 2\n+added_two = 3\n unchanged\n",
        "diff --git a/internal/privacy/filter.go b/internal/privacy/filter.go\n"
        "index 1a2b3c4..5d6e7f8 100644\n--- a/internal/privacy/filter.go\n"
        "+++ b/internal/privacy/filter.go\n@@ -12,7 +12,7 @@ func (f *Filter) Apply(s string) string {\n"
        "-\treturn f.rules.Redact(s)\n+\treturn f.rules.RedactWithNER(s)\n",
        "--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-# aiproxy\n+# aiproxy \u2014 a local privacy filter\n",
        # ANSI / terminal control sequences, which is the other half.
        "\x1b[31mFAIL\x1b[0m internal/privacy 0.12s",
        "\x1b[1;32m\u2713\x1b[0m 42 passed \x1b[2m(3.1s)\x1b[0m",
        "\x1b[2K\r\x1b[38;5;208mbuilding\x1b[0m \u2588\u2588\u2588\u2591\u2591 60%\r",
        "\x1b[?25l\x1b[10;20Hcursor moved\x1b[?25h",
        # PEM, base64 and JWT: long high-entropy runs, where BPE behaves least
        # like it does on prose.
        "-----BEGIN RSA PRIVATE KEY-----\n"
        "MIIEogIBAAKCAQEAvR2kQ8Vv7bTn1mKcXpLzYqA3sJdWfHgNrEuBtCiOxZaPlMnQ\n"
        "dKjFhGyUsVoWbXnZtQrElAiPmCdNhJkTgYzXwLbOvUeRcFaSpDqMnIyHtGkVxZuB\n"
        "wQIDAQABAoIBAB2mLfPq7sXnKdYvTgEaJcRhWuBzZiOlNxFqMsVtCpDgYkHrXeAu\n"
        "-----END RSA PRIVATE KEY-----\n",
        "-----BEGIN CERTIFICATE-----\nMIIB9TCCAV6gAwIBAgIJAK3xY9\n-----END CERTIFICATE-----",
        "SGVsbG8sIHRoaXMgaXMgYSBmYWlybHkgbG9uZyBiYXNlNjQgcnVuIHRoYXQgc2hvdWxkIGV4ZXJjaXNlIG1lcmdlcyBhdCBzY2FsZS4=",
        "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==",
        "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9."
        "eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkFkYSBMb3ZlbGFjZSIsImlhdCI6MTUxNjIzOTAyMn0."
        "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
        # Source in the two languages this repo is built from.
        "func (t *Tokenizer) Encode(s string) ([]Token, error) {\n"
        "\tif s == \"\" {\n\t\treturn nil, nil\n\t}\n"
        "\tvar out []Token\n\tfor i := range s {\n\t\t_ = i\n\t}\n"
        "\treturn out, nil\n}\n",
        "def main() -> None:\n"
        "    tok = Tokenizer.from_file(sys.argv[1])\n"
        "    for text in CASES:\n"
        "        enc = tok.encode(text, add_special_tokens=False)\n"
        "        print(f\"{len(enc.ids)} tokens\")\n",
        # Config fragments, where keys and secrets tend to live.
        "privacy:\n  enabled: true\n  model_path: ~/.aiproxy/models/privacy-filter\n"
        "  categories:\n    - PERSON\n    - EMAIL\n  api_key: sk-ant-api03-REDACTME\n",
        "{\n  \"providers\": {\n    \"anthropic\": {\n"
        "      \"base_url\": \"https://api.anthropic.com\",\n"
        "      \"api_key\": \"sk-ant-api03-abcdefghijklmnopqrstuvwxyz\",\n"
        "      \"timeout_ms\": 30000\n    }\n  }\n}",
        "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\n"
        "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n"
        "DATABASE_URL=postgres://ada:hunter2@db.internal:5432/aiproxy?sslmode=require\n",
    ]
    return cases


def encode_all(tok: Tokenizer, texts: list[str]) -> list[dict]:
    out = []
    for text in texts:
        enc = tok.encode(text, add_special_tokens=False)
        out.append({
            "text": text,
            "ids": enc.ids,
            # Byte offsets. The tokenizers library reports character offsets for
            # some configurations, so they are converted here rather than in Go:
            # the reference is the authority on what a span means.
            "offsets": [
                [len(text[:s].encode()), len(text[:e].encode())]
                for s, e in enc.offsets
            ],
        })
    return out


def main() -> None:
    tok = Tokenizer.from_file(sys.argv[1])
    out = encode_all(tok, CASES)
    json.dump(out, open(sys.argv[2], "w"), ensure_ascii=False, indent=1)
    print(f"wrote {len(out)} cases", file=sys.stderr)
    if len(sys.argv) > 3:
        extra = encode_all(tok, extra_cases())
        json.dump(extra, open(sys.argv[3], "w"), ensure_ascii=False)
        print(f"wrote {len(extra)} extra cases", file=sys.stderr)


if __name__ == "__main__":
    main()
