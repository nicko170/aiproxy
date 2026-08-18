package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// loginUpstream builds a fake upstream that answers the token exchange first,
// then the profile read — the order Login's flow makes them in — so a single
// FakeUpstream (which serves scripts strictly in request order) can stand in
// for both endpoints.
func loginUpstream(t *testing.T, accessToken, refreshToken string) *testutil.FakeUpstream {
	t.Helper()
	tokenBody, _ := json.Marshal(map[string]any{
		"access_token": accessToken, "refresh_token": refreshToken, "expires_in": 3600,
	})
	return testutil.NewFakeUpstream(t,
		testutil.Script{Status: 200, Body: string(tokenBody)},
		testutil.Script{Status: 200, Body: `{"account":{"uuid":"acct-1","email":"a@example.com",
			"display_name":"A","has_claude_max":true},"organization":{"uuid":"org-1","name":"Acme"}}`},
	)
}

// newLoginProvider builds an Anthropic pointed entirely at a fake upstream
// (never the public internet) with a short login timeout so timeout tests
// run in milliseconds rather than the real two minutes.
func newLoginProvider(t *testing.T, up *testutil.FakeUpstream) *Anthropic {
	t.Helper()
	p := New(http.DefaultClient)
	p.TokenEndpointOverride = up.URL()
	p.BaseURLOverride = up.URL()
	p.LoginTimeoutOverride = 300 * time.Millisecond
	return p
}

// sessionParams extracts the state and redirect_uri Login embedded in the
// authorize URL, so a test can act as the browser completing the loopback
// callback without ever fetching the (fake) authorize URL itself — Login
// never fetches it either; only the caller (a real browser, in production)
// does.
func sessionParams(t *testing.T, rawURL string) (state, redirectURI string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	state = q.Get("state")
	redirectURI = q.Get("redirect_uri")
	if state == "" || redirectURI == "" {
		t.Fatalf("authorize URL missing state/redirect_uri: %s", rawURL)
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize URL missing PKCE challenge: %s", rawURL)
	}
	// The verifier must never appear in the URL a browser (and any proxy log
	// of the request) will see — only its one-way S256 hash may.
	if strings.Contains(rawURL, "code_verifier") {
		t.Fatalf("authorize URL must not carry the PKCE verifier: %s", rawURL)
	}
	return state, redirectURI
}

func awaitResult(t *testing.T, done <-chan provider.LoginResult) provider.LoginResult {
	t.Helper()
	select {
	case res, ok := <-done:
		if !ok {
			t.Fatal("Done closed with no value sent")
		}
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for LoginResult")
		return provider.LoginResult{}
	}
}

// assertListenerClosed proves the loopback listener really was torn down:
// dialing the port it accepted this into ought to fail. It retries briefly
// since finish() may still be a few scheduler ticks from actually closing
// the socket by the moment Done fires... but finish() shuts the server down
// SYNCHRONOUSLY before sending on Done specifically so this is deterministic;
// the retry loop is just slack for CI jitter, not a raced assumption.
func assertListenerClosed(t *testing.T, redirectURI string) {
	t.Helper()
	u, _ := url.Parse(redirectURI)
	addr := u.Host
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return // refused/failed to connect: listener is gone, as wanted
		}
		conn.Close()
		lastErr = fmt.Errorf("dial to %s unexpectedly succeeded", addr)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("listener at %s was never closed: %v", addr, lastErr)
}

func TestLoginReturnsAWellFormedAuthorizeURLAndNeverTheVerifier(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer sess.Cancel()

	if !strings.HasPrefix(sess.URL, AuthorizeURL) {
		t.Errorf("URL = %q, want it to start with %q", sess.URL, AuthorizeURL)
	}
	sessionParams(t, sess.URL) // fails the test itself if malformed
}

