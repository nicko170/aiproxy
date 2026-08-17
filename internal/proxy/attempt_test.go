package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeTokenEndpoint answers an OAuth refresh with a usable credential. Without
// it a forced refresh would reach the real token endpoint, so a test about what
// the proxy does after a 401 would instead be a test of internet access.
func fakeTokenEndpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at2","refresh_token":"rt2","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// harness wires N accounts against one fake upstream through a real HTTP server,
// so every test measures what a client actually observes.
type harness struct {
	t        *testing.T
	mgr      *account.Manager
	upstream *testutil.FakeUpstream
	srv      *httptest.Server
	lastRes  Result
}

func newHarness(t *testing.T, nAccounts int, cfg RetryConfig, scripts ...testutil.Script) *harness {
	t.Helper()
	up := testutil.NewFakeUpstream(t, scripts...)

	accts := make([]config.Account, 0, nAccounts)
	for i := 0; i < nAccounts; i++ {
		accts = append(accts, config.Account{
			ID:       "acct-" + strconv.Itoa(i),
			Provider: "anthropic",
			Label:    "acct-" + strconv.Itoa(i),
			Priority: i,
			Upstream: up.URL(),
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		})
	}

	p := anthropic.New(http.DefaultClient)
	p.TokenEndpointOverride = fakeTokenEndpoint(t)
	providers := map[string]provider.Provider{"anthropic": p}
	mgr := account.New(accts, providers, account.Options{
		SwitchThreshold: 0.98,
		Ramp:            account.Ramp{Enabled: false},
		Persist:         func(string, provider.Credential) error { return nil },
	})

	h := &harness{t: t, mgr: mgr, upstream: up}
	at := NewAttempter(mgr, providers, NewTransport(TransportOptions{}), cfg, quietLogger())
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.lastRes = at.Do(r.Context(), w, Request{
			Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(),
			Body: body, Model: "claude-sonnet-5", SessionID: r.Header.Get("x-session"),
		})
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) post() (*http.Response, time.Duration) {
	h.t.Helper()
	start := time.Now()
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	if err != nil {
		h.t.Fatalf("POST: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res, time.Since(start)
}

func defaultRetry() RetryConfig {
	return RetryConfig{Budget: 2 * time.Second, InlineAbsorbMax: 500 * time.Millisecond, BodyIdle: 5 * time.Second}
}

func TestAttemptRelaysASuccessfulResponse(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   `{"ok":true}`,
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if h.lastRes.AccountID != "acct-0" || h.lastRes.Attempts != 1 {
		t.Errorf("result = %+v", h.lastRes)
	}
	if h.upstream.Requests()[0].Header.Get("Authorization") != "Bearer at" {
		t.Error("credential was not injected")
	}
}

// LOAD-BEARING. Every account answers 429 with no Retry-After and no ratelimit
// headers — the exact shape observed in production. The client must be answered
// promptly, bounded by the configured budget.
//
// The defect this pins down: defaulting a missing Retry-After to 60s and
// absorbing it inline, once per account, converts a sub-second upstream
// rejection into minutes of silence with no bytes sent.
func TestAttemptBoundsTotalWaitOnHeaderlessRateLimits(t *testing.T) {
	cfg := RetryConfig{Budget: 700 * time.Millisecond, InlineAbsorbMax: 5 * time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, 3, cfg, testutil.Script{Status: 429}) // repeats forever

	res, elapsed := h.post()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", res.StatusCode)
	}
	// Generous ceiling: the point is that it is bounded at all, and nowhere near
	// the minutes a fabricated per-account delay would produce.
	if elapsed > cfg.Budget+2*time.Second {
		t.Fatalf("client waited %v with a %v budget — the wait is not bounded", elapsed, cfg.Budget)
	}
	if h.lastRes.WaitMS > cfg.Budget.Milliseconds()+250 {
		t.Errorf("recorded wait %dms exceeds the %v budget", h.lastRes.WaitMS, cfg.Budget)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Error("a 429 to the client must carry a Retry-After it can act on")
	}
}

// A header-less 429 says nothing about whether waiting helps, so the request
// must try other accounts rather than spending its whole budget on one.
func TestAttemptRotatesOnHeaderlessRateLimit(t *testing.T) {
	h := newHarness(t, 3, defaultRetry(),
		testutil.Script{Status: 429},
		testutil.Script{Status: 429},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after rotating to a healthy account", res.StatusCode)
	}
	if !h.lastRes.Rotated {
		t.Error("Rotated should be true")
	}
	if h.lastRes.AccountID == "acct-0" {
		t.Error("the third attempt should be on a different account")
	}
	if n := len(h.upstream.Requests()); n != 3 {
		t.Errorf("made %d upstream attempts, want 3", n)
	}
}

func TestAttemptRotatesAndHoldsOnQuotaRejection(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	h := newHarness(t, 2, defaultRetry(),
		testutil.Script{Status: 429, Header: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status": []string{"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Reset":  []string{reset},
		}},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, elapsed := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// A spent bucket cannot be waited out, so rotation must be immediate.
	if elapsed > time.Second {
		t.Errorf("took %v; a quota rejection should rotate without waiting", elapsed)
	}
	got, err := h.mgr.Select(account.SelectRequest{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Select after rejection: %v", err)
	}
	if got.ID == "acct-0" {
		t.Error("the rejected account should be held out of selection")
	}
}

// A 429 that states a short duration is worth absorbing on the same account:
// rotating would move the burst and discard the warm upstream cache.
func TestAttemptAbsorbsShortHintOnTheSameAccount(t *testing.T) {
	cfg := RetryConfig{Budget: 3 * time.Second, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, 2, cfg,
		testutil.Script{Status: 429, Header: http.Header{"Retry-After": []string{"0"}}},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if h.lastRes.AccountID != "acct-0" {
		t.Errorf("served by %q; a hinted throttle must retry the same account", h.lastRes.AccountID)
	}
}

// A hint longer than the inline cap is surfaced rather than slept on, and the
// client is handed the upstream's own number.
func TestAttemptSurfacesLongHintImmediately(t *testing.T) {
	cfg := RetryConfig{Budget: 5 * time.Second, InlineAbsorbMax: 500 * time.Millisecond, BodyIdle: 5 * time.Second}
	h := newHarness(t, 1, cfg, testutil.Script{
		Status: 429, Header: http.Header{"Retry-After": []string{"90"}},
	})

	res, elapsed := h.post()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	if elapsed > time.Second {
		t.Errorf("took %v; a hint over the inline cap must not be slept on", elapsed)
	}
	if got := res.Header.Get("Retry-After"); got != "90" {
		t.Errorf("Retry-After = %q, want the upstream's 90", got)
	}
}

func TestAttemptForcesOneRefreshOn401ThenRetriesSameAccount(t *testing.T) {
	h := newHarness(t, 2, defaultRetry(),
		testutil.Script{Status: 401, Body: `{"error":"expired"}`},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if h.lastRes.AccountID != "acct-0" {
		t.Errorf("served by %q; a 401 should retry the same account after a refresh", h.lastRes.AccountID)
	}
}

// A 403 is the upstream refusing THIS account. Relaying it would make the client
// discard its own unrelated session, so it never reaches the client.
func TestAttemptReportsProxyErrorWhenEveryAccountIsRefused(t *testing.T) {
	h := newHarness(t, 2, defaultRetry(), testutil.Script{Status: 403, Body: `{"error":"not allowed"}`})

	res, _ := h.post()
	if res.StatusCode == http.StatusForbidden {
		t.Fatal("a 403 must not be relayed to the client")
	}
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when every account is refused", res.StatusCode)
	}
	if n := len(h.upstream.Requests()); n != 2 {
		t.Errorf("made %d attempts, want one per account", n)
	}
}

func TestAttemptStreamsSSEThrough(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"},
			{Delay: 30 * time.Millisecond, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"},
		},
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if h.lastRes.Bytes == 0 {
		t.Error("no bytes recorded for a streamed response")
	}
}

func TestAttemptDoesNotForwardHopByHopOrClientAPIKey(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-api-key", "client-proxy-key")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("anthropic-version", "2023-06-01")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	sent := h.upstream.Requests()[0].Header
	if sent.Get("x-api-key") == "client-proxy-key" {
		t.Error("the client's proxy key must never be forwarded upstream")
	}
	if sent.Get("Connection") != "" {
		t.Error("hop-by-hop headers must be stripped")
	}
	if sent.Get("anthropic-version") != "2023-06-01" {
		t.Error("client API headers should pass through")
	}
}

// The credential copy taken by Select predates any rotation the pre-attempt
// refresh performs. Sending the superseded token would produce a 401 that looks
// like the upstream's fault, so the account is re-read before the attempt.
func TestAttemptSendsTheCredentialRotatedBeforeTheAttempt(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})

	p := anthropic.New(http.DefaultClient)
	p.TokenEndpointOverride = fakeTokenEndpoint(t)
	providers := map[string]provider.Provider{"anthropic": p}
	mgr := account.New([]config.Account{{
		ID: "acct-0", Provider: "anthropic", Label: "acct-0", Upstream: up.URL(),
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "stale", RefreshToken: "rt",
			// Already inside the refresh threshold, so the attempt loop renews it.
			ExpiresAt: time.Now().Add(time.Minute).UnixMilli(),
		},
	}}, providers, account.Options{
		SwitchThreshold: 0.98,
		Ramp:            account.Ramp{Enabled: false},
		Persist:         func(string, provider.Credential) error { return nil },
	})

	at := NewAttempter(mgr, providers, NewTransport(TransportOptions{}), defaultRetry(), quietLogger())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		at.Do(r.Context(), w, Request{
			Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(),
			Body: body, Model: "claude-sonnet-5",
		})
	}))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	recs := up.Requests()
	if len(recs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(recs))
	}
	if got := recs[0].Header.Get("Authorization"); got != "Bearer at2" {
		t.Errorf("Authorization = %q, want the rotated token", got)
	}
}

