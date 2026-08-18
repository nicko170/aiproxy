package anthropic

import (
	"net/http"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

func resp(status int, hdr map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		res        *http.Response
		wantKind   provider.OutcomeKind
		wantRetry  time.Duration
		wantScoped string
	}{{
		name:     "200 is ok",
		res:      resp(200, nil),
		wantKind: provider.OutcomeOK,
	}, {
		// The exact shape captured from production: 429, no retry-after,
		// no ratelimit headers. Inventing a delay here is the defect.
		name:     "bare 429 with no headers is throttled without hint",
		res:      resp(429, nil),
		wantKind: provider.OutcomeThrottledNoHint,
	}, {
		name:      "429 with retry-after is throttled with hint",
		res:       resp(429, map[string]string{"Retry-After": "7"}),
		wantKind:  provider.OutcomeThrottledWithHint,
		wantRetry: 7 * time.Second,
	}, {
		name:     "retry-after of zero is a hint of zero, not a missing hint",
		res:      resp(429, map[string]string{"Retry-After": "0"}),
		wantKind: provider.OutcomeThrottledWithHint,
	}, {
		name:     "unparseable retry-after is treated as no hint",
		res:      resp(429, map[string]string{"Retry-After": "soon"}),
		wantKind: provider.OutcomeThrottledNoHint,
	}, {
		name:     "negative retry-after is treated as no hint",
		res:      resp(429, map[string]string{"Retry-After": "-5"}),
		wantKind: provider.OutcomeThrottledNoHint,
	}, {
		name: "5h rejected is quota rejection",
		res: resp(429, map[string]string{
			"anthropic-ratelimit-unified-5h-status": "rejected",
		}),
		wantKind: provider.OutcomeQuotaRejected,
	}, {
		name: "7d rejected is quota rejection",
		res: resp(429, map[string]string{
			"anthropic-ratelimit-unified-7d-status": "rejected",
		}),
		wantKind: provider.OutcomeQuotaRejected,
	}, {
		name: "model scoped rejection names the model and does not reject generally",
		res: resp(429, map[string]string{
			"anthropic-ratelimit-unified-7d_oi-status": "rejected",
		}),
		wantKind:   provider.OutcomeQuotaRejected,
		wantScoped: "7d_oi",
	}, {
		name: "quota rejection wins over a retry-after hint",
		res: resp(429, map[string]string{
			"Retry-After":                           "3",
			"anthropic-ratelimit-unified-5h-status": "rejected",
		}),
		wantKind: provider.OutcomeQuotaRejected,
	}, {
		name:     "401 is a stale credential",
		res:      resp(401, nil),
		wantKind: provider.OutcomeCredentialStale,
	}, {
		name:     "403 is a refused credential",
		res:      resp(403, nil),
		wantKind: provider.OutcomeCredentialRefused,
	}, {
		name:     "400 is a client error",
		res:      resp(400, nil),
		wantKind: provider.OutcomeClientError,
	}, {
		name:     "503 is a server error",
		res:      resp(503, nil),
		wantKind: provider.OutcomeServerError,
	}, {
		// 529 is Anthropic's own overload signal, not a generic 5xx: it is
		// transient by definition and worth waiting out, where a 500 or 503
		// says nothing about whether retrying helps.
		name:     "529 is overloaded, not a generic server error",
		res:      resp(529, nil),
		wantKind: provider.OutcomeOverloaded,
	}, {
		name:      "529 with retry-after carries the hint",
		res:       resp(529, map[string]string{"Retry-After": "3"}),
		wantKind:  provider.OutcomeOverloaded,
		wantRetry: 3 * time.Second,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.res)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			if got.RetryAfter != tc.wantRetry {
				t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, tc.wantRetry)
			}
			if got.ScopedModel != tc.wantScoped {
				t.Errorf("ScopedModel = %q, want %q", got.ScopedModel, tc.wantScoped)
			}
		})
	}
}

