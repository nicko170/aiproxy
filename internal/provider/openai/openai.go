// Package openai adapts ChatGPT subscription accounts to the proxy core.
//
// Inference goes to the Responses API on api.openai.com. Quota and the model
// catalogue come from chatgpt.com/backend-api, which is a different host and a
// private one — see Quota and Models for what happens when it moves.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

const (
	defaultAPIBase     = "https://api.openai.com/v1"
	defaultChatGPTBase = "https://chatgpt.com/backend-api"
	defaultIssuer      = "https://auth.openai.com"

	// clientID is Codex CLI's public OAuth client. A public client has no
	// secret; PKCE is what binds the code to this flow.
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// originator identifies the caller to the Responses API. Codex sends this
	// and the backend is known to gate on it, so it is sent verbatim.
	originator = "codex_cli_rs"

	// defaultClientVersion is required by the wham/models endpoint, which
	// rejects a request without it. Overridable because a server-side version
	// gate is a plausible way for this to break.
	defaultClientVersion = "0.147.0"
)

type OpenAI struct {
	hc *http.Client

	// TokenEndpointOverride redirects the OAuth token endpoint in tests.
	TokenEndpointOverride string
	// BaseURLOverride redirects the Responses API in tests.
	BaseURLOverride string
	// ChatGPTBaseURLOverride redirects quota and model reads in tests.
	ChatGPTBaseURLOverride string
	// ClientVersion is sent to wham/models. Empty takes defaultClientVersion.
	ClientVersion string
	// LoginTimeoutOverride redirects Login's end-to-end timeout in tests.
	LoginTimeoutOverride time.Duration
	// BindCallbackOverride replaces Login's fixed-port callback listener
	// (1455, falling back to 1457) in tests with an ephemeral one, so the
	// test suite never binds — and can never collide with — the real,
	// registered port a live Codex or aiproxy login might be using. Empty
	// takes the default fixed-port-with-fallback behaviour.
	BindCallbackOverride func() (net.Listener, int, error)
	// OnLoginSuccess mirrors anthropic.Anthropic.OnLoginSuccess: called once,
	// synchronously, when Login's code exchange and Profile lookup both
	// succeed, and before the resulting LoginResult is ever sent on Done.
	// cmd/aiproxy sets this to persist the account through config.Store and
	// add it to the live account.Manager. A nil hook (only in tests that do
	// not need persistence) means Login completes without persisting
	// anything.
	//
	// Returning an error here becomes LoginResult.Err, e.g. a config-store
	// write failure; the exchanged credential still cannot leak through that
	// error, since only the error's text (never the Credential value) is
	// used.
	OnLoginSuccess func(ctx context.Context, cred provider.Credential, profile provider.Profile) error
}

func New(hc *http.Client) *OpenAI {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &OpenAI{hc: hc}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) apiBase() string {
	if o.BaseURLOverride != "" {
		return o.BaseURLOverride
	}
	return defaultAPIBase
}

func (o *OpenAI) chatgptBase() string {
	if o.ChatGPTBaseURLOverride != "" {
		return o.ChatGPTBaseURLOverride
	}
	return defaultChatGPTBase
}

func (o *OpenAI) clientVersion() string {
	if o.ClientVersion != "" {
		return o.ClientVersion
	}
	return defaultClientVersion
}

func (o *OpenAI) Endpoint(a provider.Account) *url.URL {
	base := o.apiBase()
	if a.Upstream != "" {
		base = a.Upstream
	}
	u, err := url.Parse(base)
	if err != nil {
		u, _ = url.Parse(defaultAPIBase)
	}
	return u
}

func (o *OpenAI) Authorize(r *http.Request, c provider.Credential) {
	// Cleared, not overwritten: a client may have sent its own credentials to
	// the proxy and they must never travel upstream.
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	switch c.Type {
	case provider.CredentialAPIKey:
		r.Header.Set("Authorization", "Bearer "+c.APIKey)
	default:
		r.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}
	r.Header.Set("originator", originator)
	if c.AccountID != "" {
		r.Header.Set("chatgpt-account-id", c.AccountID)
	}
}

// RewriteBody applies the account's model map. Only the "model" key is decoded;
// everything else stays raw bytes, so a large input array is never re-encoded.
func (o *OpenAI) RewriteBody(body []byte, a provider.Account) ([]byte, error) {
	if len(a.ModelMap) == 0 || len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, nil // not a JSON object: pass through untouched
	}
	raw, ok := top["model"]
	if !ok {
		return body, nil
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return body, nil
	}
	mapped, ok := a.ModelMap[model]
	if !ok || mapped == model {
		return body, nil
	}
	next, err := json.Marshal(mapped)
	if err != nil {
		return body, nil
	}
	top["model"] = next
	return json.Marshal(top)
}

type usageEnvelope struct {
	Type     string `json:"type"`
	Response *struct {
		Usage *responsesUsage `json:"usage"`
	} `json:"response"`
}

type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u *responsesUsage) delta() *provider.UsageDelta {
	d := &provider.UsageDelta{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputDetails != nil {
		d.CacheReadTokens = u.InputDetails.CachedTokens
	}
	return d
}

// ParseUsage reads the terminal response.completed event. Usage is reported
// once, at the end, rather than accumulating per delta — so every other event
// must report !ok. Returning a zero delta for them would be indistinguishable
// from a genuinely free request.
func (o *OpenAI) ParseUsage(sseEvent []byte) (*provider.UsageDelta, bool) {
	var env usageEnvelope
	if err := json.Unmarshal(sseEvent, &env); err != nil {
		return nil, false
	}
	if env.Type != "response.completed" || env.Response == nil || env.Response.Usage == nil {
		return nil, false
	}
	return env.Response.Usage.delta(), true
}

func (o *OpenAI) ParseUsageBody(body []byte) (*provider.UsageDelta, bool) {
	var top struct {
		Usage *responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &top); err != nil || top.Usage == nil {
		return nil, false
	}
	return top.Usage.delta(), true
}
