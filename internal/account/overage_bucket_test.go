package account

import (
	"errors"
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider/anthropic"
)

// liveUnifiedResponse rebuilds the exact anthropic-ratelimit-unified-* header
// set a healthy production account returned on a successful request. Only "5h"
// and "7d" are quota windows; the rest is metadata that shares the prefix.
func liveUnifiedResponse(overrides map[string]string) *http.Response {
	hdr := map[string]string{
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
	for k, v := range overrides {
		hdr[k] = v
	}
	h := http.Header{}
	for k, v := range hdr {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: h}
}

// The outage, at the level it actually broke. A healthy account served one
// request; the response's headers were parsed and recorded; every later
// request then failed with 429 no_account_ready and attempts=0, because
// "overage-status: rejected" — overage *billing* disabled for the org, not
// quota spent — had been recorded as an unscoped rejected bucket.
//
// This asserts through Select rather than through the parser so the pairing is
// covered: a parser regression that reintroduces a metadata bucket is caught
// here even if the eligibility rule itself stays correct.
func TestSelectSurvivesLiveOverageMetadata(t *testing.T) {
	m := mgr(t, acct("a", 0))
	buckets := anthropic.Classify(liveUnifiedResponse(nil)).Buckets
	m.UpdateQuota("a", buckets)

	for _, model := range []string{"claude-sonnet-4-5", "claude-opus-4-1", ""} {
		got, err := m.Select(SelectRequest{Model: model})
		if err != nil {
			t.Fatalf("Select(model=%q) after a healthy response: %v (buckets recorded: %+v)", model, err, buckets)
		}
		if got.ID != "a" {
			t.Errorf("Select(model=%q) = %q, want a", model, got.ID)
		}
	}

	recorded, _ := m.Get("a")
	for _, name := range []string{"overage", "overage-disabled", "representative", "fallback"} {
		if b, ok := recorded.Buckets[name]; ok {
			t.Errorf("recorded bucket %q is metadata, not a quota window: %+v", name, b)
		}
	}
	if len(recorded.Buckets) != 2 {
		t.Errorf("recorded %d buckets %+v, want 2 (5h and 7d)", len(recorded.Buckets), recorded.Buckets)
	}
}

// The safety rule the fix must not disable: a genuine window reporting
// "rejected" still makes the account ineligible, even when it arrives in the
// same live header set as the overage metadata.
func TestSelectStillBlocksOnRejectedRealWindow(t *testing.T) {
	m := mgr(t, acct("a", 0))
	m.UpdateQuota("a", anthropic.Classify(liveUnifiedResponse(map[string]string{
		"anthropic-ratelimit-unified-5h-status": "rejected",
	})).Buckets)

	if _, err := m.Select(SelectRequest{Model: "claude-sonnet-4-5"}); !errors.Is(err, ErrNoAccount) {
		t.Errorf("err = %v, want ErrNoAccount — a rejected 5h window must still hold the account out", err)
	}
}

// A model-scoped window parsed from headers still binds only its own model.
func TestSelectScopedWindowFromHeadersBindsOneModel(t *testing.T) {
	m := mgr(t, acct("a", 0))
	m.UpdateQuota("a", anthropic.Classify(liveUnifiedResponse(map[string]string{
		"anthropic-ratelimit-unified-7d_oi-status":      "rejected",
		"anthropic-ratelimit-unified-7d_oi-utilization": "100",
	})).Buckets)

	recorded, _ := m.Get("a")
	if _, ok := recorded.Buckets["7d_oi"]; !ok {
		t.Fatalf("7d_oi window missing from %+v", recorded.Buckets)
	}
	if _, err := m.Select(SelectRequest{Model: "claude-oi-1"}); !errors.Is(err, ErrNoAccount) {
		t.Errorf("err = %v, want ErrNoAccount for the scoped model", err)
	}
	got, err := m.Select(SelectRequest{Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("Select for an unscoped model: %v", err)
	}
	if got.ID != "a" {
		t.Errorf("selected %q, want a — only the scoped model is spent", got.ID)
	}
}
