package openai

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

func hdr(kv map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range kv {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: 200, Header: h}
}

func hdrStatus(status int, kv map[string]string) *http.Response {
	h := http.Header{}
	if kv != nil {
		for k, v := range kv {
			h.Set(k, v)
		}
	}
	return &http.Response{StatusCode: status, Header: h}
}

// The header path must produce the SAME buckets as the JSON path, or quota
// silently depends on which source happened to answer last.
func TestParseCodexBucketsMatchesTheUsageEndpointShape(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	res := hdr(map[string]string{
		"x-codex-primary-used-percent":   "29",
		"x-codex-primary-window-minutes": "10080",
		"x-codex-primary-reset-at":       strconv.FormatInt(reset, 10),
	})
	got := parseCodexBuckets(res.Header)
	if len(got) != 1 {
		t.Fatalf("buckets = %+v, want 1", got)
	}
	if got[0].Name != "7d" || got[0].Utilization != 0.29 {
		t.Errorf("bucket = %+v, want 7d at 0.29", got[0])
	}
	if got[0].ResetsAt != reset*1000 {
		t.Errorf("resetsAt = %d, want millis", got[0].ResetsAt)
	}
}

func TestClassify429IsThrottled(t *testing.T) {
	out := New(http.DefaultClient).ClassifyResponse(hdrStatus(429, nil))
	if out.Kind != provider.OutcomeThrottledNoHint && out.Kind != provider.OutcomeThrottledWithHint {
		t.Errorf("kind = %v, want a throttled kind", out.Kind)
	}
}

func TestClassifyRateLimitReachedIsQuotaRejected(t *testing.T) {
	res := hdrStatus(429, map[string]string{"x-codex-rate-limit-reached-type": "usage_limit_reached"})
	out := New(http.DefaultClient).ClassifyResponse(res)
	if out.Kind != provider.OutcomeQuotaRejected {
		t.Errorf("kind = %v, want quota_rejected: this account is spent, not merely throttled", out.Kind)
	}
}

func TestClassify401IsCredentialStale(t *testing.T) {
	if got := New(http.DefaultClient).ClassifyResponse(hdrStatus(401, nil)).Kind; got != provider.OutcomeCredentialStale {
		t.Errorf("kind = %v, want credential_stale", got)
	}
}

func TestClassify5xxIsServerError(t *testing.T) {
	if got := New(http.DefaultClient).ClassifyResponse(hdrStatus(503, nil)).Kind; got != provider.OutcomeServerError {
		t.Errorf("kind = %v, want server_error", got)
	}
}

// The header path marks a bucket rejected when the reached-type header is
// present, matching the JSON path, so holdFor() and account selection see it.
func TestClassifyMarksTheSpentWindowRejectedWhenLimitReached(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	res := hdrStatus(429, map[string]string{
		"x-codex-primary-used-percent":    "100",
		"x-codex-primary-window-minutes":  "10080",
		"x-codex-primary-reset-at":        strconv.FormatInt(reset, 10),
		"x-codex-rate-limit-reached-type": "usage_limit_reached",
	})
	got := New(http.DefaultClient).ClassifyResponse(res).Buckets
	if len(got) != 1 {
		t.Fatalf("buckets = %+v, want 1", got)
	}
	if got[0].Status != "rejected" {
		t.Errorf("Status = %q, want rejected", got[0].Status)
	}
}

// IMPORTANT 4, the header half. One reached-type header used to mark BOTH
// windows rejected. UpdateQuota merges by name and never deletes, and
// eligibleLocked disqualifies on any rejected bucket, so a spent 5h window
// also held the 7d window — and once the 7d headers stopped arriving there was
// nothing left to clear it. With quotaProbe.intervalSeconds: 0, one 429 removed
// the account until restart.
func TestClassifyDoesNotRejectAWindowThatIsNotSpent(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	res := hdrStatus(429, map[string]string{
		"x-codex-primary-used-percent":     "100",
		"x-codex-primary-window-minutes":   "300",
		"x-codex-primary-reset-at":         strconv.FormatInt(reset, 10),
		"x-codex-secondary-used-percent":   "12.5",
		"x-codex-secondary-window-minutes": "10080",
		"x-codex-secondary-reset-at":       strconv.FormatInt(reset, 10),
		"x-codex-rate-limit-reached-type":  "workspace_owner_usage_limit_reached",
	})
	byName := map[string]provider.QuotaBucket{}
	for _, b := range New(http.DefaultClient).ClassifyResponse(res).Buckets {
		byName[b.Name] = b
	}
	if len(byName) != 2 {
		t.Fatalf("buckets = %+v, want 5h and 7d", byName)
	}
	if byName["5h"].Status != "rejected" {
		t.Errorf("5h status = %q, want rejected — it is the window at 100%%", byName["5h"].Status)
	}
	if byName["7d"].Status != "" {
		t.Errorf("7d status = %q, want empty — it is only 12.5%% used and a 5h exhaustion says nothing about it",
			byName["7d"].Status)
	}
}

// When the flag is set but nothing has reached 100 the exhaustion must not be
// dropped on the floor: the most-used window is held, and only that one.
func TestClassifyHoldsTheMostUsedWindowWhenNeitherReachedTheLimit(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	res := hdrStatus(429, map[string]string{
		"x-codex-primary-used-percent":     "99.6",
		"x-codex-primary-window-minutes":   "300",
		"x-codex-primary-reset-at":         strconv.FormatInt(reset, 10),
		"x-codex-secondary-used-percent":   "40",
		"x-codex-secondary-window-minutes": "10080",
		"x-codex-secondary-reset-at":       strconv.FormatInt(reset, 10),
		"x-codex-rate-limit-reached-type":  "workspace_member_credits_depleted",
	})
	byName := map[string]provider.QuotaBucket{}
	for _, b := range New(http.DefaultClient).ClassifyResponse(res).Buckets {
		byName[b.Name] = b
	}
	if byName["5h"].Status != "rejected" {
		t.Errorf("5h status = %q, want rejected — the most-used window absorbs an unattributed exhaustion",
			byName["5h"].Status)
	}
	if byName["7d"].Status != "" {
		t.Errorf("7d status = %q, want empty", byName["7d"].Status)
	}
}

// The minor that made the review: anthropic TrimSpaces Retry-After and openai
// did not, so a padded value degraded a hinted throttle to a guessed backoff.
func TestRetryAfterToleratesSurroundingWhitespace(t *testing.T) {
	res := hdrStatus(429, map[string]string{"Retry-After": "  7 "})
	out := New(http.DefaultClient).ClassifyResponse(res)
	if out.Kind != provider.OutcomeThrottledWithHint {
		t.Fatalf("kind = %v, want throttled_with_hint", out.Kind)
	}
	if out.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", out.RetryAfter)
	}
}

// A 429 with Retry-After must return the duration from the header, not just
// classify as throttled with hint.
func TestOutcomeThrottledWithHintReturnsRetryAfter(t *testing.T) {
	res := hdrStatus(429, map[string]string{"Retry-After": "60"})
	out := New(http.DefaultClient).ClassifyResponse(res)
	if out.Kind != provider.OutcomeThrottledWithHint {
		t.Errorf("kind = %v, want throttled_with_hint", out.Kind)
	}
	if out.RetryAfter != 60*time.Second {
		t.Errorf("RetryAfter = %v, want 60s", out.RetryAfter)
	}
}
