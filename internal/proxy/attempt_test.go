package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeTokenEndpoint answers an OAuth refresh with a usable credential. Without
// it a forced refresh would reach the real token endpoint, so a test about what
// the proxy does after a 401 would instead be a test of internet access.
func fakeTokenEndpoint(t *testing.T) string {
	t.Helper()
	srv, _ := slowTokenEndpoint(t, 0)
	return srv
}

// slowTokenEndpoint is a token endpoint that takes delay to answer, plus a
// counter of how many refreshes it served. The count is what makes budget
// accounting for refreshes testable without depending on wall-clock precision.
func slowTokenEndpoint(t *testing.T, delay time.Duration) (string, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			case <-release:
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at2","refresh_token":"rt2","expires_in":3600}`)
	}))
	// Cleanups run last-registered-first, so handlers are released before Close
	// waits on them. r.Context() alone is not enough: a client that abandons a
	// request does not reliably cancel the server side promptly, so a deliberately
	// slow endpoint would otherwise stall teardown for its full delay.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// harness wires N accounts against one fake upstream through a real HTTP server,
// so every test measures what a client actually observes.
type harness struct {
	t        *testing.T
	mgr      *account.Manager
	upstream *testutil.FakeUpstream
	srv      *httptest.Server

	// lastRes is written by every handler goroutine, so it is guarded: the
	// concurrency hammer in concurrency_test.go drives 40 at once.
	mu      sync.Mutex
	lastRes Result
}

// last returns the most recently completed attempt's result.
func (h *harness) last() Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastRes
}

func newHarness(t *testing.T, nAccounts int, cfg RetryConfig, scripts ...testutil.Script) *harness {
	t.Helper()
	up := testutil.NewFakeUpstream(t, scripts...)
	return newHarnessAgainst(t, up, up.URL(), nAccounts, cfg, harnessTweaks{})
}

// harnessTweaks are the knobs the budget tests need: a token endpoint that is
// slow to answer, and credentials already inside the refresh threshold so the
// attempt loop actually refreshes them.
type harnessTweaks struct {
	tokenEndpoint string
	credExpiresIn time.Duration // 0 means an hour away, i.e. no refresh due
	// refreshTimeout bounds the manager's own refresh. Kept short in tests so a
	// deliberately slow token endpoint does not stall teardown.
	refreshTimeout time.Duration
}

