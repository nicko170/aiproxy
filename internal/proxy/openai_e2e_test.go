package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/openai"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// This file drives a ChatGPT account through the REAL attempt loop and the real
// relay, which is the one thing nothing else in the branch did.
//
// It exists because both criticals in the final whole-branch review lived in the
// seam between a new provider method and this untouched core, and both were
// pinned as CORRECT by package-local tests on either side of that seam:
// openai.Endpoint was asserted against its own constant and never against the
// join attempt.send performs on it, and openai.ParseUsage was fed bare JSON and
// never the SSE frame relay.teeUsage actually hands it. A provider-package test
// and a proxy-package test can both pass while the pair between them is broken;
// only a request driven end to end with an OpenAI account can see it.

// newOpenAIHarness wires one ChatGPT account against a fake upstream through the
// same Attempter every real request uses.
//
// BaseURLOverride is set to the fake server's BARE origin — no /v1 — because
// that is the convention attempt.send's join relies on, and therefore the
// convention the provider's own default has to follow. The default constant
// itself is pinned separately by TestOpenAIDefaultBaseJoinsToTheResponsesPath,
// which no override can stand in for.
func newOpenAIHarness(t *testing.T, cfg RetryConfig, scripts ...testutil.Script) *harness {
	t.Helper()
	up := testutil.NewFakeUpstream(t, scripts...)

	p := openai.New(http.DefaultClient)
	p.TokenEndpointOverride = fakeTokenEndpoint(t)
	p.BaseURLOverride = up.URL()
	providers := map[string]provider.Provider{"openai": p}

	accts := []config.Account{{
		ID:       "openai-0",
		Provider: "openai",
		Label:    "openai-0",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), AccountID: "acct_e2e",
		},
	}}
	mgr := account.New(accts, providers, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	h := &harness{t: t, mgr: mgr, upstream: up}
	at := NewAttempter(mgr, providers, NewTransport(TransportOptions{}), cfg, quietLogger())
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		res := at.Do(r.Context(), w, Request{
			Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(),
			Body: body, Model: "gpt-5-codex",
		})
		h.mu.Lock()
		h.lastRes = res
		h.mu.Unlock()
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// postResponses sends the request a Codex client actually sends: POST
// /v1/responses, streaming by default.
func (h *harness) postResponses() *http.Response {
	h.t.Helper()
	res, err := http.Post(h.srv.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5-codex","stream":true,"input":[]}`))
	if err != nil {
		h.t.Fatalf("POST /v1/responses: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res
}

// CRITICAL 1. attempt.send joins the provider's base to the CLIENT's full
// request URI, which already begins /v1. A base that also ends in /v1 sends
// every Codex request to /v1/v1/responses, which 404s — not one request can
// succeed. anthropic.DefaultBaseURL is bare for exactly this reason.
//
// Asserted through upstreamTarget, the same function attempt.send calls, against
// the provider's real default endpoint: an assertion written against the
// constant alone is what let this ship.
func TestOpenAIDefaultBaseJoinsToTheResponsesPath(t *testing.T) {
	p := openai.New(nil)
	got := upstreamTarget(p.Endpoint(provider.Account{}), "/v1/responses")
	if got != "https://api.openai.com/v1/responses" {
		t.Errorf("upstream target = %q, want https://api.openai.com/v1/responses", got)
	}
	if strings.Contains(got, "/v1/v1/") {
		t.Errorf("target %q doubles the /v1 prefix: the base must not repeat what the client path already carries", got)
	}
}

// The same join, end to end: whatever the client asked for is the path the
// upstream sees, with nothing inserted and nothing dropped.
func TestOpenAIRequestReachesTheUpstreamPathUnchanged(t *testing.T) {
	h := newOpenAIHarness(t, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   `{"id":"resp_1","usage":{"input_tokens":2,"output_tokens":3}}`,
	})

	if res := h.postResponses(); res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	seen := h.upstream.Requests()
	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	if seen[0].Path != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", seen[0].Path)
	}
	if got := seen[0].Header.Get("chatgpt-account-id"); got != "acct_e2e" {
		t.Errorf("chatgpt-account-id = %q, want acct_e2e (Authorize must run on the real path)", got)
	}
}

// CRITICAL 2. The Responses API streams by default, so this is the shape almost
// every Codex request takes. relay.teeUsage hands ParseUsage a whole SSE event
// block — "event: <name>\ndata: {...}" — and a ParseUsage that json.Unmarshals
// those bytes fails on every real event, recording zero tokens for every
// streamed request: indistinguishable from a free one.
func TestOpenAIStreamedUsageIsRecorded(t *testing.T) {
	h := newOpenAIHarness(t, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"},
			{Delay: 5 * time.Millisecond,
				Data: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"},
			{Delay: 5 * time.Millisecond,
				Data: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":" +
					"{\"input_tokens\":1200,\"output_tokens\":340,\"input_tokens_details\":{\"cached_tokens\":900}}}}\n\n"},
		},
	})

	if res := h.postResponses(); res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	got := h.last()
	if !got.Stream {
		t.Error("Stream = false for a text/event-stream response")
	}
	if got.InputTokens != 1200 || got.OutputTokens != 340 || got.CacheReadTokens != 900 {
		t.Errorf("tokens = in %d / out %d / cacheRead %d, want 1200/340/900",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens)
	}
}

// The non-streaming shape, so a fix to the streamed path cannot regress it.
func TestOpenAINonStreamingUsageIsRecorded(t *testing.T) {
	h := newOpenAIHarness(t, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{"id":"resp_1","usage":{"input_tokens":80,"output_tokens":21,
		        "input_tokens_details":{"cached_tokens":64}}}`,
	})

	h.postResponses()
	got := h.last()
	if got.InputTokens != 80 || got.OutputTokens != 21 || got.CacheReadTokens != 64 {
		t.Errorf("tokens = in %d / out %d / cacheRead %d, want 80/21/64",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens)
	}
}

// IMPORTANT 3. Result carried no provider, so cmd/aiproxy hardcoded "anthropic"
// on every metrics sample. Persisted for the whole retention window, on a
// request that never touched Anthropic.
func TestResultCarriesTheProviderThatServedTheRequest(t *testing.T) {
	h := newOpenAIHarness(t, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   `{"ok":true}`,
	})

	h.postResponses()
	if got := h.last().Provider; got != "openai" {
		t.Errorf("Result.Provider = %q, want openai", got)
	}
}
