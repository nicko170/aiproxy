#!/usr/bin/env python3
"""Print everything in a tokenizer.json that internal/privacy/tokenizer depends on.

This exists because the original inspection missed one. It printed a hand-picked
list of fields — including model.byte_fallback — but not model.ignore_merges,
which is a real BPE setting that changes what the reference produces. A field
that is never printed is a field nobody assesses, so this prints every key under
model (abbreviating vocab and merges) and every top-level section, rather than a
curated selection.

Run it against any new model file BEFORE trusting the Go tokenizer with it, and
compare the output against the values Load asserts. Load will refuse a file whose
settings differ, so a mismatch here is a signal to re-run the tokenizer gate, not
to relax the assertion.

Usage:  python3 scripts/inspect-tokenizer.py <tokenizer.json>
"""
import json
import sys

# Fields Load asserts, with the value this implementation was verified against.
ASSERTED = {
    "model.type": "BPE",
    "model.ignore_merges": True,
    "normalizer": None,
    "pre_tokenizer.Split.behavior": "Isolated",
    "pre_tokenizer.Split.invert": False,
    "pre_tokenizer.ByteLevel.add_prefix_space": False,
    "pre_tokenizer.ByteLevel.use_regex": False,
    "post_processor.type": "ByteLevel",
    "post_processor.trim_offsets": False,
    "added_tokens[].single_word/lstrip/rstrip/normalized": False,
}


def brief(value: object, limit: int = 600) -> str:
    text = json.dumps(value, ensure_ascii=False)
    return text if len(text) <= limit else text[:limit] + "..."


def main() -> None:
    with open(sys.argv[1], encoding="utf-8") as fh:
        tok = json.load(fh)

    print("== model ==")
    for key, value in tok.get("model", {}).items():
        if key in ("vocab", "merges"):
            print(f"  model.{key:<26} = <{len(value)} entries>")
            continue
        print(f"  model.{key:<26} = {brief(value)}")
    vocab = tok.get("model", {}).get("vocab", {})
    if vocab:
        print(f"  model.vocab max id           = {max(vocab.values())}")
    merges = tok.get("model", {}).get("merges", [])
    if merges:
        print(f"  model.merges[0]              = {brief(merges[0])}  ({type(merges[0]).__name__})")

    print("== sections ==")
    for key in ("version", "truncation", "padding", "normalizer",
                "pre_tokenizer", "post_processor", "decoder"):
        print(f"  {key:<32} = {brief(tok.get(key))}")

    added = tok.get("added_tokens", [])
    print(f"== added_tokens ({len(added)}) ==")
    for entry in added:
        flags = [name for name in ("single_word", "lstrip", "rstrip", "normalized")
                 if entry.get(name)]
        print(f"  {entry.get('id')} {entry.get('content')!r}"
              f"{' flags=' + ','.join(flags) if flags else ''}")

    print("== unrecognised top-level keys ==")
    known = {"version", "truncation", "padding", "added_tokens", "normalizer",
             "pre_tokenizer", "post_processor", "decoder", "model"}
    print(f"  {sorted(set(tok) - known) or 'none'}")

    print("== values Load asserts ==")
    for name, want in ASSERTED.items():
        print(f"  {name:<52} must be {want!r}")


if __name__ == "__main__":
    main()
