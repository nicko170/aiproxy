package anthropic

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestEndpointUsesAccountOverride(t *testing.T) {
	p := New(http.DefaultClient)

	if got := p.Endpoint(provider.Account{}).String(); got != DefaultBaseURL {
		t.Errorf("default endpoint = %q, want %q", got, DefaultBaseURL)
	}
	got := p.Endpoint(provider.Account{Upstream: "https://api.example.com"}).String()
	if got != "https://api.example.com" {
		t.Errorf("override endpoint = %q", got)
	}
}

// An override with no scheme parses successfully as a relative URL rather
// than erroring, so it must be caught by an explicit IsAbs check and fall
// back to the default — otherwise it is silently accepted here and only
// fails, confusingly, when something later tries to issue a request against
// it.
func TestEndpointFallsBackWhenUpstreamNotAbsolute(t *testing.T) {
	p := New(http.DefaultClient)

	got := p.Endpoint(provider.Account{Upstream: "api.example.com"}).String()
	if got != DefaultBaseURL {
		t.Errorf("endpoint = %q, want fallback to %q for a non-absolute override", got, DefaultBaseURL)
	}
}

func TestAuthorizeSetsOAuthBearerOrAPIKey(t *testing.T) {
	p := New(http.DefaultClient)

	r, _ := http.NewRequest("POST", "https://example.com", nil)
	p.Authorize(r, provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})
	if got := r.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization = %q", got)
	}
	if r.Header.Get("x-api-key") != "" {
		t.Error("oauth must not set x-api-key")
	}

	r2, _ := http.NewRequest("POST", "https://example.com", nil)
	p.Authorize(r2, provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk"})
	if got := r2.Header.Get("x-api-key"); got != "sk" {
		t.Errorf("x-api-key = %q", got)
	}
	if r2.Header.Get("Authorization") != "" {
		t.Error("api key must not set Authorization")
	}
}

func TestRewriteBodyPatchesUserIDAndModel(t *testing.T) {
	p := New(http.DefaultClient)
	in := []byte(`{"model":"claude-x","max_tokens":8,"metadata":{"user_id":"old"},"messages":[{"role":"user","content":"hi"}]}`)

	out, err := p.RewriteBody(in, provider.Account{
		AccountUUID: "new-uuid",
		ModelMap:    map[string]string{"claude-x": "model-y"},
	})
	if err != nil {
		t.Fatalf("RewriteBody: %v", err)
	}

	var got struct {
		Model    string `json:"model"`
		MaxTok   int    `json:"max_tokens"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got.Model != "model-y" {
		t.Errorf("model = %q, want model-y", got.Model)
	}
	if got.Metadata.UserID != "new-uuid" {
		t.Errorf("metadata.user_id = %q, want new-uuid", got.Metadata.UserID)
	}
	if got.MaxTok != 8 || len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Errorf("unrelated fields were not preserved: %+v", got)
	}
}

func TestRewriteBodyIsIdentityWhenNothingToChange(t *testing.T) {
	p := New(http.DefaultClient)
	in := []byte(`{"model":"claude-x"}`)

	out, err := p.RewriteBody(in, provider.Account{})
	if err != nil {
		t.Fatalf("RewriteBody: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("body changed to %q; with no uuid and no model map it must pass through byte-for-byte", out)
	}
}

func TestRewriteBodyPassesThroughNonJSON(t *testing.T) {
	p := New(http.DefaultClient)
	in := []byte("not json at all")

	out, err := p.RewriteBody(in, provider.Account{AccountUUID: "u"})
	if err != nil {
		t.Fatalf("RewriteBody must not fail on a non-JSON body: %v", err)
	}
	if string(out) != string(in) {
		t.Errorf("non-JSON body must pass through unchanged, got %q", out)
	}
}

func TestParseUsageReadsMessageStartAndDelta(t *testing.T) {
	p := New(http.DefaultClient)

	start := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":11,` +
		`"cache_read_input_tokens":900,"cache_creation_input_tokens":5,"output_tokens":1}}}` + "\n")
	got, ok := p.ParseUsage(start)
	if !ok {
		t.Fatal("message_start should yield usage")
	}
	if got.InputTokens != 11 || got.CacheReadTokens != 900 || got.CacheWriteTokens != 5 {
		t.Errorf("start usage = %+v", got)
	}

	delta := []byte(`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n")
	got, ok = p.ParseUsage(delta)
	if !ok {
		t.Fatal("message_delta should yield usage")
	}
	if got.OutputTokens != 42 {
		t.Errorf("delta usage = %+v", got)
	}

	if _, ok := p.ParseUsage([]byte("event: ping\ndata: {}\n")); ok {
		t.Error("an event with no usage must report false")
	}
	if _, ok := p.ParseUsage([]byte("data: [DONE]\n")); ok {
		t.Error("a non-JSON data line must report false, not panic")
	}
}

// Usage must be accepted only from message_start and message_delta. Without
// that gate, an event of any other type that happens to carry the same usage
// object — a retry, a replay, some event we haven't accounted for — would be
// parsed too, and double-count tokens already reported.
func TestParseUsageIgnoresUsageOnOtherEventTypes(t *testing.T) {
	p := New(http.DefaultClient)

	event := []byte(`data: {"type":"content_block_delta","usage":{"output_tokens":5}}` + "\n")
	if _, ok := p.ParseUsage(event); ok {
		t.Error("usage on an event type other than message_start/message_delta must not be parsed")
	}
}