// truncatingUpstream writes response headers and one SSE event, then closes the
// socket without the terminating zero-length chunk. That is what a real upstream
// dropping mid-generation looks like: the read after the last good chunk fails.
func truncatingUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
				}
				io.WriteString(c, "HTTP/1.1 200 OK\r\n"+
					"Content-Type: text/event-stream\r\n"+
					"Transfer-Encoding: chunked\r\n\r\n")
				const payload = "data: {\"type\":\"content_block_delta\"}\n\n"
				fmt.Fprintf(c, "%x\r\n%s\r\n", len(payload), payload)
				// No terminating chunk: the stream just stops here.
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

func attempterServerAt(t *testing.T, upstreamURL string, cfg RetryConfig) *httptest.Server {
	t.Helper()
	p := anthropic.New(http.DefaultClient)
	p.TokenEndpointOverride = fakeTokenEndpoint(t)
	providers := map[string]provider.Provider{"anthropic": p}
	mgr := account.New([]config.Account{{
		ID: "acct-0", Provider: "anthropic", Label: "acct-0", Upstream: upstreamURL,
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}}, providers, account.Options{
		SwitchThreshold: 0.98,
		Ramp:            account.Ramp{Enabled: false},
		Persist:         func(string, provider.Credential) error { return nil },
	})
	at := NewAttempter(mgr, providers, NewTransport(TransportOptions{}), cfg, quietLogger())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		at.Do(r.Context(), w, Request{
			Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(),
			Body: body, Model: "claude-sonnet-5",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A stream that dies mid-generation must NOT reach the client as a cleanly
// finished short 200. A clean finish is indistinguishable from a complete answer,
// so the client accepts a truncated reply and does not retry. The relay aborts
// the connection instead, which the client reads as a transport failure.
func TestAttemptDoesNotEndATruncatedStreamCleanly(t *testing.T) {
	srv := attempterServerAt(t, truncatingUpstream(t), defaultRetry())

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (headers were already sent upstream)", res.StatusCode)
	}
	body, readErr := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "content_block_delta") {
		t.Errorf("the bytes received before the break should still have been relayed: %q", body)
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		t.Fatalf("read ended with %v; a truncated stream must not look like a clean finish", readErr)
	}
}