func TestLoginCallbackWithCorrectStateSucceedsAndPersists(t *testing.T) {
	up := loginUpstream(t, "secret-access-xyz", "secret-refresh-xyz")
	p := newLoginProvider(t, up)

	var gotCred provider.Credential
	var gotProfile provider.Profile
	hookCalled := make(chan struct{})
	p.OnLoginSuccess = func(ctx context.Context, cred provider.Credential, profile provider.Profile) error {
		gotCred, gotProfile = cred, profile
		close(hookCalled)
		return nil
	}

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	state, redirectURI := sessionParams(t, sess.URL)

	res, err := http.Get(redirectURI + "?code=auth-code-1&state=" + state)
	if err != nil {
		t.Fatalf("simulate callback: %v", err)
	}
	res.Body.Close()

	result := awaitResult(t, sess.Done)
	if result.Err != nil {
		t.Fatalf("LoginResult.Err = %v, want nil", result.Err)
	}
	if result.Profile.Email != "a@example.com" || result.Profile.OrgName != "Acme" {
		t.Errorf("Profile = %+v", result.Profile)
	}

	<-hookCalled
	if gotCred.AccessToken != "secret-access-xyz" || gotCred.RefreshToken != "secret-refresh-xyz" {
		t.Errorf("OnLoginSuccess credential = %+v, want the exchanged tokens", gotCred)
	}
	if gotProfile.Email != "a@example.com" {
		t.Errorf("OnLoginSuccess profile = %+v", gotProfile)
	}

	assertListenerClosed(t, redirectURI)
}

// A wrong-state callback is answered with an error (400) itself, but must
// not consume the flow's one chance to complete: before the fix, tryClaim
// ran before the state check, so this request — indistinguishable from a
// stray/forged one from the flow's point of view — claimed the flow and
// finished it with ErrStateMismatch. The real callback, arriving moments
// later with the correct state, then found the flow already claimed and got
// "already complete" instead of ever succeeding. This proves the fix: a
// wrong-state request first, followed by the real one, still succeeds.
func TestLoginCallbackWithWrongStateDoesNotConsumeTheFlow(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)
	p.LoginTimeoutOverride = 5 * time.Second // long enough that only completion, not a timeout, explains the result

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	state, redirectURI := sessionParams(t, sess.URL)

	badRes, err := http.Get(redirectURI + "?code=auth-code-1&state=wrong-state")
	if err != nil {
		t.Fatalf("simulate stray callback: %v", err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != http.StatusBadRequest {
		t.Errorf("stray callback status = %d, want 400", badRes.StatusCode)
	}

	goodRes, err := http.Get(redirectURI + "?code=auth-code-1&state=" + state)
	if err != nil {
		t.Fatalf("simulate real callback: %v", err)
	}
	goodRes.Body.Close()
	if goodRes.StatusCode != http.StatusOK {
		t.Errorf("real callback status = %d, want 200", goodRes.StatusCode)
	}

	result := awaitResult(t, sess.Done)
	if result.Err != nil {
		t.Fatalf("Err = %v, want nil: the wrong-state callback must not have killed the flow", result.Err)
	}
	if result.Profile.Email != "a@example.com" {
		t.Errorf("Profile = %+v", result.Profile)
	}
	assertListenerClosed(t, redirectURI)
}

// A flow that only ever receives wrong-state callbacks (no legitimate one
// ever arrives) must still terminate on its own — bounded by the ordinary
// timeout, not left to leak the listener forever.
func TestLoginCallbackWithOnlyWrongStateEventuallyTimesOut(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, redirectURI := sessionParams(t, sess.URL)

	res, err := http.Get(redirectURI + "?code=auth-code-1&state=wrong-state")
	if err != nil {
		t.Fatalf("simulate callback: %v", err)
	}
	res.Body.Close()

	result := awaitResult(t, sess.Done)
	if !errors.Is(result.Err, ErrLoginTimedOut) {
		t.Fatalf("Err = %v, want ErrLoginTimedOut", result.Err)
	}
	if result.Profile != (provider.Profile{}) {
		t.Errorf("Profile = %+v, want zero value on a failed login", result.Profile)
	}
	assertListenerClosed(t, redirectURI)
}

func TestLoginTimesOutAndCleansUpTheListener(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, redirectURI := sessionParams(t, sess.URL)

	start := time.Now()
	result := awaitResult(t, sess.Done)
	if time.Since(start) < p.LoginTimeoutOverride {
		t.Errorf("returned before the configured timeout elapsed")
	}
	if !errors.Is(result.Err, ErrLoginTimedOut) {
		t.Fatalf("Err = %v, want ErrLoginTimedOut", result.Err)
	}
	assertListenerClosed(t, redirectURI)
}

func TestLoginCancelCleansUpTheListenerAndDeliversExactlyOneResult(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)
	p.LoginTimeoutOverride = 5 * time.Second // long enough that only Cancel could explain completion

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, redirectURI := sessionParams(t, sess.URL)

	sess.Cancel()
	result := awaitResult(t, sess.Done)
	if !errors.Is(result.Err, ErrLoginCancelled) {
		t.Fatalf("Err = %v, want ErrLoginCancelled", result.Err)
	}

	// Cancel again: must not panic (double close / double send).
	sess.Cancel()
	assertListenerClosed(t, redirectURI)
}

