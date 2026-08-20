package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrQuotaThrottled reports that the zero-spend usage endpoint rate-limited us.
// It is distinct from a failure to read: the caller must back off rather than
// retry, and must not treat previously observed quota as fresh.
//
// It wraps provider.ErrQuotaThrottled so internal/prober can back off on the
// generic seam-level sentinel without importing this concrete provider (spec
// aims the seam at "additional providers" later); errors.Is(err,
// ErrQuotaThrottled) still works exactly as before for existing callers.
var ErrQuotaThrottled = fmt.Errorf("%w: usage endpoint throttled", provider.ErrQuotaThrottled)

const usageBeta = "oauth-2025-04-20"

// apiVersion is the Anthropic API version this client speaks. Normally a
// client supplies its own and the proxy forwards it; these are aiproxy's own
// reads, which have no client behind them.
const apiVersion = "2023-06-01"

func (a *Anthropic) get(ctx context.Context, path string, c provider.Credential, beta bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL()+path, nil)
	if err != nil {
		return nil, 0, err
	}
	a.Authorize(req, c)
	req.Header.Set("Accept", "application/json")
	// Required by every VERSIONED endpoint, and harmless on the internal ones.
	//
	// This helper was written for /api/oauth/usage, which is unversioned and
	// answers happily without it, so its absence went unnoticed until Models
	// reused the helper for /v1/models — a public endpoint that refuses with
	// "anthropic-version: header is required" and therefore returned an empty
	// catalogue for every Anthropic account. Set unconditionally rather than
	// per-caller so the next endpoint added here cannot repeat that: verified
	// against the live usage endpoint, which returns byte-identical payloads
	// with and without it.
	req.Header.Set("anthropic-version", apiVersion)
	if beta {
		req.Header.Set("anthropic-beta", usageBeta)
	}

	res, err := a.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	return body, res.StatusCode, nil
}

type profileResponse struct {
	Account *struct {
		UUID         string `json:"uuid"`
		Email        string `json:"email"`
		DisplayName  string `json:"display_name"`
		HasClaudeMax bool   `json:"has_claude_max"`
		HasClaudePro bool   `json:"has_claude_pro"`
	} `json:"account"`
	Organization *struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

func (a *Anthropic) Profile(ctx context.Context, c provider.Credential) (provider.Profile, error) {
	body, status, err := a.get(ctx, "/api/oauth/profile", c, false)
	if err != nil {
		return provider.Profile{}, fmt.Errorf("profile: %w", err)
	}
	if status != http.StatusOK {
		return provider.Profile{}, fmt.Errorf("profile: HTTP %d", status)
	}

	var pr profileResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return provider.Profile{}, fmt.Errorf("profile: %w", err)
	}
	out := provider.Profile{}
	if pr.Account != nil {
		out.AccountUUID = pr.Account.UUID
		out.Email = pr.Account.Email
		out.DisplayName = pr.Account.DisplayName
		switch {
		case pr.Account.HasClaudeMax:
			out.Plan = "max"
		case pr.Account.HasClaudePro:
			out.Plan = "pro"
		}
	}
	if pr.Organization != nil {
		out.OrgUUID = pr.Organization.UUID
		out.OrgName = pr.Organization.Name
	}
	return out, nil
}

type usageBucketJSON struct {
	Utilization    *float64 `json:"utilization"`
	UsedPercentage *float64 `json:"used_percentage"`
	Percent        *float64 `json:"percent"`
	ResetsAt       any      `json:"resets_at"`
}

