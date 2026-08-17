package proxy

import (
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

func TestPassthroughDoesNotClaimOrdinaryPaths(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.PassthroughPrefixes = DefaultPassthroughPrefixes
	}, testutil.Script{Status: 200, Body: `{"ok":true}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	// It went through the account path, so a result was recorded.
	if r := h.lastResult(t); r.AccountID != "acct-0" {
		t.Errorf("result = %+v; /v1/messages must use account selection", r)
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
