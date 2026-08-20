package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
)

// modelsHandler answers GET /v1/models from the union of every enabled
// account's discovered catalogue, rather than relaying upstream.
//
// The emitted object carries BOTH vendors' field names: object/owned_by for
// an OpenAI-shaped parser, type/id/display_name for an Anthropic-shaped one.
// That is a shape neither vendor documents, and it is chosen so the endpoint
// does not have to guess which client is calling — a guess that would be
// wrong for any client we have not seen.
//
// There is deliberately no "created" field, though OpenAI's own /v1/models
// carries one. provider.Model has no date to populate it from, and only
// Anthropic-sourced models could ever have one anyway, since the ChatGPT
// catalogue endpoint exposes no creation date at all — a field present for
// half the list helps no parser. Omitting it entirely is also the safer
// failure: an absent integer a client never asked about is tolerated, where
// created: 0 would assert the entry was created in 1970, which is wrong
// rather than merely missing.
func modelsHandler(o HandlerOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Same gate as the proxy and control-API handlers (handler.go,
		// control.go): unauthenticated, this is the only route in the process
		// that would answer a non-loopback caller with no key, and what it
		// answers is the union of every logged-in account's models — a leak of
		// plan entitlements and account composition to anyone who can reach
		// the port.
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "Invalid proxy API key")
			return
		}
		type entry struct {
			ID            string `json:"id"`
			Object        string `json:"object"`
			Type          string `json:"type"`
			DisplayName   string `json:"display_name"`
			OwnedBy       string `json:"owned_by"`
			ContextWindow int    `json:"context_window,omitempty"`
		}
		seen := map[string]bool{}
		out := []entry{}
		for _, a := range o.Manager.All() {
			if a.Disabled {
				continue
			}
			for _, m := range a.Models {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				name := m.DisplayName
				if name == "" {
					name = m.ID
				}
				out = append(out, entry{
					ID: m.ID, Object: "model", Type: "model",
					DisplayName: name, OwnedBy: a.Provider, ContextWindow: m.ContextWindow,
				})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
	}
}
