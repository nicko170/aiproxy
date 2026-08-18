package anthropic

// Anthropic reports the same quota number on two different scales depending
// on which endpoint answers, and this package has one call site for each:
//
//   - The /api/oauth/usage JSON body (bucketValue, in usage.go) reports a
//     PERCENTAGE, 0..100.
//   - The anthropic-ratelimit-unified-* response HEADERS (parseBuckets, in
//     classify.go) report a FRACTION, 0..1.
//
// Both were measured live against the same account inside the same minute:
//
//	JSON   /api/oauth/usage   five_hour.utilization = 56   seven_day.utilization = 12
//	HEADER response           5h-utilization = 0.55        7d-utilization = 0.12
//
// 56 and 0.55 describe the same 55-56% used; 12 and 0.12 describe the same
// 12%. A prior version divided by 100 at both call sites, which is correct
// for the JSON percentage and wrong for the header fraction: it took an
// already-0..1 number and divided it again, so a real 55% read back as
// 0.55%. Nothing about that number looks broken in isolation — it is exactly
// the "believable but wrong" failure this file exists to rule out — which is
// why the scale is named explicitly here instead of left as an inferred
// property of whichever endpoint happens to be calling.
type utilizationScale int

const (
	// fractionScale: raw is already a 0..1 fraction. The response headers.
	fractionScale utilizationScale = iota
	// percentScale: raw is a 0..100 percentage. The JSON usage body.
	percentScale
)

// normalizeUtilization converts raw into a 0..1 fraction under scale, and
// reports whether raw was in range for that scale.
//
// A header above 1.0, or a percentage above 100, means the assumption this
// scale encodes no longer holds for that source: the upstream shape changed,
// or the two scales got crossed again. Returning ok=false rather than
// clamping or dividing anyway is deliberate — either of those would produce
// a new plausible-looking number from a value we no longer understand, which
// is the class of bug this whole file is about. Callers must not silently
// store raw on ok=false; see parseBuckets and bucketValue, which instead
// mark the window's Status "rejected" so account selection fails closed on
// it rather than trusting a fabricated utilization.
func normalizeUtilization(raw float64, scale utilizationScale) (frac float64, ok bool) {
	if scale == percentScale {
		if raw < 0 || raw > 100 {
			return 0, false
		}
		return raw / 100, true
	}
	if raw < 0 || raw > 1 {
		return 0, false
	}
	return raw, true
}
