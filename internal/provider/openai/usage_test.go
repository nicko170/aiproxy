package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

const liveUsagePayload = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true, "limit_reached": false,
    "primary_window": { "used_percent": 29, "limit_window_seconds": 604800,
                        "reset_after_seconds": 89259, "reset_at": 1787282195 },
    "secondary_window": null
  },
  "credits": { "has_credits": false, "unlimited": false, "balance": "0" },
  "rate_limit_reached_type": null
}`

func usageServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			t.Errorf("path = %q, want /wham/usage", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// used_percent is 0..100 here and MUST be divided by 100. The Anthropic header
// is already a fraction and must not be; this repo has now got that division
// wrong in both directions, so both directions are pinned by a test.
func TestQuotaConvertsPercentAndDerivesTheWindowName(t *testing.T) {
	srv := usageServer(t, 200, liveUsagePayload)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL

	got, err := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if len(got.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want exactly one (secondary is null)", got.Buckets)
	}
	b := got.Buckets[0]
	if b.Name != "7d" {
		t.Errorf("name = %q, want 7d derived from 604800 seconds", b.Name)
	}
	if b.Utilization != 0.29 {
		t.Errorf("utilization = %v, want 0.29 from used_percent 29", b.Utilization)
	}
	if b.ResetsAt != 1787282195000 {
		t.Errorf("resetsAt = %d, want unix seconds converted to millis", b.ResetsAt)
	}
	if b.Status != "" {
		t.Errorf("status = %q, want empty while allowed", b.Status)
	}
}

// A null secondary window must produce NO bucket. A zero-utilization bucket
// with no reset would make a spent account look idle to the ranking.
func TestQuotaOmitsANullSecondaryWindow(t *testing.T) {
	srv := usageServer(t, 200, liveUsagePayload)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL
	got, _ := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	for _, b := range got.Buckets {
		if b.ResetsAt == 0 {
			t.Errorf("bucket %q has no reset; it should not exist", b.Name)
		}
	}
}

func TestQuotaMarksRejectedWhenTheLimitIsReached(t *testing.T) {
	body := `{"rate_limit":{"allowed":false,"limit_reached":true,
	  "primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":1787282195}}}`
	srv := usageServer(t, 200, body)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL

	got, _ := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	if len(got.Buckets) != 1 || got.Buckets[0].Name != "5h" {
		t.Fatalf("buckets = %+v, want one named 5h", got.Buckets)
	}
	if got.Buckets[0].Status != "rejected" {
		t.Errorf("status = %q, want rejected", got.Buckets[0].Status)
	}
}

// The endpoint is private and undocumented. A 429 on it is throttling of the
// probe itself, which internal/prober already backs off on.
func TestQuotaReportsThrottling(t *testing.T) {
	srv := usageServer(t, 429, `{}`)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL
	_, err := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	if !errors.Is(err, provider.ErrQuotaThrottled) {
		t.Errorf("err = %v, want ErrQuotaThrottled", err)
	}
}

func TestBucketName(t *testing.T) {
	for _, c := range []struct {
		secs int64
		want string
	}{
		{18000, "5h"}, {604800, "7d"}, {3600, "1h"}, {86400, "1d"}, {0, ""}, {90, "90s"},
	} {
		if got := bucketName(c.secs); got != c.want {
			t.Errorf("bucketName(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}
