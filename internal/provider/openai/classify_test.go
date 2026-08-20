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
