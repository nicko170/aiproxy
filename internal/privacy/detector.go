package privacy

import (
	"context"
	"sort"
)

// Finding is one sensitive span within a single decoded string. Offsets are
// byte offsets into that string, not into the request body: detectors never see
// the body, only values handed to them one at a time.
type Finding struct {
	Start, End int
	Label      Label
	// Rule names what fired. Diagnostic only, and narrower than it looks: it is
	// the final tiebreak in Resolve, it appears in the error a bad span
	// produces, and that is all. No UI surface reports it — the Activity feed
	// renders metrics rows, which carry no per-finding detail — so do not treat
	// it as a display string.
	Rule string
	// Confidence is 1.0 for deterministic rules and the model's score for NER
	// findings. Nothing filters on it today; it exists so a future threshold is
	// a config change rather than an interface change.
	Confidence float64
}

// Detector finds sensitive spans in one decoded string.
//
// Implementations must be safe for concurrent use: one Filter is shared by every
// in-flight request. They must also be pure with respect to the input — the
// scan cache keys on content alone, so a detector that returned different
// findings for the same text would produce results that depend on cache state.
type Detector interface {
	Name() string
	Scan(ctx context.Context, text string) ([]Finding, error)
}

// Resolve flattens per-detector results into one ordered, non-overlapping set.
//
// perDetector is grouped BY DETECTOR IN REGISTRATION ORDER, and that ordering is
// the point: it is the third sort key, so two detectors reporting the identical
// span always resolve the same way. Were it a field on Finding, a detector could
// misreport its own priority and the same body would redact differently between
// runs — which shows up as a collapsed prompt-cache hit rate rather than as an
// obvious bug.
//
// Sort is start ascending, then length descending, then registration order. The
// longer span wins an overlap: the whole connection string rather than the
// password inside it.
func Resolve(perDetector [][]Finding) []Finding {
	type ranked struct {
		f   Finding
		idx int
	}
	var all []ranked
	for i, group := range perDetector {
		for _, f := range group {
			if f.End <= f.Start {
				// An empty or inverted span is a detector bug. Dropping it here
				// keeps every consumer downstream free of the check.
				continue
			}
			all = append(all, ranked{f: f, idx: i})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.f.Start != b.f.Start {
			return a.f.Start < b.f.Start
		}
		if a.f.End != b.f.End {
			return a.f.End > b.f.End // longer first
		}
		if a.idx != b.idx {
			return a.idx < b.idx
		}
		// Rule is the final tiebreak, so no two findings are ever "equal" to the
		// comparator and the result cannot depend on the sort's stability. One
		// detector CAN report the same span twice — openai-key and anthropic-key
		// both match an sk-ant-... value — and without this the kept finding is
		// whichever the sort happened to leave first.
		return a.f.Rule < b.f.Rule
	})

	out := make([]Finding, 0, len(all))
	end := -1
	for _, r := range all {
		if r.f.Start < end {
			continue // overlaps something already kept
		}
		out = append(out, r.f)
		end = r.f.End
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
