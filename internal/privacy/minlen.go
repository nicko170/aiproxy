package privacy

// MinScanBytes is the shortest string the rule and model detectors will look at.
//
// No credential format fits in fewer bytes, and the overwhelming majority of
// strings in a provider request are short protocol values — "user", "text",
// "tool_use" — so skipping them is most of the scan budget saved for free.
//
// The check lives inside each detector rather than in the pipeline, because the
// operator denylist must NOT honour it: a four-byte project codename is a
// perfectly reasonable thing to ask to have redacted, and silently ignoring an
// explicit instruction is the worst failure this component could have. Keeping
// the rule per-detector means the pipeline needs no exceptions.
const MinScanBytes = 8
