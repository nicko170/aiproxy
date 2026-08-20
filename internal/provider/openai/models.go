package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/nicko170/aiproxy/internal/provider"
)

// Models lists what this account can reach.
//
// NOT api.openai.com/v1/models: a ChatGPT OAuth token is refused there with
// "Missing scopes: api.model.read", because Codex's token carries only
// openid/profile/email/offline_access/api.connectors.*. The wham catalogue is
// the substitute and is per-account, which is what makes plan differences fall
// out for free.
func (o *OpenAI) Models(ctx context.Context, c provider.Credential) ([]provider.Model, error) {
	// The guard anthropic.Models deliberately does NOT have, and the asymmetry
	// is correct rather than an oversight: anthropic's catalogue lives on
	// /v1/models, which an API key authenticates perfectly well, while this
	// one lives on chatgpt.com/backend-api behind a ChatGPT session — an
	// OpenAI API key cannot reach it at all.
	//
	// ErrUnsupported, not an error: callers must read it as "unknown", never as
	// "none" (an empty catalogue means "serves anything" to
	// account.servesModel). internal/prober relies on this answer instead of
	// pre-judging credential types itself, which is why the decision belongs
	// here — this is the only thing that knows which credentials its endpoint
	// accepts.
	if c.Type == provider.CredentialAPIKey {
		return nil, provider.ErrUnsupported
	}
	u := o.chatgptBase() + "/wham/models?client_version=" + url.QueryEscape(o.clientVersion())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	o.Authorize(req, c)
	req.Header.Set("Accept", "application/json")

	res, err := o.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: models: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<21))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: models: HTTP %d", res.StatusCode)
	}
	var mr struct {
		Models []struct {
			Slug          string `json:"slug"`
			DisplayName   string `json:"display_name"`
			ContextWindow int    `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("openai: models: %w", err)
	}
	out := make([]provider.Model, 0, len(mr.Models))
	for _, m := range mr.Models {
		out = append(out, provider.Model{ID: m.Slug, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow})
	}
	return out, nil
}
