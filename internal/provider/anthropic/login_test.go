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

func TestLoginCallbackWithWrongStateIsAnErrorNotAWarning(t *testing.T) {
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
	if !errors.Is(result.Err, ErrStateMismatch) {
		t.Fatalf("Err = %v, want ErrStateMismatch", result.Err)
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
