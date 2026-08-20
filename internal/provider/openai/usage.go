package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrQuotaThrottled wraps the seam-level sentinel so internal/prober can back
// off without importing this package, exactly as anthropic's does.
var ErrQuotaThrottled = fmt.Errorf("%w: chatgpt usage endpoint throttled", provider.ErrQuotaThrottled)

// bucketName turns a window length into the name the rest of the system uses.
// Deriving rather than hardcoding is what lets account.windowHours and the
// expiring-allowance ranking treat an OpenAI window exactly like an Anthropic
// one, with no provider-specific branch anywhere in selection.
func bucketName(windowSeconds int64) string {
	switch {
	case windowSeconds <= 0:
		return ""
	case windowSeconds%86400 == 0:
		return strconv.FormatInt(windowSeconds/86400, 10) + "d"
	case windowSeconds%3600 == 0:
		return strconv.FormatInt(windowSeconds/3600, 10) + "h"
	default:
		return strconv.FormatInt(windowSeconds, 10) + "s"
	}
}

type usageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type usageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         bool         `json:"allowed"`
		LimitReached    bool         `json:"limit_reached"`
		PrimaryWindow   *usageWindow `json:"primary_window"`
		SecondaryWindow *usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

// toBucket converts one window. Returns ok=false for a window we cannot name or
// time, because a bucket with no reset sorts as "unknown" in the ranking and an
// invented one is worse than an absent one.
//
// Status is deliberately left empty here; markSpent decides it across the whole
// set, because the exhaustion signal is not per-window. See markSpent.
func (w *usageWindow) toBucket() (provider.QuotaBucket, bool) {
	if w == nil {
		return provider.QuotaBucket{}, false
	}
	name := bucketName(w.LimitWindowSeconds)
	if name == "" || w.ResetAt == 0 {
		return provider.QuotaBucket{}, false
	}
	return provider.QuotaBucket{
		Name: name,
		// used_percent is 0..100. See the note in the spec: the Anthropic
		// header is already a fraction and this repo has divided wrongly in
		// both directions once.
		Utilization: w.UsedPercent / 100,
		ResetsAt:    w.ResetAt * 1000,
	}, true
}

// markSpent flags, in place, the windows a single exhaustion signal applies to.
//
// The upstream reports exhaustion as ONE flag covering the whole account —
// rate_limit.limit_reached in the JSON usage payload, or the presence of
// x-codex-rate-limit-reached-type on a live response — and says nothing about
// which window is spent. Applying it to every window is not a conservative
// approximation, it strands accounts:
//
//   - account.Manager.UpdateQuota merges buckets BY NAME and never deletes one,
//     so a rejection outlives the reading that produced it;
//   - eligibleLocked disqualifies an account on ANY rejected bucket that applies
//     to the requested model;
//   - spec §5 says a later reading routinely omits secondary_window entirely
//     (it reports null), so nothing ever arrives to overwrite the stale
//     rejection.
//
// A 5h exhaustion therefore marked 7d rejected too, and once secondary_window
// went null that 7d rejection was permanent. With quotaProbe.intervalSeconds: 0
// there is no probe cycle to correct it either, so a single 429 removed the
// account until the process restarted.
//
// Scoping is done by each window's OWN used-percent, and specifically NOT by
// rate_limit_reached_type: the values that appear in the Codex binary
// (workspace_owner_usage_limit_reached, workspace_member_credits_depleted, and
// friends) name the ACTOR and the KIND of limit, never the window, so keying off
// them would be inventing a vocabulary. A window at or past 100% is spent, which
// needs no vocabulary at all.
//
// When the flag is set but no window has reached 100 — rounding, or a limit
// enforced slightly early — the single most-used window is held instead of none.
// An exhaustion signal is never ignored outright, and a demonstrably less-used
// window is never held on another's behalf. Ties keep the first window, which is
// the shorter (primary) one: holding the window that recovers soonest is the
// cheaper mistake.
func markSpent(buckets []provider.QuotaBucket) {
	if len(buckets) == 0 {
		return
	}
	// Named to avoid shadowing the `any` and `max` builtins.
	held := false
	for i := range buckets {
		if buckets[i].Utilization >= 1 {
			buckets[i].Status = "rejected"
			held = true
		}
	}
	if held {
		return
	}
	top := 0
	for i := range buckets {
		if buckets[i].Utilization > buckets[top].Utilization {
			top = i
		}
	}
	buckets[top].Status = "rejected"
}

func (o *OpenAI) Quota(ctx context.Context, c provider.Credential) (provider.Quota, error) {
	if c.Type == provider.CredentialAPIKey {
		return provider.Quota{}, provider.ErrUnsupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.chatgptBase()+"/wham/usage", nil)
	if err != nil {
		return provider.Quota{}, err
	}
	o.Authorize(req, c)
	req.Header.Set("Accept", "application/json")

	res, err := o.hc.Do(req)
	if err != nil {
		return provider.Quota{}, fmt.Errorf("openai: usage: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode == http.StatusTooManyRequests {
		return provider.Quota{}, ErrQuotaThrottled
	}
	if res.StatusCode != http.StatusOK {
		return provider.Quota{}, fmt.Errorf("openai: usage: HTTP %d", res.StatusCode)
	}

	var ur usageResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return provider.Quota{}, fmt.Errorf("openai: usage: %w", err)
	}
	out := provider.Quota{ObservedAt: time.Now().UnixMilli()}
	if ur.RateLimit == nil {
		return out, nil
	}
	if b, ok := ur.RateLimit.PrimaryWindow.toBucket(); ok {
		out.Buckets = append(out.Buckets, b)
	}
	if b, ok := ur.RateLimit.SecondaryWindow.toBucket(); ok {
		out.Buckets = append(out.Buckets, b)
	}
	if ur.RateLimit.LimitReached {
		markSpent(out.Buckets)
	}
	return out, nil
}
