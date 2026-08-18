package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func TestIsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:5000", "[::1]:5000", "::1", "127.0.0.1"} {
		if !IsLoopback(addr) {
			t.Errorf("IsLoopback(%q) = false, want true", addr)
		}
	}
	for _, addr := range []string{"10.0.0.4:5000", "203.0.113.7:443", ""} {
		if IsLoopback(addr) {
			t.Errorf("IsLoopback(%q) = true, want false", addr)
		}
	}
}

func TestAuthorized(t *testing.T) {
	cases := []struct {
		name                       string
		remote, presented, config_ string
		want                       bool
	}{
		{"no key configured allows anyone", "203.0.113.7:1", "", "", true},
		{"loopback is exempt", "127.0.0.1:1", "", "secret", true},
		{"remote with correct key", "203.0.113.7:1", "secret", "secret", true},
		{"remote with wrong key", "203.0.113.7:1", "nope", "secret", false},
		{"remote with no key", "203.0.113.7:1", "", "secret", false},
		{"wrong length key", "203.0.113.7:1", "s", "secret", false},
	}
	for _, c := range cases {
		if got := Authorized(c.remote, c.presented, c.config_); got != c.want {
			t.Errorf("%s: Authorized(%q,%q,%q) = %v, want %v",
				c.name, c.remote, c.presented, c.config_, got, c.want)
		}
	}
}

func TestParseModel(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"model":"claude-sonnet-5","messages":[]}`, "claude-sonnet-5"},
		{`{"messages":[{"role":"user","content":"model: fake"}],"model":"claude-opus-5"}`, "claude-opus-5"},
		{`{"messages":[]}`, ""},
		{`not json`, ""},
		{``, ""},
		{`{"model":123}`, ""},
	}
	for _, c := range cases {
		if got := ParseModel([]byte(c.body)); got != c.want {
			t.Errorf("ParseModel(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestModelMatches(t *testing.T) {
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"*fable*", "claude-fable-5", true},
		{"*fable*", "claude-sonnet-5", false},
		{"claude-opus-5", "claude-opus-5", true},
		{"claude-opus-5", "claude-opus-4", false},
		{"claude-*", "claude-haiku-4-5", true},
	}
	for _, c := range cases {
		if got := ModelMatches(c.pattern, c.model); got != c.want {
			t.Errorf("ModelMatches(%q,%q) = %v, want %v", c.pattern, c.model, got, c.want)
		}
	}
}

// routerHarness builds the full router over a fake upstream.
type routerHarness struct {
	srv     *httptest.Server
	up      *testutil.FakeUpstream
	mu      sync.Mutex
	results []Result
}

func newRouterHarness(t *testing.T, opts func(*HandlerOptions), scripts ...testutil.Script) *routerHarness {
	t.Helper()
	up := testutil.NewFakeUpstream(t, scripts...)
	p := anthropic.New(http.DefaultClient)
	p.TokenEndpointOverride = fakeTokenEndpoint(t)
	providers := map[string]provider.Provider{"anthropic": p}

	mgr := account.New([]config.Account{{
		ID: "acct-0", Provider: "anthropic", Label: "acct-0", Upstream: up.URL(),
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}}, providers, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	h := &routerHarness{up: up}
	ho := HandlerOptions{
		Attempter: NewAttempter(mgr, providers, NewTransport(TransportOptions{}), defaultRetry(), quietLogger()),
		Manager:   mgr,
		Log:       quietLogger(),
		OnResult: func(_ Request, r Result) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.results = append(h.results, r)
		},
	}
	if opts != nil {
		opts(&ho)
	}
	h.srv = httptest.NewServer(NewRouter(ho))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *routerHarness) lastResult(t *testing.T) Result {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.results) == 0 {
		t.Fatal("no result recorded")
	}
	return h.results[len(h.results)-1]
}

func TestRouterProxiesAndReportsResult(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{"ok":true}`})

	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	req.Header.Set("x-claude-code-session-id", "sess-9")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := h.up.Requests()[0].Path; got != "/v1/messages" {
		t.Errorf("upstream path = %q", got)
	}
	r := h.lastResult(t)
	if r.AccountID != "acct-0" || r.Status != 200 {
		t.Errorf("result = %+v", r)
	}
}

// A blocked model is refused locally. A model no account can serve otherwise
// burns a rotation cycle and comes back as a rate limit, which reads to the
// client as a transient problem worth retrying — it is not.
func TestRouterRejectsBlockedModelWithoutCallingUpstream(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.BlockedModels = []string{"*fable*"}
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-fable-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("a blocked model must not reach upstream")
	}
	if !strings.Contains(string(body), "blocked") {
		t.Errorf("body should explain the block: %s", body)
	}
}

func TestRouterRejectsUnauthorizedRemoteCaller(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.APIKey = "secret"
	}, testutil.Script{Status: 200, Body: `{}`})

	// The test client is loopback and therefore exempt, so assert the gate
	// directly for the remote case and confirm loopback still passes end to end.
	if Authorized("203.0.113.9:1234", "", "secret") {
		t.Error("a remote caller with no key must be refused")
	}
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		t.Error("loopback should be exempt from the proxy key gate")
	}
}

