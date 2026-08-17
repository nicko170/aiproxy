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
		name: "negative retry-after is treated as no hint",
		res:  resp(429, map[string]string{"Retry-After": "-5"}),
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
		"anthropic-ratelimit-unified-5h-utilization": "42",
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
	if b.Utilization != 0.42 {
		t.Errorf("Utilization = %v, want 0.42 (percent normalized to a fraction)", b.Utilization)
	}
	if b.ResetsAt != 1786986000_000 {
		t.Errorf("ResetsAt = %d, want unix ms 1786986000000", b.ResetsAt)
	}
	if b.Status != "allowed" {
		t.Errorf("Status = %q", b.Status)
	}
}
