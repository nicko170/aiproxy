// Package anthropic implements provider.Provider for the Anthropic API.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/nicko170/aiproxy/internal/provider"
)

const DefaultBaseURL = "https://api.anthropic.com"

// Anthropic is the provider implementation. It holds no per-request state.
type Anthropic struct {
	hc *http.Client
	// TokenEndpointOverride redirects the OAuth token endpoint in tests.
	TokenEndpointOverride string
	// BaseURLOverride redirects profile/usage reads in tests.
	BaseURLOverride string
}

func New(hc *http.Client) *Anthropic {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Anthropic{hc: hc}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) tokenEndpoint() string {
	if a.TokenEndpointOverride != "" {
		return a.TokenEndpointOverride
	}
	return TokenEndpoint
}

func (a *Anthropic) baseURL() string {
	if a.BaseURLOverride != "" {
		return a.BaseURLOverride
	}
	return DefaultBaseURL
}

func (a *Anthropic) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	return RefreshToken(ctx, a.hc, a.tokenEndpoint(), c.RefreshToken)
}

// Endpoint is the base URL for an account: its override when set, else the
// provider default.
func (a *Anthropic) Endpoint(acct provider.Account) *url.URL {
	raw := acct.Upstream
	if raw == "" {
		raw = a.baseURL()
	}
	u, err := url.Parse(raw)
	if err != nil {
		u, _ = url.Parse(DefaultBaseURL)
	}
	return u
}

// Authorize injects exactly one credential form and never both: an OAuth bearer
// alongside an x-api-key is ambiguous to the upstream.
func (a *Anthropic) Authorize(r *http.Request, c provider.Credential) {
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	switch c.Type {
	case provider.CredentialAPIKey:
		r.Header.Set("x-api-key", c.APIKey)
	default:
		r.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}
}

func (a *Anthropic) ClassifyResponse(r *http.Response) provider.Outcome { return Classify(r) }

// RewriteBody aligns the body with the credential actually being used: the
// request's declared user id must match the account whose token is injected, and
// an account targeting a different upstream may name models differently.
//
// Only the two top-level keys that need changing are decoded. Everything else
// stays as raw bytes, so a megabyte of message history is never re-encoded and
// nested content is preserved exactly. A body that is not a JSON object, or that
// needs no change, is returned untouched.
func (a *Anthropic) RewriteBody(body []byte, acct provider.Account) ([]byte, error) {
	if acct.AccountUUID == "" && len(acct.ModelMap) == 0 {
		return body, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, nil // not a JSON object; pass through
	}

	changed := false

	if len(acct.ModelMap) > 0 {
		if raw, ok := fields["model"]; ok {
			var model string
			if json.Unmarshal(raw, &model) == nil {
				if mapped, ok := acct.ModelMap[model]; ok && mapped != model {
					enc, err := json.Marshal(mapped)
					if err != nil {
						return nil, err
					}
					fields["model"] = enc
					changed = true
				}
			}
		}
	}

	if acct.AccountUUID != "" {
		meta := map[string]json.RawMessage{}
		if raw, ok := fields["metadata"]; ok {
			if err := json.Unmarshal(raw, &meta); err != nil {
				meta = map[string]json.RawMessage{}
			}
		}
		enc, err := json.Marshal(acct.AccountUUID)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(meta["user_id"], enc) {
			meta["user_id"] = enc
			encMeta, err := json.Marshal(meta)
			if err != nil {
				return nil, err
			}
			fields["metadata"] = encMeta
			changed = true
		}
	}

	if !changed {
		return body, nil
	}
	return json.Marshal(fields)
}

type sseUsage struct {
	Type    string `json:"type"`
	Usage   *usage `json:"usage"`
	Message *struct {
		Usage *usage `json:"usage"`
	} `json:"message"`
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ParseUsage extracts token counts from one SSE event. Cache reads and cache
// writes are reported separately because under an agent workload cache reads
// dominate plain input tokens, and folding them together makes any cost figure
// derived from this wrong.
func (a *Anthropic) ParseUsage(event []byte) (*provider.UsageDelta, bool) {
	for _, line := range strings.Split(string(event), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev sseUsage
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			continue
		}
		u := ev.Usage
		if u == nil && ev.Message != nil {
			u = ev.Message.Usage
		}
		if u == nil {
			continue
		}
		return &provider.UsageDelta{
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CacheReadInputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens,
		}, true
	}
	return nil, false
}