func TestClassifyParsesQuotaBuckets(t *testing.T) {
	res := resp(200, map[string]string{
		"anthropic-ratelimit-unified-5h-status":      "allowed",
		"anthropic-ratelimit-unified-5h-utilization": "0.42",
		"anthropic-ratelimit-unified-5h-reset":       "1786986000",
	})

	out := Classify(res)
	if len(out.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1", len(out.Buckets))
	}
	b := out.Buckets[0]
	if b.Name != "5h" {
		t.Errorf("Name = %q, want %q", b.Name, "5h")
	}
	// The header is already a 0..1 fraction — not a percentage, unlike the
	// JSON usage endpoint (bucketValue, usage.go). See scale.go.
	if b.Utilization != 0.42 {
		t.Errorf("Utilization = %v, want 0.42 (header fraction stored as-is)", b.Utilization)
	}
	if b.ResetsAt != 1786986000_000 {
		t.Errorf("ResetsAt = %d, want unix ms 1786986000000", b.ResetsAt)
	}
	if b.Status != "allowed" {
		t.Errorf("Status = %q", b.Status)
	}
}

// liveUnifiedHeaders is the exact anthropic-ratelimit-unified-* header set
// captured from a healthy production account on a successful request. Most of
// it is metadata, not quota windows: only "5h" and "7d" describe an allowance.
//
// Treating the rest as windows caused an outage. "overage-status: rejected"
// says overage billing is disabled for the organisation — as the sibling
// "overage-disabled-reason: org_level_disabled" states outright, and as is the
// default for most orgs — yet it arrived at account selection as a rejected,
// unscoped bucket and made the account permanently ineligible after its first
// successful response.
func liveUnifiedHeaders() map[string]string {
	return map[string]string{
		"anthropic-ratelimit-unified-5h-status":               "allowed",
		"anthropic-ratelimit-unified-5h-utilization":          "0.0",
		"anthropic-ratelimit-unified-5h-reset":                "1787025600",
		"anthropic-ratelimit-unified-7d-status":               "allowed",
		"anthropic-ratelimit-unified-7d-utilization":          "0.07",
		"anthropic-ratelimit-unified-7d-reset":                "1787446800",
		"anthropic-ratelimit-unified-fallback-percentage":     "0.5",
		"anthropic-ratelimit-unified-overage-status":          "rejected",
		"anthropic-ratelimit-unified-overage-disabled-reason": "org_level_disabled",
		"anthropic-ratelimit-unified-representative-claim":    "five_hour",
		"anthropic-ratelimit-unified-reset":                   "1787025600",
		"anthropic-ratelimit-unified-status":                  "allowed",
	}
}

func TestClassifyKeepsOnlyRealWindowsFromLiveHeaders(t *testing.T) {
	out := Classify(resp(200, liveUnifiedHeaders()))

	if out.Kind != provider.OutcomeOK {
		t.Errorf("Kind = %v, want OutcomeOK", out.Kind)
	}

	got := map[string]provider.QuotaBucket{}
	for _, b := range out.Buckets {
		if _, dup := got[b.Name]; dup {
			t.Errorf("bucket %q emitted twice", b.Name)
		}
		got[b.Name] = b
	}

	// Metadata sharing the header prefix must never become a bucket: an
	// unscoped bucket binds every model, and "overage" carried "rejected".
	for _, name := range []string{"overage", "overage-disabled", "representative", "fallback"} {
		if b, ok := got[name]; ok {
			t.Errorf("bucket %q is metadata, not a quota window (parsed as %+v)", name, b)
		}
	}

	want := map[string]provider.QuotaBucket{
		// Headers are 0..1 fractions, not percentages: "0.07" is 7% used,
		// stored as-is. See scale.go.
		"5h": {Name: "5h", Status: "allowed", Utilization: 0, ResetsAt: 1787025600_000},
		"7d": {Name: "7d", Status: "allowed", Utilization: 0.07, ResetsAt: 1787446800_000},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d buckets %v, want exactly %d (5h and 7d)", len(got), out.Buckets, len(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Fatalf("bucket %q missing; parsed %+v", name, out.Buckets)
		}
		if g.Name != w.Name || g.Status != w.Status || g.ResetsAt != w.ResetsAt {
			t.Errorf("bucket %q = %+v, want %+v", name, g, w)
		}
		if diff := g.Utilization - w.Utilization; diff > 1e-12 || diff < -1e-12 {
			t.Errorf("bucket %q Utilization = %v, want %v", name, g.Utilization, w.Utilization)
		}
	}
}