// newHarnessAgainst wires nAccounts at an arbitrary upstream URL. up may be nil
// when the upstream is not a testutil.FakeUpstream.
func newHarnessAgainst(t *testing.T, up *testutil.FakeUpstream, upstreamURL string, nAccounts int, cfg RetryConfig, tw harnessTweaks) *harness {
	t.Helper()

	expiresIn := tw.credExpiresIn
	if expiresIn == 0 {
		expiresIn = time.Hour
	}
	accts := make([]config.Account, 0, nAccounts)
	for i := 0; i < nAccounts; i++ {
		accts = append(accts, config.Account{
			ID:       "acct-" + strconv.Itoa(i),
			Provider: "anthropic",
			Label:    "acct-" + strconv.Itoa(i),
			Priority: i,
			Upstream: upstreamURL,
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(expiresIn).UnixMilli(),
			},
		})
	}

	p := anthropic.New(http.DefaultClient)
	if tw.tokenEndpoint != "" {
		p.TokenEndpointOverride = tw.tokenEndpoint
	} else {
		p.TokenEndpointOverride = fakeTokenEndpoint(t)
	}
	providers := map[string]provider.Provider{"anthropic": p}
	mgr := account.New(accts, providers, account.Options{
		SwitchThreshold: 0.98,
		Ramp:            account.Ramp{Enabled: false},
		RefreshTimeout:  tw.refreshTimeout,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	h := &harness{t: t, mgr: mgr, upstream: up}
	at := NewAttempter(mgr, providers, NewTransport(TransportOptions{}), cfg, quietLogger())
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		res := at.Do(r.Context(), w, Request{
			Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(),
			Body: body, Model: "claude-sonnet-5", SessionID: r.Header.Get("x-session"),
		})
		h.mu.Lock()
		h.lastRes = res
		h.mu.Unlock()
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
	if h.last().AccountID != "acct-0" || h.last().Attempts != 1 {
		t.Errorf("result = %+v", h.last())
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
	// The real measurement: wall-clock dead air from request to response. This is
	// deliberately tight. The 2s of slack it used to carry is precisely why two
	// other unbounded-wait defects — a zero Retry-After spin and refresh time
	// escaping the budget — sailed through this gate.
	if elapsed > cfg.Budget+400*time.Millisecond {
		t.Fatalf("client waited %v with a %v budget — the wait is not bounded", elapsed, cfg.Budget)
	}
	// WaitMS must record the dead air that actually happened. Asserting only an
	// upper bound cannot fail: WaitMS is Budget - Remaining, so it is bounded by
	// Budget by construction. The lower bound is the falsifiable half, and it is
	// what catches the deferred accounting writing to a dead local.
	wait := h.last().WaitMS
	if wait <= 0 {
		t.Errorf("WaitMS = %d; this request spent real time backing off, so the "+
			"accounting is not reaching the caller", wait)
	}
	if wait > cfg.Budget.Milliseconds() {
		t.Errorf("WaitMS = %d, which exceeds the whole %v budget", wait, cfg.Budget)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Error("a 429 to the client must carry a Retry-After it can act on")
	}
}

// LOAD-BEARING. A persistent 429 whose Retry-After is literally "0" used to be
// an unbounded hot loop: zero classified as a real hint, so the inline
// absorption waited zero, spent zero budget, excluded nobody, and went straight
// back to the top. Measured before the fix: 75,933 upstream attempts in 3s with
// no bytes to the client.
//
// This shape is real — Anthropic's /api/oauth/usage has been observed answering
// exactly 429 with Retry-After: 0 — so it must be assumed possible anywhere.
func TestAttemptBoundsAttemptsOnZeroRetryAfter(t *testing.T) {
	cfg := RetryConfig{Budget: time.Second, InlineAbsorbMax: 5 * time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, 3, cfg, testutil.Script{
		Status: 429, Header: http.Header{"Retry-After": []string{"0"}},
	}) // repeats forever

	res, elapsed := h.post()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", res.StatusCode)
	}
	if elapsed > cfg.Budget+500*time.Millisecond {
		t.Fatalf("client waited %v with a %v budget — a zero hint is not being floored", elapsed, cfg.Budget)
	}
	// The floor, asserted on its own: a zero hint must still yield the socket for
	// a real minimum rather than retrying instantly. Without this the send cap is
	// the only thing between us and a spin.
	if elapsed < minRetryWait {
		t.Errorf("answered in %v, faster than the %v floor — a zero hint is being retried instantly",
			elapsed, minRetryWait)
	}
	// Each wait is floored at 250ms and drawn from a 1s budget, so a handful of
	// attempts is the ceiling. Single digits is what separates a bounded retry
	// from a spin.
	if n := len(h.upstream.Requests()); n >= 10 {
		t.Errorf("made %d upstream attempts; a zero retry hint is spinning", n)
	}
}

// The per-account send cap must bind on its own, independent of the budget: with
// a generous budget the wait schedule alone would keep retrying one account for
// as long as it is allowed to.
func TestAttemptCapsSendsPerAccountRegardlessOfBudget(t *testing.T) {
	const accounts = 3
	cfg := RetryConfig{Budget: 30 * time.Second, InlineAbsorbMax: 5 * time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, accounts, cfg, testutil.Script{
		Status: 429, Header: http.Header{"Retry-After": []string{"0"}},
	})

	res, elapsed := h.post()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", res.StatusCode)
	}
	// accounts * maxSendsPerAccount, and not one send more, even though the
	// budget could have paid for a hundred.
	if n := len(h.upstream.Requests()); n > accounts*maxSendsPerAccount {
		t.Errorf("made %d upstream attempts, want at most %d (%d accounts x %d sends)",
			n, accounts*maxSendsPerAccount, accounts, maxSendsPerAccount)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %v; the cap should end this well before the 30s budget", elapsed)
	}
}

