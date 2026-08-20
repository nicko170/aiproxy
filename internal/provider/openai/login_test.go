package openai

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// PKCE S256 against a known vector, so a change to the encoding is caught here
// rather than as an opaque invalid_grant from the token endpoint.
func TestPkceChallengeIsS256Base64URLNoPad(t *testing.T) {
	const verifier = "abc123"
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := pkceChallenge(verifier); got != want {
		t.Errorf("challenge = %q, want %q", got, want)
	}
	if strings.Contains(pkceChallenge(verifier), "=") {
		t.Error("challenge must not be padded")
	}
}

func TestPkceVerifierIsLongEnough(t *testing.T) {
	v, err := pkceVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("verifier length %d, want 43..128 per RFC 7636", len(v))
	}
}

func TestAuthorizeURLCarriesEveryRequiredParameter(t *testing.T) {
	raw := New(nil).authorizeURL(1455, "chal", "st")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":              "code",
		"client_id":                  clientID,
		"redirect_uri":               "http://localhost:1455/auth/callback",
		"code_challenge":             "chal",
		"code_challenge_method":      "S256",
		"state":                      "st",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 originator,
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got := q.Get("scope"); !strings.Contains(got, "offline_access") {
		t.Errorf("scope = %q, want it to request offline_access", got)
	}
	if u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" {
		t.Errorf("url = %q", raw)
	}
}

// loginAccessToken is a JWT carrying the namespaced claims Profile reads, so
// a successful exchange in these tests also produces a non-empty profile —
// the same idToken() shape oauth_test.go already uses for id_token, reused
// here as the access_token (Profile reads AccessToken directly).
func loginAccessToken(t *testing.T) string {
	return idToken(t)
}

// loginUpstream builds a fake token endpoint answering Login's exchange.
// Unlike anthropic, openai's Profile never makes a network call (it decodes
// the access token's own claims), so only one scripted response is needed.
func loginUpstream(t *testing.T, accessToken, refreshToken string) *testutil.FakeUpstream {
	t.Helper()
	body := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":3600}`,
		accessToken, refreshToken)
	return testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: body})
}

// newLoginProvider builds an OpenAI pointed entirely at a fake upstream
// (never the public internet) with a short login timeout so timeout tests
// run in milliseconds rather than the real two minutes.
func newLoginProvider(t *testing.T, up *testutil.FakeUpstream) *OpenAI {
	t.Helper()
	p := New(http.DefaultClient)
	p.TokenEndpointOverride = up.URL()
	p.LoginTimeoutOverride = 300 * time.Millisecond
	return p
}

// sessionParams extracts the state and redirect_uri Login embedded in the
// authorize URL, so a test can act as the browser completing the loopback
// callback without ever fetching the (fake) authorize URL itself. Reading
// the port out of the URL, rather than assuming callbackPort, is what keeps
// these tests from depending on 1455 actually being free: Login itself falls
// back to callbackFallbackPort when it isn't, and the test simply follows
// whichever one it used.
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
// the socket by the moment Done fires — finish() shuts the server down
// SYNCHRONOUSLY before sending on Done specifically so this is deterministic;
// the retry loop is just slack for CI jitter, not a raced assumption. Proving
// this matters more here than in anthropic's suite: the port is fixed
// (1455/1457), so a listener left dangling would make every following test
// in this file fall back to the alternate port, or fail outright if both are
// held.
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
	up := loginUpstream(t, loginAccessToken(t), "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer func() {
		sess.Cancel()
		awaitResult(t, sess.Done)
	}()

	if !strings.HasPrefix(sess.URL, defaultIssuer+"/oauth/authorize") {
		t.Errorf("URL = %q, want it to start with %q", sess.URL, defaultIssuer+"/oauth/authorize")
	}
	sessionParams(t, sess.URL) // fails the test itself if malformed
}

