package openai

import (
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestNameIsOpenAI(t *testing.T) {
	if got := New(http.DefaultClient).Name(); got != "openai" {
		t.Errorf("Name() = %q, want openai", got)
	}
}

// The account's Upstream override wins so a test (and an operator behind a
// gateway) can point one account somewhere else without touching the others.
func TestEndpointPrefersTheAccountOverride(t *testing.T) {
	o := New(http.DefaultClient)
	if got := o.Endpoint(provider.Account{}).String(); got != "https://api.openai.com/v1" {
		t.Errorf("default endpoint = %q", got)
	}
	got := o.Endpoint(provider.Account{Upstream: "http://127.0.0.1:9/v1"}).String()
	if got != "http://127.0.0.1:9/v1" {
		t.Errorf("override endpoint = %q", got)
	}
}
