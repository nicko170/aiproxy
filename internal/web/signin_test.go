package web

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, h http.HandlerFunc, body string, tls bool) *http.Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/_aiproxy/api/v1/session", strings.NewReader(body))
	if tls {
		r.TLS = &tlsState
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w.Result()
}

func TestSignInSetsAHardenedCookie(t *testing.T) {
	s := NewSessions(time.Hour)
	res := post(t, SignInHandler(s, "secret"), `{"apiKey":"secret"}`, false)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != CookieName {
		t.Errorf("cookie name = %q, want %q", c.Name, CookieName)
	}
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly — JavaScript can read the session")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("cookie is not SameSite=Strict — a cross-site form can drive the control plane")
	}
	if c.Path != "/_aiproxy" {
		t.Errorf("cookie path = %q, want /_aiproxy", c.Path)
	}
	if !s.Valid(c.Value) {
		t.Error("the cookie's token is not a valid session")
	}
}

// Secure is set only over TLS. Setting it unconditionally would break the
// loopback HTTP case entirely — the browser would refuse to store the cookie
// and sign-in would appear to succeed and then not work.
func TestSecureAttributeFollowsTLS(t *testing.T) {
	s := NewSessions(time.Hour)
	if c := post(t, SignInHandler(s, "k"), `{"apiKey":"k"}`, false).Cookies()[0]; c.Secure {
		t.Error("Secure set on a plaintext request")
	}
	if c := post(t, SignInHandler(s, "k"), `{"apiKey":"k"}`, true).Cookies()[0]; !c.Secure {
		t.Error("Secure not set on a TLS request")
	}
}

func TestWrongKeyIsRejectedAndMintsNothing(t *testing.T) {
	s := NewSessions(time.Hour)
	res := post(t, SignInHandler(s, "secret"), `{"apiKey":"wrong"}`, false)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if len(res.Cookies()) != 0 {
		t.Error("a cookie was set for a wrong key")
	}
	if s.Count() != 0 {
		t.Error("a session was minted for a wrong key")
	}
}

// The error body must not echo what was submitted. An error that repeats the
// attempted key writes it into the browser's console and any log that captures
// response bodies.
func TestRejectionDoesNotEchoTheSubmittedKey(t *testing.T) {
	res := post(t, SignInHandler(NewSessions(time.Hour), "secret"), `{"apiKey":"hunter2"}`, false)
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	if strings.Contains(strings.ToLower(string(mustJSON(t, body))), "hunter2") {
		t.Error("the rejection echoed the submitted key")
	}
}

// With no API key configured, sign-in must refuse rather than hand out a
// session to anyone who asks. The bind guard (Task 4) stops this combination
// reaching a public interface, but defence in depth costs one branch.
func TestSignInRefusesWhenNoKeyIsConfigured(t *testing.T) {
	s := NewSessions(time.Hour)
	res := post(t, SignInHandler(s, ""), `{"apiKey":""}`, false)
	if res.StatusCode == http.StatusNoContent {
		t.Fatal("sign-in succeeded with no configured key")
	}
	if s.Count() != 0 {
		t.Error("a session was minted with no configured key")
	}
}

func TestSignOutRevokes(t *testing.T) {
	s := NewSessions(time.Hour)
	h := SignInHandler(s, "secret")
	tok := post(t, h, `{"apiKey":"secret"}`, false).Cookies()[0].Value

	r := httptest.NewRequest(http.MethodDelete, "/_aiproxy/api/v1/session", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: tok})
	w := httptest.NewRecorder()
	h(w, r)

	if s.Valid(tok) {
		t.Error("the session survived sign-out")
	}
	if c := w.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Error("sign-out did not expire the cookie in the browser")
	}
}

func TestMalformedBodyIsRejected(t *testing.T) {
	res := post(t, SignInHandler(NewSessions(time.Hour), "secret"), `not json`, false)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

var tlsState = tls.ConnectionState{}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