func TestLoginCallbackWithCorrectStateSucceedsAndPersists(t *testing.T) {
	// The access token must be a JWT (Profile decodes its claims directly,
	// unlike anthropic's network-based profile lookup), so loginAccessToken
	// stands in for an opaque "secret-access-xyz"-style token here.
	up := loginUpstream(t, loginAccessToken(t), "secret-refresh-xyz")
	p := newLoginProvider(t, up)

	var gotCred provider.Credential
	var gotProfile provider.Profile
	hookCalled := make(chan struct{})
	p.OnLoginSuccess = func(cred provider.Credential, profile provider.Profile) {
		gotCred, gotProfile = cred, profile
		close(hookCalled)
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
	if result.Profile.Email != "someone@example.com" || result.Profile.Plan != "plus" {
		t.Errorf("Profile = %+v", result.Profile)
	}

	<-hookCalled
	if gotCred.RefreshToken != "secret-refresh-xyz" {
		t.Errorf("OnLoginSuccess credential = %+v, want the exchanged refresh token", gotCred)
	}
	if gotProfile.Email != "someone@example.com" {
		t.Errorf("OnLoginSuccess profile = %+v", gotProfile)
	}

	assertListenerClosed(t, redirectURI)
}

// A wrong-state callback is answered with an error (400) itself, but must
// not consume the flow's one chance to complete: a wrong-state request first,
// followed by the real one, must still succeed.
func TestLoginCallbackWithWrongStateDoesNotConsumeTheFlow(t *testing.T) {
	up := loginUpstream(t, loginAccessToken(t), "rt")
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
	if result.Profile.Email != "someone@example.com" {
		t.Errorf("Profile = %+v", result.Profile)
	}
	assertListenerClosed(t, redirectURI)
}

// A flow that only ever receives wrong-state callbacks (no legitimate one
// ever arrives) must still terminate on its own, bounded by the ordinary
// timeout rather than leaking the listener forever.
func TestLoginCallbackWithOnlyWrongStateEventuallyTimesOut(t *testing.T) {
	up := loginUpstream(t, loginAccessToken(t), "rt")
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
	up := loginUpstream(t, loginAccessToken(t), "rt")
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
	up := loginUpstream(t, loginAccessToken(t), "rt")
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
	up := loginUpstream(t, loginAccessToken(t), "rt")
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
	if result.Profile.Email != "someone@example.com" {
		t.Errorf("Profile = %+v", result.Profile)
	}
	assertListenerClosed(t, redirectURI)
}

func TestLoginSubmitCodeWithWrongStateIsRejected(t *testing.T) {
	up := loginUpstream(t, loginAccessToken(t), "rt")
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
	up := loginUpstream(t, loginAccessToken(t), "rt")
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
	const secretRefresh = "sekrit-refresh-token-xyz789"
	up := loginUpstream(t, loginAccessToken(t), secretRefresh)
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
	for _, secret := range []string{secretRefresh, "auth-code-1"} {
		if strings.Contains(blob, secret) {
			t.Errorf("credential material %q leaked into %q", secret, blob)
		}
	}
}

// The control API hands Login the HTTP request's own context, which is done
// the instant the begin handler returns — long before a real user has had a
// chance to finish signing in. awaitTimeout's ctx.Done() branch must finish
// the flow promptly rather than leaving the listener bound and its
// goroutines running forever.
func TestLoginParentContextCancellationTerminatesTheFlow(t *testing.T) {
	up := loginUpstream(t, loginAccessToken(t), "rt")
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

// A Cancel racing an in-flight exchange must not still create the account.
// The token endpoint deliberately stalls so the callback's exchange is still
// outstanding when Cancel runs; complete() must recheck f.terminated before
// calling OnLoginSuccess so a cancelled login never persists an account.
func TestLoginCancelMidExchangeNeverPersistsTheAccount(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200, HeaderDelay: 2 * time.Second,
		Body: fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt","expires_in":3600}`, loginAccessToken(t)),
	})
	p := newLoginProvider(t, up)
	p.LoginTimeoutOverride = 5 * time.Second

	var hookCalled atomic.Bool
	p.OnLoginSuccess = func(provider.Credential, provider.Profile) {
		hookCalled.Store(true)
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

// Pins the authorize URL's protocol-relevant parameters as Login actually
// produces them end to end (as opposed to authorizeURL_test.go's direct unit
// test of the helper), so a regression that stops Login from calling
// authorizeURL correctly would still be caught here.
func TestLoginAuthorizeURLPinsExactProtocolParameters(t *testing.T) {
	up := loginUpstream(t, loginAccessToken(t), "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer func() {
		sess.Cancel()
		awaitResult(t, sess.Done)
	}()

	u, err := url.Parse(sess.URL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()

	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want %q", got, "code")
	}
	if got := q.Get("client_id"); got != clientID {
		t.Errorf("client_id = %q, want %q", got, clientID)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want %q", got, "S256")
	}
	if q.Get("code_challenge") == "" {
		t.Error("missing code_challenge")
	}
	if q.Get("state") == "" {
		t.Error("missing state")
	}
	if got := q.Get("originator"); got != originator {
		t.Errorf("originator = %q, want %q", got, originator)
	}

	redirectURI := q.Get("redirect_uri")
	ru, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatalf("parse redirect_uri %q: %v", redirectURI, err)
	}
	if ru.Hostname() != "localhost" {
		t.Errorf("redirect_uri host = %q, want %q (got full redirect_uri %q)", ru.Hostname(), "localhost", redirectURI)
	}
	if ru.Path != "/auth/callback" {
		t.Errorf("redirect_uri path = %q, want /auth/callback", ru.Path)
	}
	port := ru.Port()
	if port != "1455" && port != "1457" {
		t.Errorf("redirect_uri port = %q, want 1455 or 1457", port)
	}
}

// A user copying the callback out of the browser's address bar (because the
// redirect never reached the loopback listener) will often paste the whole
// URL, not just the bare code. SubmitCode must accept that shape too, and
// still verify state when the pasted input carries one.
func TestLoginSubmitCodeAcceptsAFullPastedCallbackURL(t *testing.T) {
	up := loginUpstream(t, loginAccessToken(t), "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	state, redirectURI := sessionParams(t, sess.URL)

	pasted := redirectURI + "?code=pasted-code-1&state=" + state
	if err := sess.SubmitCode(pasted); err != nil {
		t.Fatalf("SubmitCode(%q): %v", pasted, err)
	}

	result := awaitResult(t, sess.Done)
	if result.Err != nil {
		t.Fatalf("Err = %v, want nil", result.Err)
	}
	if result.Profile.Email != "someone@example.com" {
		t.Errorf("Profile = %+v", result.Profile)
	}
	assertListenerClosed(t, redirectURI)
}

// The state-mismatch check must apply to a pasted full URL exactly as it
// does to the "code#state" shape — a forged or stale callback URL is not
// trustworthy just because it parses.
func TestLoginSubmitCodeWithFullPastedURLAndWrongStateIsRejected(t *testing.T) {
	up := loginUpstream(t, loginAccessToken(t), "rt")
	p := newLoginProvider(t, up)

	sess, err := p.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	_, redirectURI := sessionParams(t, sess.URL)

	pasted := redirectURI + "?code=pasted-code-1&state=wrong-state"
	if err := sess.SubmitCode(pasted); !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("SubmitCode(%q) err = %v, want ErrStateMismatch", pasted, err)
	}
	result := awaitResult(t, sess.Done)
	if !errors.Is(result.Err, ErrStateMismatch) {
		t.Fatalf("Done Err = %v, want ErrStateMismatch", result.Err)
	}
}
