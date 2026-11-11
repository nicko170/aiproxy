import test from "node:test";
import assert from "node:assert/strict";
import { readField } from "./form.js";

// Saving ANY setting re-read every list input and re-split it on commas, so a
// denylist entry "Acme, Inc." became ["Acme", "Inc."] after an unrelated save.
test("an untouched list field keeps its original entries", () => {
  const original = ["Acme, Inc.", "secret-host"];
  const shown = original.join(", ");
  assert.deepEqual(readField("list", shown, original), { value: original });
});

test("an edited list field is split on commas and trimmed", () => {
  assert.deepEqual(readField("list", " a, b ,, c ", ["x"]), { value: ["a", "b", "c"] });
});

// A cleared number field became NaN, which JSON sends as null and Go decodes
// as 0 — for quotaProbeIntervalSeconds, silently switching the probe off.
test("an empty or non-numeric number field is an error, not zero", () => {
  for (const kind of ["int", "number"]) {
    assert.ok(readField(kind, "", 300).error, `${kind} ""`);
    assert.ok(readField(kind, "abc", 300).error, `${kind} "abc"`);
  }
  assert.deepEqual(readField("int", " 42 ", 0), { value: 42 });
  assert.deepEqual(readField("number", "0.5", 0), { value: 0.5 });
});