func TestRouterServesStatusUnderReservedPrefix(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}

	var got struct {
		Accounts []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Status string `json:"status"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].ID != "acct-0" {
		t.Errorf("accounts = %+v", got.Accounts)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("a control-plane path must never be proxied upstream")
	}
}

// Spec §7.3: a dropped accounting sample must be visible, not silent. The
// status endpoint is where it surfaces until the stage-4 TUI takes over.
func TestRouterStatusReportsMetricsDropped(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Dropped = func() int64 { return 42 }
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	var got struct {
		MetricsDropped int64 `json:"metricsDropped"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MetricsDropped != 42 {
		t.Errorf("metricsDropped = %d, want 42", got.MetricsDropped)
	}
}

// The reserved prefix must never reach the proxy path, even for an unknown
// route, or a future control endpoint would be silently forwarded to the
// upstream and answered by it.
func TestRouterDoesNotProxyUnknownReservedPaths(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/not-a-route")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
	if len(h.up.Requests()) != 0 {
		t.Error("an unknown reserved path must not be proxied")
	}
}

// A wrong METHOD on a KNOWN control path must also terminate locally. chi
// propagates a parent router's MethodNotAllowed handler into an already-mounted
// subrouter that has none of its own, so without an explicit cp.MethodNotAllowed
// the reserved subtree falls through to the proxy catch-all and the control path
// is forwarded upstream with an account credential injected. The unknown-path
// test above does not catch that: an unknown path is a NotFound, not a
// MethodNotAllowed.
func TestRouterDoesNotProxyWrongMethodsOnReservedPaths(t *testing.T) {
	const path = ReservedPrefix + "/api/v1/status"
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

		req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(`{}`))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()

		if res.StatusCode < 400 || res.StatusCode > 499 {
			t.Errorf("%s %s: status = %d, want a local 4xx", method, path, res.StatusCode)
		}
		if n := len(h.up.Requests()); n != 0 {
			t.Errorf("%s %s: %d upstream requests; a reserved path must never be proxied",
				method, path, n)
		}
	}
}

// A reserved path that chi's router cannot see must still never be proxied.
//
// chi matches on r.URL.RawPath whenever it is set, so any spelling of the prefix
// that survives escaping unchanged misses the mounted subrouter entirely, falls
// through to r.NotFound — the proxy catch-all — and is forwarded to the provider
// WITH AN ACCOUNT CREDENTIAL ATTACHED. Verified before the fix: the fake upstream
// received GET /_aiproxy/api/v1/status carrying Bearer at.
//
// The guard therefore lives in proxyHandler, at the point of harm, rather than in
// the router: there is no third spelling to discover because nothing that reaches
// the proxy path can be under the reserved prefix, however it was written.
func TestRouterDoesNotProxyEscapedReservedPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		// %5F is "_": the prefix decodes correctly but RawPath differs, so the
		// mounted subrouter never matches.
		{"percent-encoded prefix", "/%5Faiproxy/api/v1/status"},
		{"percent-encoded prefix, unknown route", "/%5Faiproxy/api/v1/not-a-route"},
		// %2F is "/": r.URL.Path normalizes back into the reserved namespace while
		// RawPath keeps chi looking at an ordinary-looking /v1 path.
		{"traversal into the prefix", "/v1/..%2F_aiproxy/api/v1/status"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

			res, err := http.Get(h.srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()

			if res.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", res.StatusCode)
			}
			// The assertion that matters. A reserved path reaching the provider
			// hands it one of our account credentials.
			if n := len(h.up.Requests()); n != 0 {
				t.Errorf("upstream saw %d requests for %q; a reserved path must never "+
					"be proxied, however it is spelled", n, c.path)
			}
		})
	}
}

// The guard above must not shadow the control plane it protects: the ordinary
// spellings still have to be served locally.
func TestRouterStillServesUnescapedReservedPaths(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200; the reserved-prefix guard swallowed a real "+
			"control endpoint", res.StatusCode)
	}
	if n := len(h.up.Requests()); n != 0 {
		t.Errorf("upstream saw %d requests", n)
	}
}

// The relay aborts a truncated stream by panicking http.ErrAbortHandler. That
// must survive the router's recovery middleware: if Recoverer swallowed it, the
// chunked body would be finished cleanly and the client would accept a partial
// answer as complete instead of retrying.
func TestRouterDoesNotEndATruncatedStreamCleanly(t *testing.T) {
	up := truncatingUpstream(t)

	p := anthropic.New(http.DefaultClient)
	p.TokenEndpointOverride = fakeTokenEndpoint(t)
	providers := map[string]provider.Provider{"anthropic": p}
	mgr := account.New([]config.Account{{
		ID: "acct-0", Provider: "anthropic", Label: "acct-0", Upstream: up,
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}}, providers, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	srv := httptest.NewServer(NewRouter(HandlerOptions{
		Attempter: NewAttempter(mgr, providers, NewTransport(TransportOptions{}), defaultRetry(), quietLogger()),
		Manager:   mgr,
		Log:       quietLogger(),
	}))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
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
		t.Fatalf("read ended with %v; the router turned a truncated stream into a clean finish", readErr)
	}
}