func TestParseBucketsIgnoresNonWindowNames(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool // want a bucket emitted
	}{
		{"plain window", "anthropic-ratelimit-unified-5h-status", true},
		{"multi digit window", "anthropic-ratelimit-unified-30d-status", true},
		{"model scoped window", "anthropic-ratelimit-unified-7d_oi-status", true},
		{"overage metadata", "anthropic-ratelimit-unified-overage-status", false},
		{"hyphenated metadata", "anthropic-ratelimit-unified-overage-disabled-reason", false},
		{"representative claim", "anthropic-ratelimit-unified-representative-claim", false},
		{"fallback percentage", "anthropic-ratelimit-unified-fallback-percentage", false},
		// The overall unified values carry no window name at all. Dropping
		// them is deliberate: eligibility is decided per window.
		{"overall status", "anthropic-ratelimit-unified-status", false},
		{"overall reset", "anthropic-ratelimit-unified-reset", false},
		// A field we cannot read must not mint a phantom window either.
		{"unknown field on a window", "anthropic-ratelimit-unified-5h-flavour", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBuckets(resp(200, map[string]string{tc.header: "rejected"}).Header)
			if (len(got) > 0) != tc.want {
				t.Errorf("parseBuckets(%s) = %+v, want emitted=%v", tc.header, got, tc.want)
			}
		})
	}
}

// A model-scoped window is a real window and must survive the metadata filter,
// carrying its scope so Classify reports it as scoped rather than general.
func TestClassifyKeepsModelScopedWindow(t *testing.T) {
	out := Classify(resp(429, map[string]string{
		"anthropic-ratelimit-unified-7d_oi-status":         "rejected",
		"anthropic-ratelimit-unified-7d_oi-utilization":    "1", // fully used; header is a 0..1 fraction, not a percentage
		"anthropic-ratelimit-unified-overage-status":       "rejected",
		"anthropic-ratelimit-unified-representative-claim": "seven_day",
	}))

	if out.Kind != provider.OutcomeQuotaRejected {
		t.Errorf("Kind = %v, want OutcomeQuotaRejected", out.Kind)
	}
	if out.ScopedModel != "7d_oi" {
		t.Errorf("ScopedModel = %q, want %q — a metadata bucket must not turn a scoped rejection into a general one", out.ScopedModel, "7d_oi")
	}
	if len(out.Buckets) != 1 || out.Buckets[0].Name != "7d_oi" {
		t.Fatalf("Buckets = %+v, want exactly 7d_oi", out.Buckets)
	}
	if out.Buckets[0].Utilization != 1 {
		t.Errorf("Utilization = %v, want 1", out.Buckets[0].Utilization)
	}
}

