package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestModelsReadsTheWhamCatalogue(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/models" {
			t.Errorf("path = %q, want /wham/models", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"models":[
		  {"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","context_window":272000},
		  {"slug":"gpt-5.4-mini","display_name":"GPT-5.4-Mini","context_window":272000}
		]}`)
	}))
	defer srv.Close()

	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL
	got, err := o.Models(context.Background(), provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	// client_version is required: without it the endpoint answers a validation
	// error rather than a catalogue.
	if !strings.Contains(gotQuery, "client_version=") {
		t.Errorf("query = %q, want client_version", gotQuery)
	}
	if len(got) != 2 || got[0].ID != "gpt-5.6-sol" || got[0].ContextWindow != 272000 {
		t.Errorf("models = %+v", got)
	}
}