type usageResponse struct {
	FiveHour *usageBucketJSON `json:"five_hour"`
	SevenDay *usageBucketJSON `json:"seven_day"`
	Limits   []struct {
		Group    string   `json:"group"`
		Percent  *float64 `json:"percent"`
		ResetsAt any      `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// modelBucketName turns a display name like "Claude Fable 5" into a stable
// bucket key like "7d_fable", so a bucket survives cosmetic renames upstream.
//
// When no name token survives (e.g. "Claude 5", or an empty display name),
// this must fail closed rather than open: BucketAppliesTo treats any "_"
// suffix as a model scope, so returning something like "7d_model" would make
// a spent bucket silently non-binding for every real model — the exact
// opposite of safe. Returning the plain, unscoped "7d" instead makes it merge
// with the general weekly bucket, which is the conservative outcome: the
// bucket then binds every model, same as an ordinary window bucket.
func modelBucketName(displayName string) string {
	s := strings.ToLower(displayName)
	s = strings.ReplaceAll(s, "claude", " ")
	s = nonAlnum.ReplaceAllString(s, " ")
	for _, word := range strings.Fields(s) {
		// Skip pure version numbers ("5" in "Claude Fable 5") and take the first
		// real name token, so the key survives cosmetic renames upstream.
		if strings.IndexFunc(word, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			continue
		}
		return "7d_" + word
	}
	return "7d"
}

// quotaReading distinguishes "this window was absent from the body" from
// "this window was present but its value made no sense", so a caller can
// treat them differently: a missing window contributes no bucket, while an
// out-of-range one still must — see bucketValue and Quota below.
type quotaReading int

const (
	quotaMissing quotaReading = iota
	quotaValid
	quotaOutOfRange
)

func bucketValue(b *usageBucketJSON) (float64, quotaReading) {
	if b == nil {
		return 0, quotaMissing
	}
	// Divided by 100 because THIS endpoint reports a PERCENTAGE, not a 0..1
	// fraction — see scale.go for the full contrast with the header path,
	// which is the opposite scale. Verified against the live endpoint rather
	// than inferred from the field names: a real response carried
	//   five_hour: {"utilization":3,...}   seven_day: {"utilization":8,...}
	// for an account nowhere near its cap, which is 3% and 8% — a 0..1 fraction
	// would have meant 300% and 800%. Review flagged this as a possible
	// double-scaling bug; it is not. Do not "fix" it without re-checking the
	// live shape first.
	for _, p := range []*float64{b.Utilization, b.UsedPercentage, b.Percent} {
		if p != nil {
			if frac, ok := normalizeUtilization(*p, percentScale); ok {
				return frac, quotaValid
			}
			return 0, quotaOutOfRange
		}
	}
	return 0, quotaMissing
}

func resetMillis(v any) int64 {
	switch t := v.(type) {
	case float64:
		return toUnixMillis(fmt.Sprintf("%.0f", t))
	case string:
		return toUnixMillis(t)
	}
	return 0
}

// Quota reads the zero-spend usage endpoint. Only OAuth credentials have one;
// an API-key account reports ErrUnsupported and is selected on priority alone.
func (a *Anthropic) Quota(ctx context.Context, c provider.Credential) (provider.Quota, error) {
	if c.Type != provider.CredentialOAuth {
		return provider.Quota{}, provider.ErrUnsupported
	}

	body, status, err := a.get(ctx, "/api/oauth/usage", c, true)
	if err != nil {
		return provider.Quota{}, fmt.Errorf("usage: %w", err)
	}
	if status == http.StatusTooManyRequests {
		return provider.Quota{}, ErrQuotaThrottled
	}
	if status != http.StatusOK {
		return provider.Quota{}, fmt.Errorf("usage: HTTP %d", status)
	}

	var ur usageResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return provider.Quota{}, fmt.Errorf("usage: %w", err)
	}

	out := provider.Quota{ObservedAt: time.Now().UnixMilli()}
	if v, reading := bucketValue(ur.FiveHour); reading != quotaMissing {
		out.Buckets = append(out.Buckets, quotaBucket("5h", v, reading, resetMillis(ur.FiveHour.ResetsAt)))
	}
	if v, reading := bucketValue(ur.SevenDay); reading != quotaMissing {
		out.Buckets = append(out.Buckets, quotaBucket("7d", v, reading, resetMillis(ur.SevenDay.ResetsAt)))
	}
	for _, l := range ur.Limits {
		if l.Group != "weekly" || l.Scope == nil || l.Scope.Model == nil || l.Percent == nil {
			continue
		}
		frac, ok := normalizeUtilization(*l.Percent, percentScale)
		reading := quotaValid
		if !ok {
			reading = quotaOutOfRange
		}
		out.Buckets = append(out.Buckets, quotaBucket(
			modelBucketName(l.Scope.Model.DisplayName), frac, reading, resetMillis(l.ResetsAt)))
	}
	return out, nil
}

// quotaBucket builds one QuotaBucket, marking it "rejected" on an
// out-of-range reading instead of trusting a fabricated utilization — the
// same fail-closed treatment parseBuckets (classify.go) gives a header
// outside 0..1. See scale.go.
func quotaBucket(name string, utilization float64, reading quotaReading, resetsAt int64) provider.QuotaBucket {
	b := provider.QuotaBucket{Name: name, Utilization: utilization, ResetsAt: resetsAt}
	if reading == quotaOutOfRange {
		b.Status = "rejected"
	}
	return b
}
