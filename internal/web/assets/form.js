// Settings form parsing: pure, so node --test covers it (settings.js imports
// app.js, which only loads in a browser).

// readField turns one input's raw text into the value to send, or an error to
// show beside it. original is the value the form was loaded with.
//
// A list field whose text is unchanged returns the ORIGINAL list rather than
// re-splitting it: entries may contain commas ("Acme, Inc."), and an unrelated
// save must not rewrite them. A number field that is empty or not a number is
// an error, never 0 — JSON would send NaN as null, which Go decodes as zero.
export function readField(kind, raw, original) {
  if (kind === "list") {
    if (Array.isArray(original) && raw === original.join(", ")) return { value: original };
    return { value: raw.split(",").map((s) => s.trim()).filter(Boolean) };
  }
  if (kind === "int" || kind === "number") {
    const t = String(raw).trim();
    const n = kind === "int" ? Number.parseInt(t, 10) : Number.parseFloat(t);
    if (t === "" || !Number.isFinite(n) || !/^-?\d*\.?\d+$/.test(t)) return { error: "must be a number" };
    return { value: n };
  }
  return { value: raw };
}
