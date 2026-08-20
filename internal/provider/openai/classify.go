package openai

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// parseCodexBuckets reads the rate-limit headers every Responses reply carries.
// This is the backup for Quota: wham/usage is a private endpoint and may move,
// but live traffic always answers with these, so quota stays current either way.
//
// The units differ from the JSON endpoint — minutes here, seconds there — so
// both are normalised to the same QuotaBucket before anything else sees them.
//
// Status is left empty here; the caller applies markSpent when the reached-type
// header is present, so the header path and the JSON path scope an exhaustion
// identically. See markSpent for why marking every window rejected off one flag
// strands accounts.
func parseCodexBuckets(h http.Header) []provider.QuotaBucket {
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
		out = append(out, provider.QuotaBucket{
			Name:        name,
			Utilization: pct / 100,
			ResetsAt:    reset * 1000,
		})
	}
	return out
}

// headerFloat and headerInt TrimSpace before parsing, matching
// anthropic.retryAfter. Without it a padded "Retry-After: 3" — legal, and
// emitted by real intermediaries — fails to parse and silently degrades a
// hinted throttle to OutcomeThrottledNoHint, where the proxy guesses a backoff
// instead of honouring the one it was given. Two providers reading the same
// header two different ways is its own defect.
func headerFloat(h http.Header, k string) (float64, bool) {
	raw := strings.TrimSpace(h.Get(k))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil
}

func headerInt(h http.Header, k string) (int64, bool) {
	raw := strings.TrimSpace(h.Get(k))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	return v, err == nil
}

func (o *OpenAI) ClassifyResponse(r *http.Response) provider.Outcome {
	limitReached := r.Header.Get("x-codex-rate-limit-reached-type") != ""
	out := provider.Outcome{Buckets: parseCodexBuckets(r.Header)}
	if limitReached {
		markSpent(out.Buckets)
	}

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