func TestLoginSubmitCodeAcceptsAPastedCodeWithoutTheBrowser(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	state, redirectURI := sessionParams(t, sess.URL)

	if err := sess.SubmitCode("pasted-code#" + state); err != nil {
		t.Fatalf("SubmitCode: %v", err)
	}

	result := awaitResult(t, sess.Done)
	if result.Err != nil {
		t.Fatalf("Err = %v, want nil", result.Err)
	}
	if result.Profile.Email != "a@example.com" {
		t.Errorf("Profile = %+v", result.Profile)
	}
	assertListenerClosed(t, redirectURI)
}

func TestLoginSubmitCodeWithWrongStateIsRejected(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	sessionParams(t, sess.URL)

	if err := sess.SubmitCode("pasted-code#wrong-state"); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("SubmitCode err = %v, want ErrStateMismatch", err)
	}
	result := awaitResult(t, sess.Done)
	if !errors.Is(result.Err, ErrStateMismatch) {
		t.Fatalf("Done Err = %v, want ErrStateMismatch", result.Err)
	}
}

// A second submission after the flow has already completed must be rejected
// synchronously, not silently ignored and not a second send on Done (which
// would panic on a closed channel).
func TestLoginDoubleSubmitIsRejectedNotAPanic(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	state, _ := sessionParams(t, sess.URL)

	if err := sess.SubmitCode("code-1#" + state); err != nil {
		t.Fatalf("first SubmitCode: %v", err)
	}
	awaitResult(t, sess.Done)

	if err := sess.SubmitCode("code-2#" + state); err == nil {
		t.Error("second SubmitCode after completion should return an error")
	}
}

// The single most likely place to leak a credential is exactly this flow;
// this asserts the exchanged tokens never appear in anything this package
// hands back to a caller: the authorize URL, the LoginResult (by type, it
// cannot — Profile/Err only — but stringify it anyway in case that ever
// regresses), or an error message.
func TestLoginNeverReturnsCredentialMaterial(t *testing.T) {
	const secretAccess = "sekrit-access-token-abc123"
	const secretRefresh = "sekrit-refresh-token-xyz789"
	up := loginUpstream(t, secretAccess, secretRefresh)
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	state, redirectURI := sessionParams(t, sess.URL)

	res, _ := http.Get(redirectURI + "?code=auth-code-1&state=" + state)
	if res != nil {
		res.Body.Close()
	}
	result := awaitResult(t, sess.Done)

	blob := fmt.Sprintf("%+v %s", result, sess.URL)
	for _, secret := range []string{secretAccess, secretRefresh, "auth-code-1"} {
		if strings.Contains(blob, secret) {
			t.Errorf("credential material %q leaked into %q", secret, blob)
		}
	}
}

