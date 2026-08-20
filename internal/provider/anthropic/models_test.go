package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestModelsReadsTheAnthropicList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		// The live endpoint refuses without this with "anthropic-version:
		// header is required", which is exactly how every Anthropic account
		// came to report an empty catalogue.
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version header missing; /v1/models rejects the request without it")
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"data":[
		  {"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5","max_input_tokens":1000000},
		  {"type":"model","id":"claude-haiku-4-5","display_name":"Claude Haiku 4.5","max_input_tokens":200000}
		]}`)
	}))
	defer srv.Close()

	a := New(http.DefaultClient)
	a.BaseURLOverride = srv.URL
	got, err := a.Models(context.Background(), provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 2 || got[0].ID != "claude-opus-5" || got[0].ContextWindow != 1000000 {
		t.Errorf("models = %+v", got)
	}
}
