package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nicko170/aiproxy/internal/web"
)

type stubSessions struct{ good string }

func (s stubSessions) Valid(tok string) bool { return tok != "" && tok == s.good }

func reqWith(remote, key, cookie string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/_aiproxy/api/v1/status", nil)
	r.RemoteAddr = remote
	if key != "" {
		r.Header.Set("x-api-key", key)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	return r
}

// A valid session authorizes a non-loopback caller with no API key header —
// which is the entire point: a browser cannot send x-api-key on a page load.
func TestAuthorizedRequestAcceptsAValidSession(t *testing.T) {
	o := HandlerOptions{APIKey: "secret", Sessions: stubSessions{good: "tok"}}
	if !authorizedRequest(o, reqWith("203.0.113.9:1234", "", "tok")) {
		t.Error("a valid session was rejected")
	}
}

// A forged or stale cookie must not authorize, and must not fall through to
// the loopback exemption on a remote caller.
func TestAuthorizedRequestRejectsABadSession(t *testing.T) {
	o := HandlerOptions{APIKey: "secret", Sessions: stubSessions{good: "tok"}}
	if authorizedRequest(o, reqWith("203.0.113.9:1234", "", "forged")) {
		t.Error("a forged session was accepted")
	}
	if authorizedRequest(o, reqWith("203.0.113.9:1234", "", "")) {
		t.Error("a missing session was accepted")
	}
}

// The existing paths must keep working exactly as before.
func TestAuthorizedRequestStillHonoursKeyAndLoopback(t *testing.T) {
	o := HandlerOptions{APIKey: "secret", Sessions: stubSessions{good: "tok"}}
	if !authorizedRequest(o, reqWith("203.0.113.9:1234", "secret", "")) {
		t.Error("a correct x-api-key was rejected")
	}
	if authorizedRequest(o, reqWith("203.0.113.9:1234", "wrong", "")) {
		t.Error("a wrong x-api-key was accepted")
	}
	if !authorizedRequest(o, reqWith("127.0.0.1:5555", "", "")) {
		t.Error("loopback was rejected")
	}
}

// A nil Sessions is the state of every pre-existing caller and every existing
// test. It must not panic and must not change behaviour.
func TestAuthorizedRequestToleratesNilSessions(t *testing.T) {
	o := HandlerOptions{APIKey: "secret"}
	if authorizedRequest(o, reqWith("203.0.113.9:1234", "", "anything")) {
		t.Error("a cookie authorized with no session store configured")
	}
	if !authorizedRequest(o, reqWith("127.0.0.1:5555", "", "")) {
		t.Error("loopback broke when Sessions was nil")
	}
}

// The proxy reads the cookie internal/web writes. They cannot share a constant
// without a dependency edge neither package wants, so this is what stops them
// drifting apart into a dashboard that signs in and is then refused on every
// subsequent request.
func TestSessionCookieNameMatchesTheWebPackage(t *testing.T) {
	if sessionCookie != web.CookieName {
		t.Errorf("proxy.sessionCookie = %q, web.CookieName = %q", sessionCookie, web.CookieName)
	}
}
