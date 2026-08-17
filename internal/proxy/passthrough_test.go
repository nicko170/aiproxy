package proxy

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/testutil"
)

// Some upstream paths are bound to the CLIENT's own paired identity. Injecting a
// rotated account token there is refused upstream, and the client silently loses
// the feature. These must relay transparently: client headers intact, no account
// selection, no body buffering.
func TestPassthroughForwardsClientCredentialUntouched(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})

	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Upstream = up.URL()
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 500, Body: `should not be used`})

	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/code/sessions", nil)
	req.Header.Set("Authorization", "Bearer client-own-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	recs := up.Requests()
	if len(recs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(recs))
	}
	if got := recs[0].Header.Get("Authorization"); got != "Bearer client-own-token" {
		t.Errorf("Authorization = %q; the client's own credential must survive", got)
	}
	if recs[0].Path != "/v1/code/sessions" {
		t.Errorf("path = %q", recs[0].Path)
	}
}

// The passthrough carries the client's OWN credential, which is its entire
// purpose — but x-api-key is not that credential. It authenticates the client to
// US, and the obvious client setup (ANTHROPIC_API_KEY set to the aiproxy key)
// therefore shipped the user's proxy key to api.anthropic.com on every /v1/code/
// and /api/oauth/files/ request. The account path has always stripped it; this
// path did not.
func TestPassthroughStripsTheProxyKeyButKeepsAuthorization(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})

	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Upstream = up.URL()
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 500, Body: `should not be used`})

	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/code/sessions", nil)
	req.Header.Set("x-api-key", "the-users-aiproxy-key")
	req.Header.Set("Authorization", "Bearer client-own-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	recs := up.Requests()
	if len(recs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(recs))
	}
	if got := recs[0].Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key = %q reached upstream; the client's proxy key "+
			"authenticates it to us and must never leave this process", got)
	}
	// The other half: stripping must not go so far as to break the feature.
	if got := recs[0].Header.Get("Authorization"); got != "Bearer client-own-token" {
		t.Errorf("Authorization = %q; the client's own credential is the whole point "+
			"of the passthrough and must survive", got)
	}
}

// Prefix scoping, in both directions. This needs Upstream set: without it
// NewRouter registers no passthrough routes at all, so the test passed vacuously
// and proved nothing about which paths the passthrough claims.
func TestPassthroughDoesNotClaimOrdinaryPaths(t *testing.T) {
	ptUp := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"passthrough":true}`})

	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Upstream = ptUp.URL()
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 200, Body: `{"account":true}`})

	// An ordinary inference path must take the account path.
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "account") {
		t.Errorf("body = %s; /v1/messages was answered by the passthrough upstream", body)
	}
	if r := h.lastResult(t); r.AccountID != "acct-0" {
		t.Errorf("result = %+v; /v1/messages must use account selection", r)
	}
	if n := len(ptUp.Requests()); n != 0 {
		t.Errorf("passthrough upstream saw %d requests; it must not claim /v1/messages", n)
	}
	if n := len(h.up.Requests()); n != 1 {
		t.Errorf("account upstream saw %d requests, want 1", n)
	}

	// A passthrough prefix, by contrast, must NOT take the account path: no
	// account is selected and no attempt result is recorded.
	h.mu.Lock()
	before := len(h.results)
	h.mu.Unlock()

	res2, err := http.Get(h.srv.URL + "/v1/code/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(res2.Body)
	res2.Body.Close()

	if !strings.Contains(string(body2), "passthrough") {
		t.Errorf("body = %s; /v1/code/ was not answered by the passthrough upstream", body2)
	}
	if n := len(ptUp.Requests()); n != 1 {
		t.Errorf("passthrough upstream saw %d requests, want 1", n)
	}
	if n := len(h.up.Requests()); n != 1 {
		t.Errorf("account upstream saw %d requests; a passthrough path must not select an account", n)
	}
	h.mu.Lock()
	after := len(h.results)
	h.mu.Unlock()
	if after != before {
		t.Errorf("%d attempt results recorded for a passthrough path; it must not run the attempt loop",
			after-before)
	}
}

// The passthrough must treat a truncated stream exactly as the account path does.
// Discarding Relay's error finishes the chunked body cleanly, and a client cannot
// tell that from a complete response — the opposite of the behaviour the account
// path is careful to get right.
func TestPassthroughDoesNotEndATruncatedStreamCleanly(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Upstream = truncatingUpstream(t)
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Get(h.srv.URL + "/v1/code/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (upstream headers were already relayed)", res.StatusCode)
	}
	body, readErr := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "content_block_delta") {
		t.Errorf("bytes received before the break should still be relayed: %q", body)
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		t.Fatalf("read ended with %v; a truncated passthrough stream must not look like a clean finish", readErr)
	}
}

func TestPassthroughReturns502WhenUpstreamUnreachable(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Upstream = "http://127.0.0.1:1" // nothing listens here
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 200})

	res, err := http.Get(h.srv.URL + "/v1/code/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
}