// isModelScoped decides whether a quota rejection holds the whole account or
// only one model family. Getting it wrong in the "scoped" direction fails OPEN:
// ScopedModel is set, the attempt loop skips MarkRateLimited, and a spent
// account keeps being selected.
func TestIsModelScopedOnlyRecognisesKnownModelScopes(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"5h", false},
		{"7d", false},
		{"7d_oi", true},
		{"7d_opus", true},
		{"7d_fable", true},
		{"7d_sonnet", true},
		{"7d_haiku", true},
		// A future window that is merely qualified, not model-scoped. Shaped
		// exactly like a scoped one, so only the known-suffix check separates
		// them — and treating it as scoped would silently stop the account from
		// ever being marked rate-limited.
		{"24h_soft", false},
		{"7d_hard", false},
		{"5h_burst", false},
		// Metadata that shares the header prefix and happens to contain "_".
		{"overage_status", false},
		{"representative_claim", false},
		// A scope-shaped tail on something that is not a window at all.
		{"_oi", false},
		{"7d_", false},
	}
	for _, c := range cases {
		if got := isModelScoped(c.name); got != c.want {
			t.Errorf("isModelScoped(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// The end-to-end consequence: a rejected window whose suffix is not a known
// model scope must be reported as a GENERAL rejection, so the caller holds the
// account rather than treating it as one family's problem.
func TestUnknownWindowSuffixIsAGeneralRejectionNotAModelScopedOne(t *testing.T) {
	out := Classify(resp(429, map[string]string{
		"anthropic-ratelimit-unified-24h_soft-status": "rejected",
	}))
	if out.Kind != provider.OutcomeQuotaRejected {
		t.Fatalf("Kind = %v, want quota rejected", out.Kind)
	}
	if out.ScopedModel != "" {
		t.Errorf("ScopedModel = %q, want empty — an unrecognised suffix must not make a general rejection model-scoped, which would skip MarkRateLimited", out.ScopedModel)
	}
}

// The regression this whole fix is about. These are the real values captured
// live from one account within the same minute as the JSON evidence in
// TestBucketValueAcceptsJSONPercentage: the header carried 5h-utilization =
// 0.55 and 7d-utilization = 0.12, describing the same account the JSON
// endpoint separately reported as 56% and 12% used.
//
// A prior version divided this header value by 100 — correct for the JSON
// percentage, wrong here — so 0.55 read back as 0.0055: an account at 55%
// looked 1% used, and eligibleLocked's switchThreshold comparison (0.98)
// never tripped. This must fail against that code.
func TestParseBucketsHeaderIsAlreadyAFraction(t *testing.T) {
	h := resp(200, map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "0.55",
		"anthropic-ratelimit-unified-7d-utilization": "0.12",
	}).Header

	got := parseBuckets(h)
	byName := map[string]provider.QuotaBucket{}
	for _, b := range got {
		byName[b.Name] = b
	}

	if b := byName["5h"]; b.Utilization != 0.55 {
		t.Errorf("5h Utilization = %v, want 0.55", b.Utilization)
	}
	if b := byName["7d"]; b.Utilization != 0.12 {
		t.Errorf("7d Utilization = %v, want 0.12", b.Utilization)
	}
}

// The two scales, live evidence for the same account in the same minute
// (see scale.go), must agree once each is normalized: the header's 0.55/0.12
// fraction and the JSON endpoint's 56/12 percentage both describe ~55-56%
// and ~12% used. This is the assertion that would have caught the original
// bug — a header-path regression that reintroduces the /100 division moves
// the header-derived value far outside this tolerance.
func TestHeaderAndJSONUtilizationAgreeForTheSameQuota(t *testing.T) {
	cases := []struct {
		name           string
		headerFraction string
		jsonPercent    float64
	}{
		{"5h", "0.55", 56},
		{"7d", "0.12", 12},
	}
	const tolerance = 0.02 // measurements taken moments apart, not identical

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			headerBuckets := parseBuckets(resp(200, map[string]string{
				"anthropic-ratelimit-unified-" + c.name + "-utilization": c.headerFraction,
			}).Header)
			if len(headerBuckets) != 1 {
				t.Fatalf("got %d buckets from header, want 1", len(headerBuckets))
			}
			fromHeader := headerBuckets[0].Utilization

			fromJSON, ok := normalizeUtilization(c.jsonPercent, percentScale)
			if !ok {
				t.Fatalf("normalizeUtilization(%v, percentScale) rejected an in-range percentage", c.jsonPercent)
			}

			if diff := fromHeader - fromJSON; diff > tolerance || diff < -tolerance {
				t.Errorf("header-derived %v and JSON-derived %v disagree by more than %v (header=%q json=%v%%)",
					fromHeader, fromJSON, tolerance, c.headerFraction, c.jsonPercent)
			}
		})
	}
}