// C1: the control API hands Login the HTTP request's own context, which is
// done the instant the begin handler returns — long before a real user has
// had a chance to finish signing in. Before the fix, awaitTimeout's
// ctx.Done() branch assumed only finish() itself (via f.cancel) could ever
// reach it and did nothing there, so this left the flow never finished: the
// listener stayed bound, its goroutines never exited, and the timeout was
// silently defeated. This proves a cancelled parent context terminates the
// flow promptly instead.
func TestLoginParentContextCancellationTerminatesTheFlow(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)
	p.LoginTimeoutOverride = 5 * time.Second // long enough that only cancellation explains a prompt result

	ctx, cancel := context.WithCancel(context.Background())
	sess, err := p.Login(ctx)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, redirectURI := sessionParams(t, sess.URL)

	start := time.Now()
	cancel()
	result := awaitResult(t, sess.Done)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("parent context cancellation took %v to terminate the flow, want promptly", elapsed)
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", result.Err)
	}
	assertListenerClosed(t, redirectURI)
}

// C2: a Cancel racing an in-flight exchange must not still create the
// account. The token endpoint deliberately stalls so the callback's
// exchange is still outstanding when Cancel runs; before the fix, exchange
// and Profile ran on context.Background() and complete() never rechecked
// whether the flow had already terminated, so the exchange (and
// OnLoginSuccess) ran to completion in the background regardless — the
// caller was told the login was cancelled, and an account showed up anyway.
func TestLoginCancelMidExchangeNeverPersistsTheAccount(t *testing.T) {
	// Two scripts, deliberately: FakeUpstream repeats its last script for
	// every request past the first, so a single delayed script would have
	// made the (unguarded) Profile call that follows exchange stall for
	// another 2s on top of exchange's own 2s — pushing the whole unguarded
	// path past this test's check window below and leaving it unable to
	// fail no matter what leaked, exactly the "test that cannot fail"
	// pattern this suite is otherwise on guard against. Only the exchange
	// delays; Profile answers immediately, so an unguarded OnLoginSuccess
	// would fire at ~2.0s — well inside the window below — while a
	// correctly cancelled flow never reaches it at all.
	up := testutil.NewFakeUpstream(t,
		testutil.Script{
			Status: 200, HeaderDelay: 2 * time.Second,
			Body: `{"access_token":"at","refresh_token":"rt","expires_in":3600}`,
		},
		testutil.Script{
			Status: 200,
			Body:   `{"account":{"uuid":"acct-1","email":"a@example.com"},"organization":{"uuid":"org-1","name":"Acme"}}`,
		},
	)
	p := newLoginProvider(t, up)
	p.LoginTimeoutOverride = 5 * time.Second

	var hookCalled atomic.Bool
	p.OnLoginSuccess = func(context.Context, provider.Credential, provider.Profile) error {
		hookCalled.Store(true)
		return nil
	}

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	state, redirectURI := sessionParams(t, sess.URL)

	res, err := http.Get(redirectURI + "?code=auth-code-1&state=" + state)
	if err != nil {
		t.Fatalf("simulate callback: %v", err)
	}
	res.Body.Close()

	// Give the exchange a moment to actually land on the fake upstream and
	// start waiting on HeaderDelay, so this exercises "cancel while the
	// exchange is genuinely in flight" rather than racing complete()'s own
	// goroutine startup.
	time.Sleep(50 * time.Millisecond)
	sess.Cancel()

	result := awaitResult(t, sess.Done)
	if !errors.Is(result.Err, ErrLoginCancelled) {
		t.Fatalf("Err = %v, want ErrLoginCancelled", result.Err)
	}

	// Give complete()'s goroutine every chance to (wrongly) let the exchange
	// run to completion and call OnLoginSuccess before checking — longer
	// than the fake upstream's 2s HeaderDelay would take if cancellation had
	// no effect on it.
	time.Sleep(2500 * time.Millisecond)
	if hookCalled.Load() {
		t.Error("OnLoginSuccess ran after Cancel raced the exchange: a cancelled login must never persist an account")
	}
}
