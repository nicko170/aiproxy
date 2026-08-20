package openai

import (
	"net/http"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestNameIsOpenAI(t *testing.T) {
	if got := New(http.DefaultClient).Name(); got != "openai" {
		t.Errorf("Name() = %q, want openai", got)
	}
}

// The default base must be BARE. The proxy core appends the client's own
// request URI to whatever Endpoint returns (proxy.upstreamTarget, called from
// Attempter.send); the client already sends POST /v1/responses, so a base
// ending in /v1 makes every request /v1/v1/responses and nothing works.
//
// The equality check alone is what let the doubled base ship: it asserted the
// constant against a copy of itself, which is true no matter what the constant
// says. So the join the core performs is reproduced here as well, and a base
// carrying the version prefix fails on the second assertion even if someone
// updates the first one to match. internal/proxy's
// TestOpenAIDefaultBaseJoinsToTheResponsesPath asserts the same thing through
// the core's real function, which is the check that cannot drift.
func TestDefaultEndpointOmitsTheVersionPrefix(t *testing.T) {
	o := New(http.DefaultClient)
	base := o.Endpoint(provider.Account{}).String()
	if base != "https://api.openai.com" {
		t.Errorf("default endpoint = %q, want https://api.openai.com (bare, no /v1)", base)
	}
	// The core's join, reproduced: base + the client's full request URI.
	if got := strings.TrimSuffix(base, "/") + "/v1/responses"; got != "https://api.openai.com/v1/responses" {
		t.Errorf("client POST /v1/responses would reach %q; the base must not repeat /v1", got)
	}
}

// The account's Upstream override wins so a test (and an operator behind a
// gateway) can point one account somewhere else without touching the others.
func TestEndpointPrefersTheAccountOverride(t *testing.T) {
	o := New(http.DefaultClient)
	got := o.Endpoint(provider.Account{Upstream: "http://127.0.0.1:9"}).String()
	if got != "http://127.0.0.1:9" {
		t.Errorf("override endpoint = %q", got)
	}
}

func TestAuthorizeSetsBearerAndClearsForeignAuth(t *testing.T) {
	o := New(http.DefaultClient)
	r, _ := http.NewRequest("POST", "http://x/v1/responses", nil)
	r.Header.Set("x-api-key", "leaked-from-client")
	o.Authorize(r, provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})

	if got := r.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization = %q", got)
	}
	if r.Header.Get("x-api-key") != "" {
		t.Error("a client's own x-api-key must never reach upstream")
	}
	if got := r.Header.Get("originator"); got != "codex_cli_rs" {
		t.Errorf("originator = %q, want codex_cli_rs", got)
	}
}

// Usage arrives on the terminal response.completed SSE event, not per-delta.
//
// Fed a REAL SSE event block — the "event: <name>\ndata: {...}" pair the proxy's
// relay actually splits out of the stream and hands over — and not bare JSON.
// The earlier version of this test passed a naked JSON document, so it kept
// passing against a ParseUsage that json.Unmarshalled the frame and failed on
// every genuine event: all streamed ChatGPT accounting read zero.
func TestParseUsageReadsAFramedResponseCompletedEvent(t *testing.T) {
	o := New(http.DefaultClient)
	ev := []byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":4}}}}`)
	got, ok := o.ParseUsage(ev)
	if !ok {
		t.Fatal("ParseUsage returned !ok for a framed response.completed event")
	}
	if got.InputTokens != 11 || got.OutputTokens != 7 || got.CacheReadTokens != 4 {
		t.Errorf("usage = %+v, want in=11 out=7 cacheRead=4", got)
	}
}

// Anything that is not the terminal event carries no usage, and reporting a
// zero delta as if it were real is how token accounting silently reads free.
// Framed, for the same reason as above.
func TestParseUsageIgnoresANonTerminalEvent(t *testing.T) {
	o := New(http.DefaultClient)
	ev := []byte("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}`)
	if _, ok := o.ParseUsage(ev); ok {
		t.Error("a text delta must not report usage")
	}
	// Neither may the frame alone, nor a comment-only keep-alive block.
	if _, ok := o.ParseUsage([]byte("event: response.completed")); ok {
		t.Error("an event line with no data must not report usage")
	}
	if _, ok := o.ParseUsage([]byte(": keep-alive")); ok {
		t.Error("an SSE comment must not report usage")
	}
}

func TestParseUsageBodyReadsNonStreamingResponse(t *testing.T) {
	o := New(http.DefaultClient)
	body := []byte(`{"usage":{"input_tokens":3,"output_tokens":5}}`)
	got, ok := o.ParseUsageBody(body)
	if !ok || got.InputTokens != 3 || got.OutputTokens != 5 {
		t.Errorf("got %+v ok=%v, want in=3 out=5", got, ok)
	}
}

func TestRewriteBodyAppliesTheAccountModelMap(t *testing.T) {
	o := New(http.DefaultClient)
	in := []byte(`{"model":"proxy-fast","input":"hi"}`)
	got, err := o.RewriteBody(in, provider.Account{ModelMap: map[string]string{"proxy-fast": "gpt-5.4-mini"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"gpt-5.4-mini"`) {
		t.Errorf("body = %s, want the mapped model", got)
	}
	if !strings.Contains(string(got), `"input":"hi"`) {
		t.Errorf("body = %s, want the rest of the body preserved", got)
	}
}

func TestAuthorizeSetsTheAccountHeader(t *testing.T) {
	o := New(http.DefaultClient)
	r, _ := http.NewRequest("POST", "http://x/v1/responses", nil)
	o.Authorize(r, provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at", AccountID: "acc-1"})
	if got := r.Header.Get("chatgpt-account-id"); got != "acc-1" {
		t.Errorf("chatgpt-account-id = %q", got)
	}
}
