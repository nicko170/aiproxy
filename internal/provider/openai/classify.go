package openai

import (
	"net/http"
	"strconv"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// parseCodexBuckets reads the rate-limit headers every Responses reply carries.
// This is the backup for Quota: wham/usage is a private endpoint and may move,
// but live traffic always answers with these, so quota stays current either way.
//
// The units differ from the JSON endpoint — minutes here, seconds there — so
// both are normalised to the same QuotaBucket before anything else sees them.
// When limitReached is true, all emitted buckets are marked "rejected", matching
// the JSON path: a single limit flag applies to every window.
func parseCodexBuckets(h http.Header, limitReached bool) []provider.QuotaBucket {
	var out []provider.QuotaBucket
	for _, w := range []string{"primary", "secondary"} {
		pct, okPct := headerFloat(h, "x-codex-"+w+"-used-percent")
		mins, okMin := headerInt(h, "x-codex-"+w+"-window-minutes")
		reset, okRes := headerInt(h, "x-codex-"+w+"-reset-at")
		if !okPct || !okMin || !okRes {
			continue
		}
		name := bucketName(mins * 60)
		if name == "" {
			continue
		}
		b := provider.QuotaBucket{
			Name:        name,
			Utilization: pct / 100,
			ResetsAt:    reset * 1000,
		}
		if limitReached {
			b.Status = "rejected"
		}
		out = append(out, b)
	}
	return out
}

func headerFloat(h http.Header, k string) (float64, bool) {
	raw := h.Get(k)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil
}

func headerInt(h http.Header, k string) (int64, bool) {
	raw := h.Get(k)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	return v, err == nil
}

func (o *OpenAI) ClassifyResponse(r *http.Response) provider.Outcome {
	limitReached := r.Header.Get("x-codex-rate-limit-reached-type") != ""
	out := provider.Outcome{Buckets: parseCodexBuckets(r.Header, limitReached)}

	switch {
	case r.StatusCode == http.StatusTooManyRequests:
		// A named reached-type means the plan's allowance is spent, which is a
		// different thing from being asked to slow down: it holds the account
		// rather than pausing it, so rotation happens instead of waiting.
		if limitReached {
			out.Kind = provider.OutcomeQuotaRejected
			return out
		}
		if secs, ok := headerInt(r.Header, "Retry-After"); ok && secs >= 0 {
			out.Kind = provider.OutcomeThrottledWithHint
			out.RetryAfter = time.Duration(secs) * time.Second
			return out
		}
		out.Kind = provider.OutcomeThrottledNoHint
	case r.StatusCode == http.StatusUnauthorized:
		out.Kind = provider.OutcomeCredentialStale
	case r.StatusCode == http.StatusForbidden:
		out.Kind = provider.OutcomeCredentialRefused
	case r.StatusCode >= 500:
		out.Kind = provider.OutcomeServerError
	case r.StatusCode >= 400:
		out.Kind = provider.OutcomeClientError
	default:
		out.Kind = provider.OutcomeOK
	}
	return out
}
