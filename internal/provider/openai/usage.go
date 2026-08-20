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
func (w *usageWindow) toBucket(limitReached bool) (provider.QuotaBucket, bool) {
	if w == nil {
		return provider.QuotaBucket{}, false
	}
	name := bucketName(w.LimitWindowSeconds)
	if name == "" || w.ResetAt == 0 {
		return provider.QuotaBucket{}, false
	}
	b := provider.QuotaBucket{
		Name: name,
		// used_percent is 0..100. See the note in the spec: the Anthropic
		// header is already a fraction and this repo has divided wrongly in
		// both directions once.
		Utilization: w.UsedPercent / 100,
		ResetsAt:    w.ResetAt * 1000,
	}
	if limitReached {
		b.Status = "rejected"
	}
	return b, true
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
	if b, ok := ur.RateLimit.PrimaryWindow.toBucket(ur.RateLimit.LimitReached); ok {
		out.Buckets = append(out.Buckets, b)
	}
	if b, ok := ur.RateLimit.SecondaryWindow.toBucket(ur.RateLimit.LimitReached); ok {
		out.Buckets = append(out.Buckets, b)
	}
	return out, nil
}
