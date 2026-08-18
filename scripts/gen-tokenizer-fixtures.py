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
