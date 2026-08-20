package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/nicko170/aiproxy/internal/provider"
)

// Models reads the account's model catalogue. Behind the same oauth beta
// header as Quota and Profile: without it the OAuth token is rejected the
// same way it would be for any other beta-gated endpoint.
//
// There is deliberately NO credential-type guard here, unlike openai.Models,
// which returns ErrUnsupported for an API key. /v1/models is a public, keyed
// endpoint and Authorize sends x-api-key for an API-key credential, so this
// works for both credential types; OpenAI's catalogue lives behind a ChatGPT
// session on a different host, which an API key cannot reach at all. The
// asymmetry is a real difference between the two upstreams, and internal/prober
// now depends on it: it asks every account's provider for a catalogue rather
// than skipping non-OAuth accounts itself, so an API-key-only deployment gets
// a populated /v1/models here.
func (a *Anthropic) Models(ctx context.Context, c provider.Credential) ([]provider.Model, error) {
	body, status, err := a.get(ctx, "/v1/models?limit=100", c, true)
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("models: HTTP %d", status)
	}
	var mr struct {
		Data []struct {
			ID             string `json:"id"`
			DisplayName    string `json:"display_name"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	out := make([]provider.Model, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, provider.Model{ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.MaxInputTokens})
	}
	return out, nil
}