// A credential refresh is dead air, and elapsed dead air must always be charged.
// Budget.Spend is all-or-nothing, so charging a refresh that outlasted the
// remaining budget deducted NOTHING: each account then paid a full refresh and
// the total grew linearly with the account count. Measured before the fix: 2.26s
// against a 700ms budget with three accounts.
//
// The token-endpoint call count is the primary assertion because it does not
// depend on wall-clock precision: an escaping refresh shows up as one call per
// account, a charged one stops as soon as the budget is gone.
func TestAttemptBoundsTotalWaitOnSlowCredentialRefreshes(t *testing.T) {
	const (
		accounts     = 6
		refreshDelay = 300 * time.Millisecond
		budget       = 400 * time.Millisecond
	)
	tokenURL, tokenCalls := slowTokenEndpoint(t, refreshDelay)

	// 403 on every account: it rotates with no backoff of its own, so the only
	// thing on the clock is the refreshes.
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 403, Body: `{"error":"nope"}`})
	h := newHarnessAgainst(t, up, up.URL(), accounts,
		RetryConfig{Budget: budget, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second},
		harnessTweaks{
			tokenEndpoint: tokenURL,
			// Inside the 5-minute refresh threshold, so every account is due.
			credExpiresIn: time.Minute,
		})

	res, elapsed := h.post()

	if res.StatusCode == http.StatusForbidden {
		t.Error("a 403 must not be relayed to the client")
	}
	if res.StatusCode < 400 {
		t.Errorf("status = %d, want a local error", res.StatusCode)
	}
	// Two refreshes fit a 400ms budget at 300ms each: the first is charged and
	// leaves 100ms, the second drains it. A refresh that escapes accounting
	// instead pays for all six.
	if got := tokenCalls(); got > 2 {
		t.Errorf("refreshed %d times, want at most 2; refresh time is escaping the budget "+
			"(unaccounted, it costs one refresh per account)", got)
	}
	if elapsed > accounts*refreshDelay/2 {
		t.Errorf("took %v with a %v budget and %v refreshes; the cost is scaling with the account count",
			elapsed, budget, refreshDelay)
	}
}

// LOAD-BEARING. A refresh must be BOUNDED by the budget, not merely charged to it
// after the fact. Charging alone still let one slow token endpoint spend far more
// than the whole allowance before the client heard anything — with the 60s client
// timeout in buildHandler, up to a minute of dead air with no bytes sent, which is
// exactly the failure class this proxy exists to remove.
func TestAttemptBoundsTheWaitForACredentialRefresh(t *testing.T) {
	const (
		accounts     = 4
		refreshDelay = 3 * time.Second        // far longer than the budget
		budget       = 300 * time.Millisecond // what the client may be made to wait
	)
	tokenURL, tokenCalls := slowTokenEndpoint(t, refreshDelay)

	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})
	h := newHarnessAgainst(t, up, up.URL(), accounts,
		RetryConfig{Budget: budget, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second},
		harnessTweaks{
			tokenEndpoint: tokenURL,
			credExpiresIn: time.Minute, // inside the refresh threshold: every account is due
			// Deliberately far above both the budget and refreshDelay, so the ONLY
			// thing that can bound this request is the caller's own cap. A short
			// RefreshTimeout would bound it instead, and the test would then pass
			// even with the caller cap removed.
			refreshTimeout: 10 * time.Second,
		})

	res, elapsed := h.post()

	// The assertion: bounded by the budget, not by the refresh. Before the fix
	// this took a full refreshDelay; the ceiling sits well below that.
	if elapsed > budget+700*time.Millisecond {
		t.Fatalf("client waited %v with a %v budget against a %v refresh — the refresh "+
			"is charged but not bounded", elapsed, budget, refreshDelay)
	}
	if res.StatusCode < 400 {
		t.Errorf("status = %d, want a local error", res.StatusCode)
	}
	// Clock-independent half: the cost must not scale with the account count.
	if got := tokenCalls(); got > 2 {
		t.Errorf("refreshed %d times for %d accounts, want at most 2", got, accounts)
	}
	// Nothing was ever sent, so this must not be recorded as a success.
	if got := h.last().Outcome; got != provider.OutcomeNoAccountReady {
		t.Errorf("Outcome = %v, want no_account_ready", got)
	}
}

