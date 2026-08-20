// Package openai adapts ChatGPT subscription accounts to the proxy core.
//
// Inference goes to the Responses API on api.openai.com. Quota and the model
// catalogue come from chatgpt.com/backend-api, which is a different host and a
// private one — see Quota and Models for what happens when it moves.
package openai

import (
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
	// OnLoginSuccess mirrors anthropic.OnLoginSuccess: called once, before the
	// LoginResult is sent, so cmd/aiproxy can persist the account.
	OnLoginSuccess func(provider.Credential, provider.Profile)
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