// A refresh abandoned by its caller must still be running, so the next request
// finds the finished credential rather than starting a second refresh. The
// upstream rotates the refresh token on every exchange, so a duplicate refresh
// strands one of them.
func TestAttemptLeavesAnAbandonedRefreshRunningForTheNextRequest(t *testing.T) {
	const refreshDelay = 250 * time.Millisecond
	tokenURL, tokenCalls := slowTokenEndpoint(t, refreshDelay)

	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})
	// One account, and a budget too small to see the refresh through.
	h := newHarnessAgainst(t, up, up.URL(), 1,
		RetryConfig{Budget: 60 * time.Millisecond, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second},
		harnessTweaks{
			tokenEndpoint:  tokenURL,
			credExpiresIn:  time.Minute,
			refreshTimeout: 5 * time.Second,
		})

	// First request gives up waiting on the refresh.
	res, _ := h.post()
	if res.StatusCode < 400 {
		t.Errorf("first request: status = %d, want a local error", res.StatusCode)
	}

	// Give the abandoned refresh time to land, then confirm it did.
	deadline := time.Now().Add(3 * time.Second)
	for {
		acct, _ := h.mgr.Get("acct-0")
		if acct.Credential.AccessToken == "at2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh died with the request that started it; " +
				"the credential was never rotated")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := tokenCalls(); got != 1 {
		t.Fatalf("token endpoint called %d times, want 1", got)
	}

	// A later request uses the completed credential and starts no second refresh.
	res2, _ := h.post()
	if res2.StatusCode != 200 {
		t.Errorf("second request: status = %d, want 200", res2.StatusCode)
	}
	if got := tokenCalls(); got != 1 {
		t.Errorf("token endpoint called %d times, want still 1 — the completed "+
			"refresh was repeated instead of reused", got)
	}
	sent := up.Requests()
	if len(sent) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(sent))
	}
	if got := sent[0].Header.Get("Authorization"); got != "Bearer at2" {
		t.Errorf("Authorization = %q, want the credential the abandoned refresh produced", got)
	}
}

// A non-budget failure out of Admit must reach the client as a 502. Returning
// without writing lets net/http emit a clean, empty 200, which a client reads as
// a successful but empty answer rather than an error worth retrying.
//
// Driven through a recorder rather than a live server: the trigger is the caller's
// context expiring, and a real client would have disconnected before it could
// observe the response.
func TestAttemptWritesBadGatewayWhenAdmissionFailsLocally(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})
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

	// Paused well past the test, so Admit waits rather than admitting. The budget
	// is generous, so the wait ends in the caller's cancellation and not in
	// ErrBudgetExhausted — the non-budget branch is the one under test.
	mgr.PauseAccount("acct-0", 10*time.Second)

	at := NewAttempter(mgr, providers, NewTransport(TransportOptions{}),
		RetryConfig{Budget: 10 * time.Second, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second},
		quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	rec := httptest.NewRecorder()
	res := at.Do(ctx, rec, Request{
		Method: "POST", Path: "/v1/messages", Header: http.Header{},
		Body: []byte(`{"model":"claude-sonnet-5"}`), Model: "claude-sonnet-5",
	})

	if rec.Code != http.StatusBadGateway {
		t.Errorf("wrote status %d, want 502; a local admission failure must not "+
			"leave net/http to emit a clean empty 200", rec.Code)
	}
	if res.Status != http.StatusBadGateway {
		t.Errorf("Result.Status = %d, want 502", res.Status)
	}
	if res.Outcome != provider.OutcomeServerError {
		t.Errorf("Outcome = %v, want server_error", res.Outcome)
	}
	if body := rec.Body.String(); !strings.Contains(body, "proxy_error") {
		t.Errorf("body = %q, want a proxy_error payload", body)
	}
	if n := len(up.Requests()); n != 0 {
		t.Errorf("upstream saw %d requests; admission never succeeded", n)
	}
}

// A request answered without a single upstream attempt must not report the zero
// value of OutcomeKind, which reads as "ok" — a failure logged as a success, and
// one stage 2 would persist into its outcome breakdown.
func TestAttemptReportsNoAccountReadyRatherThanOK(t *testing.T) {
	// Every account's refresh is rejected, so no send ever happens.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer tokenSrv.Close()

	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})
	h := newHarnessAgainst(t, up, up.URL(), 2, defaultRetry(), harnessTweaks{
		tokenEndpoint: tokenSrv.URL,
		credExpiresIn: time.Minute, // refresh due, and it will fail
	})

	res, _ := h.post()

	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	got := h.last()
	if got.Outcome != provider.OutcomeNoAccountReady {
		t.Errorf("Outcome = %v (%d), want no_account_ready — a 429 with no attempt "+
			"must not be recorded as ok", got.Outcome, got.Outcome)
	}
	if got.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0: no send happened", got.Attempts)
	}
	if n := len(up.Requests()); n != 0 {
		t.Errorf("upstream saw %d requests, want 0", n)
	}
}

// A genuine upstream classification must survive to the log line: the
// no-account-ready default applies only when nothing classified the request.
func TestAttemptKeepsTheUpstreamOutcomeWhenTheBudgetRunsOut(t *testing.T) {
	cfg := RetryConfig{Budget: 700 * time.Millisecond, InlineAbsorbMax: 5 * time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, 3, cfg, testutil.Script{Status: 429}) // header-less, repeats

	res, _ := h.post()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", res.StatusCode)
	}
	if got := h.last().Outcome; got != provider.OutcomeThrottledNoHint {
		t.Errorf("Outcome = %v, want throttled_no_hint — the upstream's own verdict "+
			"must not be overwritten by the no-account default", got)
	}
}

// headerWithholdingUpstream accepts the request and then says nothing, so
// response headers never arrive. This is the silence the budget exists to bound;
// before the fix only the transport's 120s ResponseHeaderTimeout governed it.
func headerWithholdingUpstream(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Cleanups run last-registered-first, so the handlers are released before
	// Close waits on them. Otherwise the test's own teardown blocks on the
	// silence it is testing.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv.URL
}

// LOAD-BEARING. Waiting for response headers is dead air exactly like a backoff.
// An upstream that accepts a request and then goes quiet must not be able to hold
// the client past the budget, once per account.
func TestAttemptBoundsWaitWhenUpstreamWithholdsHeaders(t *testing.T) {
	cfg := RetryConfig{Budget: 600 * time.Millisecond, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second}
	h := newHarnessAgainst(t, nil, headerWithholdingUpstream(t), 3, cfg, harnessTweaks{})

	res, elapsed := h.post()

	if elapsed > cfg.Budget+700*time.Millisecond {
		t.Fatalf("client waited %v with a %v budget — a silent upstream is not bounded by "+
			"the budget (ResponseHeaderTimeout is 120s per attempt)", elapsed, cfg.Budget)
	}
	if res.StatusCode < 400 {
		t.Errorf("status = %d, want a local error", res.StatusCode)
	}
}

// The other half of the per-attempt deadline, and the trap in implementing it:
// the budget bounds PRE-FIRST-BYTE time only. Once headers are relayed the
// request cannot be retried and the deadline must be gone, because a real
// completion streams for minutes. If the attempt's cancel leaks into the
// response body, every answer longer than the budget is severed mid-sentence —
// which is a worse version of the defect this proxy exists to fix.
func TestAttemptBudgetDoesNotCutTheResponseBody(t *testing.T) {
	// Chunks span ~750ms in total, well past the 300ms budget. Headers arrive at
	// once, so the budget is satisfied and must then stop applying.
	cfg := RetryConfig{Budget: 300 * time.Millisecond, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second}
	h := newHarness(t, 1, cfg, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "data: one\n\n"},
			{Delay: 250 * time.Millisecond, Data: "data: two\n\n"},
			{Delay: 250 * time.Millisecond, Data: "data: three\n\n"},
			{Delay: 250 * time.Millisecond, Data: "data: four\n\n"},
		},
	})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	body, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		t.Fatalf("body read failed after %q: %v — the attempt deadline leaked into the "+
			"response body and severed a healthy stream", body, readErr)
	}
	if !strings.Contains(string(body), "data: four") {
		t.Errorf("stream truncated at %q; a completion that outlives the budget must "+
			"still be relayed in full", body)
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
	if !h.last().Rotated {
		t.Error("Rotated should be true")
	}
	if h.last().AccountID == "acct-0" {
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
	if h.last().AccountID != "acct-0" {
		t.Errorf("served by %q; a hinted throttle must retry the same account", h.last().AccountID)
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
	if h.last().AccountID != "acct-0" {
		t.Errorf("served by %q; a 401 should retry the same account after a refresh", h.last().AccountID)
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
	if h.last().Bytes == 0 {
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
