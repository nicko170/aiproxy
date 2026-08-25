# Web Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve a browser dashboard that fully replaces the TUI — all five screens, plus session auth and an OAuth login flow that works on a server the operator cannot SSH into casually.

**Architecture:** A single-page app served from `embed.FS`, vanilla JS and hand-rolled SVG, no build step and no CDN. It consumes the control API that stage 3 already completed, so this stage adds no new query endpoints — only an asset handler, a session endpoint, and a bind guard. `internal/web` imports `internal/view` and nothing below it, the same rule `internal/tui` follows.

**Tech Stack:** Go 1.26.5, stdlib only. Vanilla ES modules, SVG built through the DOM. `node --test` is a test-time convenience for the chart maths, never a build dependency.

**Spec:** `docs/superpowers/specs/2026-08-25-web-dashboard-design.md`

## Global Constraints

- `CGO_ENABLED=0` must keep working: no cgo, no native dependencies. One runner cross-compiles four targets, and `install.sh` and the self-updater depend on it.
- **No new third-party modules and no third-party JavaScript.** `go.mod` gains nothing; no CDN reference; no vendored chart library. The binary stays self-contained and the dashboard works offline.
- **No build step.** `CGO_ENABLED=0 go build ./cmd/aiproxy` produces a working dashboard. Anything requiring `npm` before `go build` is a plan violation.
- `gofmt`, `go vet` and `go test ./...` clean at every commit.
- `internal/web` imports `internal/view` and nothing below it — not `account`, not `metrics`, not `proxy`. Enforced by a test, the same way `TestTUIImportsOnlyTheViewSeam` enforces it for the TUI.
- Every `view.Source` method has exactly one control route, enforced by `TestEveryViewSourceMethodHasAControlRoute`. This stage adds no `view.Source` methods, so that test must stay green untouched.
- **The API key never reaches JavaScript beyond the single sign-in POST.** Not in a URL, not in `localStorage`, not in a query parameter — a key in a URL lands in access logs and `Referer` headers.
- **Charts must be readable without colour.** A stacked area distinguished only by hue is unreadable to a meaningful fraction of operators and to anyone reading a greyscale screenshot.
- **SVG is built through the DOM**, never by string concatenation. Values from the API become text nodes and attributes, never markup.
- Credentials never appear in a log line, an error string, or a control-API response.
- Session cookie name: `aiproxy_session`. Attributes: `HttpOnly`, `SameSite=Strict`, `Path=/_aiproxy`, `Secure` when the request arrived over TLS.
- Session expiry: 30 days, sliding. Sessions live in memory only; a restart logs the operator out.
- Poll intervals, tab-visible only: status and accounts 5s, usage and totals 30s, settings never (read on entry).

## File Structure

| File | Responsibility |
|---|---|
| `internal/web/session.go` | In-memory session store: mint, validate with sliding expiry, delete |
| `internal/web/web.go` | `embed.FS`, asset handler, cache headers, the `Handler` type the router mounts |
| `internal/web/signin.go` | `POST`/`DELETE /_aiproxy/api/v1/session` — API key ⇄ cookie |
| `internal/web/assets/index.html` | The shell: header, nav, one `<main>` the router fills |
| `internal/web/assets/app.js` | Hash router, fetch helper, SSE client, screen registry, error/loading states |
| `internal/web/assets/chart.js` | Scales, ticks, formatting, marks, legend, tooltip |
| `internal/web/assets/chart_test.js` | `node --test` coverage for the pure chart maths |
| `internal/web/assets/style.css` | Layout, theme, `prefers-color-scheme` |
| `internal/web/assets/screens/*.js` | One module per screen: overview, activity, usage, accounts, settings, login |
| `internal/proxy/handler.go` | `authorizedRequest` — session, then key, then loopback |
| `internal/proxy/router.go` | Mounts the web handler inside the reserved-prefix subrouter |
| `cmd/aiproxy/main.go` | Bind guard, `--insecure-no-auth`, constructs the session store |

**On `Authorized`.** The spec says it "grows a third path". The implementation keeps the exported `Authorized(remoteAddr, presentedKey, configuredKey)` exactly as it is — it is pure, already tested, and four call sites depend on it — and adds an unexported `authorizedRequest(o HandlerOptions, r *http.Request) bool` that checks the session cookie first and delegates to `Authorized` otherwise. Same behaviour the spec describes, less churn, and the pure function stays pure.

---

### Task 1: The session store

**Files:**
- Create: `internal/web/session.go`
- Create: `internal/web/session_test.go`

**Interfaces:**
- Consumes: stdlib only (`crypto/rand`, `encoding/base64`, `sync`, `time`).
- Produces:
  - `type Sessions struct` with `func NewSessions(ttl time.Duration) *Sessions`
  - `func (s *Sessions) Mint() (string, error)`
  - `func (s *Sessions) Valid(token string) bool` — slides the expiry on success
  - `func (s *Sessions) Delete(token string)`
  - `func (s *Sessions) Count() int` — for tests and for the reaper's assertions

- [ ] **Step 1: Write the failing test**

```go
package web

import (
	"testing"
	"time"
)

func TestMintedSessionIsValid(t *testing.T) {
	s := NewSessions(time.Hour)
	tok, err := s.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok == "" {
		t.Fatal("Mint returned an empty token")
	}
	if !s.Valid(tok) {
		t.Error("a freshly minted token is not valid")
	}
}

// Tokens must be unguessable and must not collide. A predictable session token
// is the whole authentication system defeated, so this asserts both randomness
// (no two mints agree) and length (enough entropy to be worth having).
func TestTokensAreUniqueAndLongEnough(t *testing.T) {
	s := NewSessions(time.Hour)
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		tok, err := s.Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if seen[tok] {
			t.Fatalf("Mint produced a duplicate token on iteration %d", i)
		}
		seen[tok] = true
		if len(tok) < 32 {
			t.Fatalf("token %q is %d chars, want at least 32", tok, len(tok))
		}
	}
}

func TestUnknownTokenIsNotValid(t *testing.T) {
	s := NewSessions(time.Hour)
	if s.Valid("not-a-token") {
		t.Error("an unminted token was accepted")
	}
	if s.Valid("") {
		t.Error("the empty token was accepted")
	}
}

func TestDeleteRevokesImmediately(t *testing.T) {
	s := NewSessions(time.Hour)
	tok, _ := s.Mint()
	s.Delete(tok)
	if s.Valid(tok) {
		t.Error("a deleted token is still valid")
	}
	if n := s.Count(); n != 0 {
		t.Errorf("Count() = %d after delete, want 0", n)
	}
}

// Expiry is driven by an injectable clock, not by sleeping: a test that sleeps
// for the real TTL cannot assert a 30-day expiry, and one that sleeps for a
// short TTL is a flake waiting for a loaded CI box.
func TestExpiredSessionIsRejectedAndReaped(t *testing.T) {
	s := NewSessions(time.Hour)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }

	tok, _ := s.Mint()
	now = now.Add(59 * time.Minute)
	if !s.Valid(tok) {
		t.Fatal("token expired early")
	}
	now = now.Add(2 * time.Hour)
	if s.Valid(tok) {
		t.Error("expired token was accepted")
	}
	if n := s.Count(); n != 0 {
		t.Errorf("Count() = %d, want the expired entry reaped", n)
	}
}

// The expiry slides on use, so an operator with the dashboard open all week is
// not logged out mid-session.
func TestValidSlidesTheExpiry(t *testing.T) {
	s := NewSessions(time.Hour)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }

	tok, _ := s.Mint()
	for i := 0; i < 10; i++ {
		now = now.Add(50 * time.Minute)
		if !s.Valid(tok) {
			t.Fatalf("token expired after %d slides despite continuous use", i)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -v`
Expected: FAIL — build error, `undefined: NewSessions`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package web serves the browser dashboard: embedded assets, the session
// exchange that lets a browser authenticate without an x-api-key header, and
// nothing else.
//
// It imports internal/view and nothing below it — the same rule internal/tui
// follows — so presentation never reaches past the seam into account, metrics
// or proxy state.
package web

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// tokenBytes is the entropy behind one session token. 32 bytes is well past
// what is guessable and costs nothing.
const tokenBytes = 32

// Sessions is an in-memory session store with a sliding expiry.
//
// In memory, deliberately: a restart logs the operator out, which is the right
// trade for a single-operator tool and avoids persisting anything
// credential-adjacent to disk. Persisting sessions would mean a file that
// grants full control-plane access sitting beside the config.
type Sessions struct {
	mu  sync.Mutex
	ttl time.Duration
	// expires maps token to the instant it dies. Valid slides the value
	// forward, so continuous use never expires.
	expires map[string]time.Time
	// now is injectable so expiry is testable without sleeping. Production
	// never reassigns it; a test that sleeps for a 30-day TTL cannot exist,
	// and one that shortens the TTL to sleep through is a flake on a loaded
	// machine.
	now func() time.Time
}

func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{ttl: ttl, expires: map[string]time.Time{}, now: time.Now}
}

// Mint creates a session and returns its token. The error is from the system
// random source and is never ignored: falling back to a weaker token would
// turn an unreadable failure into a silent one.
func (s *Sessions) Mint() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires[tok] = s.now().Add(s.ttl)
	return tok, nil
}

// Valid reports whether a token is live, and slides its expiry when it is.
// An expired entry is reaped here rather than by a background goroutine: the
// store is small, every lookup already holds the lock, and a reaper goroutine
// would be a lifecycle to own for no benefit.
func (s *Sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.expires[token]
	if !ok {
		return false
	}
	now := s.now()
	if !now.Before(exp) {
		delete(s.expires, token)
		return false
	}
	s.expires[token] = now.Add(s.ttl)
	return true
}

func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expires, token)
}

func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.expires)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/web/ -v`
Expected: PASS, six tests.

- [ ] **Step 5: Commit**

```bash
git add internal/web/
git commit -m "feat(web): in-memory session store with a sliding expiry"
```

---

### Task 2: `authorizedRequest` — session, then key, then loopback

**Files:**
- Modify: `internal/proxy/handler.go` (add `SessionValidator`, `authorizedRequest`, `HandlerOptions.Sessions`)
- Modify: `internal/proxy/control.go:26`, `internal/proxy/handler.go:152`, `internal/proxy/handler.go:308`, `internal/proxy/models.go:36` (four call sites)
- Create: `internal/proxy/session_auth_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 directly — the proxy must NOT import `internal/web`. It depends on an interface it declares itself.
- Produces:
  - `type SessionValidator interface { Valid(token string) bool }`
  - `HandlerOptions.Sessions SessionValidator` — nil means no session auth, which is every existing test's situation
  - `func authorizedRequest(o HandlerOptions, r *http.Request) bool`
  - `const sessionCookie = "aiproxy_session"`

- [ ] **Step 1: Write the failing test**

```go
package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run TestAuthorizedRequest -v`
Expected: FAIL — `undefined: authorizedRequest`, `undefined: sessionCookie`.

- [ ] **Step 3: Write minimal implementation**

In `handler.go`, beside `sessionHeader`:

```go
// sessionCookie carries the browser's dashboard session. A cookie rather than
// a header because a browser cannot attach an x-api-key header to a top-level
// navigation, and rather than a query parameter because a credential in a URL
// lands in access logs and Referer headers.
const sessionCookie = "aiproxy_session"

// SessionValidator is the one thing the proxy needs from the dashboard's
// session store.
//
// Declared HERE, as an interface, rather than importing internal/web: the
// dependency runs router -> web, and reversing it for an auth check would make
// the two packages mutually dependent. A nil SessionValidator means no session
// auth is configured, which is the state of every caller that predates the
// dashboard.
type SessionValidator interface {
	Valid(token string) bool
}
```

Add to `HandlerOptions`:

```go
	// Sessions authorizes browser dashboard requests. Nil disables the session
	// path entirely, leaving key-and-loopback behaviour byte for byte as it was.
	Sessions SessionValidator
```

And the wrapper:

```go
// authorizedRequest gates a request: a valid dashboard session, then a matching
// API key, then the loopback exemption.
//
// Session first because it is the cheapest positive answer for the dashboard,
// which is the only caller that has one. The pure Authorized below is left
// exactly as it was — four call sites depend on it and it is separately tested.
func authorizedRequest(o HandlerOptions, r *http.Request) bool {
	if o.Sessions != nil {
		if c, err := r.Cookie(sessionCookie); err == nil && o.Sessions.Valid(c.Value) {
			return true
		}
	}
	return Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey)
}
```

Then replace all four call sites. Each currently reads:

```go
		if !Authorized(r.RemoteAddr, r.Header.Get("x-api-key"), o.APIKey) {
```

and becomes:

```go
		if !authorizedRequest(o, r) {
```

The four are `internal/proxy/control.go:26`, `internal/proxy/handler.go:152`, `internal/proxy/handler.go:308`, and `internal/proxy/models.go:36`. Miss one and that surface silently refuses every browser request while the others work — a confusing partial failure that reads as "the dashboard mostly works", so verify coverage by grep afterwards.

The grep will match **exactly one** line: `authorizedRequest`'s own delegation to `Authorized`. That is the correct end state, not a miss. Anything else it prints is a call site you did not convert.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/proxy/ 2>&1 | tail -5` then `grep -rn "Authorized(r.RemoteAddr" --include="*.go" internal/ | grep -v _test.go`
Expected: PASS, and the grep returns exactly one line — `authorizedRequest`'s delegation inside `handler.go`. Two or more lines means an unconverted call site.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy/
git commit -m "feat(proxy): authorize a browser session alongside key and loopback"
```

---
### Task 3: The sign-in endpoint

**Files:**
- Create: `internal/web/signin.go`
- Create: `internal/web/signin_test.go`

**Interfaces:**
- Consumes: `Sessions` from Task 1.
- Produces:
  - `func SignInHandler(sessions *Sessions, apiKey string) http.HandlerFunc` — serves both `POST` (sign in) and `DELETE` (sign out)
  - `const CookieName = "aiproxy_session"` — must equal `proxy.sessionCookie`

- [ ] **Step 1: Write the failing test**

```go
package web

import (
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
```

Add the two helpers at the bottom of the test file:

```go
var tlsState = tls.ConnectionState{}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
```

with `crypto/tls` imported.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run 'TestSignIn|TestSecure|TestWrongKey|TestRejection|TestSignOut|TestMalformed' -v`
Expected: FAIL — `undefined: SignInHandler`.

- [ ] **Step 3: Write minimal implementation**

```go
package web

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"
)

// CookieName is the browser's dashboard session cookie. It must equal
// proxy.sessionCookie — the two packages cannot share a constant without a
// dependency neither wants, so a test in internal/proxy asserts they agree.
const CookieName = "aiproxy_session"

// SignInHandler exchanges the proxy API key for a session cookie, and revokes
// it on DELETE.
//
// This endpoint exists because a browser cannot attach an x-api-key header to
// a top-level navigation. The key crosses the wire exactly once, in a POST
// body — never in a URL, where it would land in access logs and Referer
// headers — and the browser holds an opaque token afterwards.
func SignInHandler(sessions *Sessions, apiKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			if c, err := r.Cookie(CookieName); err == nil {
				sessions.Delete(c.Value)
			}
			http.SetCookie(w, expireCookie(r))
			w.WriteHeader(http.StatusNoContent)
			return
		case http.MethodPost:
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Use POST to sign in and DELETE to sign out.")
			return
		}

		// An unconfigured key must never authorize. Without this branch the
		// constant-time compare below would succeed on two empty strings and
		// hand a session to anyone who posted an empty body.
		if apiKey == "" {
			writeJSONError(w, http.StatusForbidden, "authentication_error",
				"No proxy API key is configured, so the dashboard cannot be signed in to. Set listen.apiKey in config.json.")
			return
		}

		var body struct {
			APIKey string `json:"apiKey"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request_error", "Could not read the sign-in request.")
			return
		}

		if subtle.ConstantTimeCompare([]byte(body.APIKey), []byte(apiKey)) != 1 {
			// Deliberately says nothing about what was submitted: echoing the
			// attempt writes it into the browser console and any log that
			// captures response bodies.
			writeJSONError(w, http.StatusUnauthorized, "authentication_error", "Incorrect API key.")
			return
		}

		tok, err := sessions.Mint()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "Could not create a session.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: CookieName, Value: tok, Path: "/_aiproxy",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
			Secure: r.TLS != nil, MaxAge: int(sessions.ttl / time.Second),
		})
		w.WriteHeader(http.StatusNoContent)
	}
}

func expireCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: "", Path: "/_aiproxy",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil, MaxAge: -1,
	}
}

// writeJSONError mirrors the envelope internal/proxy uses, so the dashboard
// parses one error shape from every endpoint it talks to.
func writeJSONError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
```

- [ ] **Step 4: Add the cookie-name agreement test**

The two packages cannot share the constant without a dependency neither wants, so pin that they agree. In `internal/proxy/session_auth_test.go`:

```go
// The proxy reads the cookie internal/web writes. They cannot share a constant
// without a dependency edge neither package wants, so this is what stops them
// drifting apart into a dashboard that signs in and is then refused on every
// subsequent request.
func TestSessionCookieNameMatchesTheWebPackage(t *testing.T) {
	if sessionCookie != web.CookieName {
		t.Errorf("proxy.sessionCookie = %q, web.CookieName = %q", sessionCookie, web.CookieName)
	}
}
```

This is the one place `internal/proxy`'s **test** imports `internal/web`; the non-test code must not.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/web/ ./internal/proxy/ -v 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/ internal/proxy/
git commit -m "feat(web): exchange the API key for a hardened session cookie"
```

---

### Task 4: Refuse to be open by default

The dashboard makes a non-loopback bind meaningfully more dangerous: account removal and OAuth login become reachable. Today `Authorized` returns true when no key is configured, so that combination is wide open.

**Files:**
- Modify: `cmd/aiproxy/main.go` (flag, guard, wire the session store into `HandlerOptions`)
- Create: `cmd/aiproxy/bindguard_test.go`

**Interfaces:**
- Consumes: `proxy.IsLoopback`, `web.NewSessions` (Task 1), `proxy.HandlerOptions.Sessions` (Task 2).
- Produces: `func checkBindSafety(addr, apiKey string, insecure bool) error`; flag `--insecure-no-auth`.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"strings"
	"testing"
)

func TestBindGuard(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		apiKey   string
		insecure bool
		wantErr  bool
	}{
		{"loopback without a key is fine", "127.0.0.1:3456", "", false, false},
		{"loopback with a key is fine", "127.0.0.1:3456", "k", false, false},
		{"localhost name resolves as loopback", "localhost:3456", "", false, false},
		{"ipv6 loopback is fine", "[::1]:3456", "", false, false},
		{"public bind with a key is fine", "0.0.0.0:3456", "k", false, false},
		{"public bind without a key is refused", "0.0.0.0:3456", "", false, true},
		{"specific public address without a key is refused", "203.0.113.9:3456", "", false, true},
		{"empty host means all interfaces and is refused", ":3456", "", false, true},
		{"the escape hatch permits it", "0.0.0.0:3456", "", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkBindSafety(c.addr, c.apiKey, c.insecure)
			if c.wantErr && err == nil {
				t.Fatal("expected a refusal, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected refusal: %v", err)
			}
			if c.wantErr && !strings.Contains(err.Error(), "apiKey") {
				t.Errorf("refusal does not name the fix: %v", err)
			}
		})
	}
}
```

`":3456"` is the case that matters most: it is the shorthand people actually type, it binds every interface, and a naive `IsLoopback("")` returns false only by accident. Pin it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/aiproxy/ -run TestBindGuard -v`
Expected: FAIL — `undefined: checkBindSafety`.

- [ ] **Step 3: Write minimal implementation**

```go
// checkBindSafety refuses to start a publicly-reachable proxy with no API key.
//
// Before the dashboard, an unauthenticated non-loopback bind exposed inference
// and account listing. With the dashboard it also exposes OAuth login, account
// removal, and settings — the whole control plane. A warning is not enough:
// warnings scroll past, and the failure mode is a stranger holding your Claude
// subscription.
//
// The escape hatch exists because refusing every public bind would break a
// legitimate deployment behind an authenticating reverse proxy. It is loud, it
// is explicit, and it is named for what it costs.
func checkBindSafety(addr, apiKey string, insecure bool) error {
	if apiKey != "" || insecure {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	// An empty host is the ":3456" shorthand, which binds every interface. It
	// must not be read as "no host, therefore local". And IsLoopback parses IPs
	// only, so the NAME localhost needs its own branch.
	if host == "localhost" || (host != "" && proxy.IsLoopback(addr)) {
		return nil
	}
	return fmt.Errorf(
		"refusing to listen on %s with no API key: the dashboard and control plane would be open to anyone who can reach this port.\n"+
			"Set listen.apiKey in config.json, bind to 127.0.0.1 instead, or pass --insecure-no-auth if this is behind an authenticating proxy",
		addr)
}
```

**`proxy.IsLoopback` returns false for the NAME `localhost`** — verified: it
calls `net.ParseIP`, which yields nil for a hostname. It covers `127.0.0.1` and
`[::1]` and nothing else. So `checkBindSafety` needs its own branch:

```go
	if host == "localhost" || (host != "" && proxy.IsLoopback(addr)) {
		return nil
	}
```

Put the branch HERE, not in `proxy.IsLoopback`. That function gates every
proxied request's authorization, and widening it to resolve hostnames would put
a name lookup on the hot path and change who is exempt from the API key.

Wire it in `main.go` before `listen(cfg.Listen.Addr)`:

```go
	if err := checkBindSafety(cfg.Listen.Addr, cfg.Listen.APIKey, *insecureNoAuth); err != nil {
		return err
	}
	if *insecureNoAuth && cfg.Listen.APIKey == "" {
		log.Warn("--insecure-no-auth: the dashboard and control plane are UNAUTHENTICATED on this listener")
	}
```

Register the flag beside the existing ones:

```go
	insecureNoAuth := flag.Bool("insecure-no-auth", false,
		"permit a non-loopback listener with no API key; the control plane is then unauthenticated")
```

And construct the session store, wiring it into `HandlerOptions`:

```go
	sessions := web.NewSessions(30 * 24 * time.Hour)
```

with `Sessions: sessions` added to the `HandlerOptions` literal.

- [ ] **Step 4: Run the tests**

Run: `go test ./cmd/aiproxy/ -v 2>&1 | tail -10`
Expected: PASS, nine subtests.

- [ ] **Step 5: Commit**

```bash
git add cmd/aiproxy/
git commit -m "feat(cmd): refuse a public bind with no API key"
```

---
### Task 5: Embedded assets, served under the reserved prefix

**Files:**
- Create: `internal/web/web.go`
- Create: `internal/web/web_test.go`
- Create: `internal/web/assets/index.html` (a placeholder shell — Task 6 fills it)
- Create: `internal/web/assets/style.css`
- Modify: `internal/proxy/router.go`
- Modify: `internal/proxy/handler.go` (`HandlerOptions.Web`)

**Interfaces:**
- Consumes: `Sessions` (Task 1), `SignInHandler` (Task 3).
- Produces:
  - `//go:embed assets` `var assetsFS embed.FS`
  - `func Handler(sessions *Sessions, apiKey string) http.Handler` — routes `/`, `/assets/*` and `/api/v1/session` relative to its mount point
  - `func AssetNames() []string` — every embedded path, for tests
  - `proxy.HandlerOptions.Web http.Handler` — nil means the dashboard is not mounted

- [ ] **Step 1: Write the failing test**

```go
package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServesTheShellAtRoot(t *testing.T) {
	h := Handler(NewSessions(time.Hour), "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "<html") {
		t.Errorf("root did not serve the shell: %.120s", body)
	}
}

func TestServesAnAsset(t *testing.T) {
	h := Handler(NewSessions(time.Hour), "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/style.css", nil))
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
}

func TestUnknownAssetIs404(t *testing.T) {
	h := Handler(NewSessions(time.Hour), "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/nope.js", nil))
	if w.Result().StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Result().StatusCode)
	}
}

// THE test that matters. A renamed or forgotten asset produces a 404 only in a
// real browser, on a screen nobody opened during development. Walking the
// shell's own references and resolving each against the embed catches it in CI
// instead.
func TestEveryReferencedAssetExists(t *testing.T) {
	shell, err := assetsFS.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := referencedAssets(string(shell))
	if len(refs) == 0 {
		t.Fatal("found no asset references in index.html — the extractor is broken, not the shell")
	}
	for _, ref := range refs {
		if _, err := assetsFS.ReadFile("assets/" + ref); err != nil {
			t.Errorf("index.html references assets/%s, which is not embedded", ref)
		}
	}
}

// Every JS module the app imports must also resolve. ES module specifiers are
// invisible to the HTML scan above.
func TestEveryImportedModuleExists(t *testing.T) {
	for _, name := range AssetNames() {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		src, err := assetsFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		dir := name[:strings.LastIndex(name, "/")+1]
		for _, spec := range importedModules(string(src)) {
			resolved := resolveRelative(dir, spec)
			if _, err := assetsFS.ReadFile(resolved); err != nil {
				t.Errorf("%s imports %q, which resolves to %s and is not embedded", name, spec, resolved)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run 'TestServes|TestUnknown|TestEvery' -v`
Expected: FAIL — `undefined: Handler`.

- [ ] **Step 3: Write minimal implementation**

```go
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

//go:embed assets
var assetsFS embed.FS

// Handler serves the dashboard: the shell at the mount root, static files
// under assets/, and the session exchange.
//
// Routing inside the SPA is hash-based (#/overview), so there is no
// server-side catch-all here and no path under the mount can be confused with
// a proxied upstream path. An unknown path is a 404, which is what the
// reserved prefix promises for everything it does not recognise.
func Handler(sessions *Sessions, apiKey string) http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic("web: embedded assets missing: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))
	signIn := SignInHandler(sessions, apiKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/session", signIn)
	mux.Handle("/assets/", cacheHeaders(http.StripPrefix("/assets/", files)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// The shell is never cached: it is the one file whose contents change
		// meaning when the binary is updated, and a stale shell loading new
		// assets is the confusing half-broken state.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := assetsFS.ReadFile("assets/index.html")
		w.Write(b)
	})
	return mux
}

// cacheHeaders lets a browser reuse assets within a session without pinning a
// stale copy across an update. no-cache means "revalidate", not "do not
// store": the browser keeps the file and asks whether it changed, which over
// loopback costs nothing and over a tunnel costs one conditional request.
func cacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// AssetNames lists every embedded file, for tests that walk the asset set.
func AssetNames() []string {
	var out []string
	fs.WalkDir(assetsFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

var (
	hrefSrc   = regexp.MustCompile(`(?:src|href)="(?:\./)?(assets/)?([^":]+?)"`)
	importRe  = regexp.MustCompile(`(?m)^\s*import\s[^'"]*['"]([^'"]+)['"]`)
)

// referencedAssets extracts local asset paths from the shell, ignoring
// absolute URLs and anything with a scheme.
func referencedAssets(html string) []string {
	var out []string
	for _, m := range hrefSrc.FindAllStringSubmatch(html, -1) {
		ref := m[2]
		if ref == "" || strings.HasPrefix(ref, "/") || strings.Contains(ref, "//") {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func importedModules(src string) []string {
	var out []string
	for _, m := range importRe.FindAllStringSubmatch(src, -1) {
		if strings.HasPrefix(m[1], ".") {
			out = append(out, m[1])
		}
	}
	return out
}

// resolveRelative joins an ES module specifier against the importing file's
// directory, collapsing the ../ segments a screens/ module uses to reach up.
func resolveRelative(dir, spec string) string {
	parts := strings.Split(strings.TrimSuffix(dir, "/"), "/")
	for _, seg := range strings.Split(spec, "/") {
		switch seg {
		case ".", "":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}
```

Create a minimal `assets/index.html` so the tests have something real to serve; Task 6 replaces its body:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>aiproxy</title>
  <link rel="stylesheet" href="assets/style.css">
</head>
<body>
  <noscript>The aiproxy dashboard needs JavaScript. Use the terminal UI instead: run <code>aiproxy</code> with no flags.</noscript>
  <main id="app"></main>
</body>
</html>
```

and an `assets/style.css` with the theme skeleton:

```css
:root {
  color-scheme: light dark;
  --bg: #ffffff; --fg: #16181d; --muted: #5c6370;
  --line: #e3e6ea; --panel: #f7f8fa; --accent: #3b6ea5;
}
@media (prefers-color-scheme: dark) {
  :root { --bg: #111317; --fg: #e6e8eb; --muted: #9aa1ac;
          --line: #262a31; --panel: #171a1f; --accent: #6ea8e0; }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg);
       font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
```

- [ ] **Step 4: Mount it in the router**

Add to `HandlerOptions` in `handler.go`:

```go
	// Web serves the browser dashboard under the reserved prefix. Nil leaves
	// the prefix API-only, which is what every existing test expects.
	Web http.Handler
```

In `router.go`, inside the existing `r.Route(ReservedPrefix, ...)` block and **above** the `cp.NotFound` registration:

```go
		// The dashboard mounts INSIDE the reserved prefix, as a sibling of the
		// API routes rather than by relaxing cp.NotFound. That handler exists so
		// a future control path can never fall through to the upstream, and an
		// unknown path under the prefix must stay a 404.
		if o.Web != nil {
			cp.Handle("/", http.StripPrefix(ReservedPrefix, o.Web))
			cp.Handle("/assets/*", http.StripPrefix(ReservedPrefix, o.Web))
			cp.Handle("/api/v1/session", http.StripPrefix(ReservedPrefix, o.Web))
		}
```

Wire it in `cmd/aiproxy/main.go` where `HandlerOptions` is built:

```go
		Web: web.Handler(sessions, cfg.Listen.APIKey),
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/web/ ./internal/proxy/ ./cmd/aiproxy/ 2>&1 | tail -10`
Expected: PASS. `TestEveryViewSourceMethodHasAControlRoute` must still pass untouched — this stage adds no `view.Source` methods.

- [ ] **Step 6: Add the import-boundary test**

`internal/web` must not reach past the seam, exactly as the TUI must not. Mirror the existing test — read `internal/tui/imports_test.go` and follow its approach:

```go
// internal/web reads through view.Source and nothing below it, so presentation
// can never compute an aggregate of its own or hold a live pointer into proxy
// state. Same rule as internal/tui, same reason (spec §3.1).
func TestWebImportsOnlyTheViewSeam(t *testing.T) {
	forbidden := []string{
		"github.com/nicko170/aiproxy/internal/account",
		"github.com/nicko170/aiproxy/internal/metrics",
		"github.com/nicko170/aiproxy/internal/proxy",
	}
	// walk this package's non-test files' imports and fail on any match
}
```

Copy the walking logic from `internal/tui/imports_test.go` rather than inventing a second approach.

- [ ] **Step 7: Commit**

```bash
git add internal/web/ internal/proxy/ cmd/aiproxy/
git commit -m "feat(web): embed and serve the dashboard shell under the reserved prefix"
```

---

### Task 6: The shell, the router, and signing in

**Files:**
- Modify: `internal/web/assets/index.html`
- Create: `internal/web/assets/dom.js`
- Create: `internal/web/assets/app.js`
- Create: `internal/web/assets/screens/signin.js`
- Modify: `internal/web/assets/style.css`

**Interfaces:**
- Consumes: `POST/DELETE /_aiproxy/api/v1/session` (Task 3), `GET /_aiproxy/api/v1/status`.
- Produces, exported from `app.js`:
  - `api(path, opts)` → parsed JSON; throws `ApiError {status, message}`
  - `registerScreen(name, {title, mount(el), unmount()})`
  - `navigate(hash)`, `currentScreen()`
  - `el(tag, attrs, ...children)` — the DOM helper every screen and chart uses, defined in `dom.js` and re-exported here
  - `subscribeEvents(handler)` → unsubscribe function; owns the single `EventSource`
  - `startPolling(fn, intervalMs)` → stop function; visible-tab only

- [ ] **Step 1: Write the DOM helper in its own module**

`el` lives in `dom.js`, NOT in `app.js`, and this is load-bearing rather than
tidiness. `app.js` registers a `hashchange` listener at module scope, so
importing it outside a browser throws `ReferenceError: window is not defined`.
`chart.js` needs `el` for its marks (Task 8) and is covered by `node --test`
(Task 7) — so if `chart.js` reached for `el` through `app.js`, the chart tests
would die the moment the marks landed. Verified: a module with a top-level
`window.addEventListener` fails to import under node.

`dom.js` touches `document` only inside function bodies, so importing it under
node is harmless even though calling `el` there would not be.

```js
// The DOM helper every screen and every chart builds with.
//
// Separate from app.js deliberately: app.js registers listeners at module
// scope and cannot be imported outside a browser, while chart.js needs this
// helper AND is unit-tested under node. Keep this module free of top-level
// browser access.

const SVG_TAGS = new Set(["svg","g","path","rect","circle","line","text",
  "polyline","polygon","defs","clipPath","tspan","title"]);

// el builds a DOM node. Children that are strings become TEXT NODES, never
// markup — which is why no part of this app needs to think about escaping a
// model name, an account label, or an error message from an upstream.
export function el(tag, attrs = {}, ...children) {
  const ns = SVG_TAGS.has(tag) ? "http://www.w3.org/2000/svg" : "http://www.w3.org/1999/xhtml";
  const node = document.createElementNS(ns, tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, String(v));
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.appendChild(typeof child === "object" ? child : document.createTextNode(String(child)));
  }
  return node;
}
```

- [ ] **Step 2: Write the fetch layer**

`app.js` imports `el` from `./dom.js` and re-exports it, so screens can keep
importing everything from one place:

```js
export { el } from "./dom.js";
```

`app.js`:

```js
// The dashboard shell: routing, fetch, live events, and the DOM helper every
// screen builds with.
//
// No framework and no build step (spec §3). The helper below is the whole
// abstraction: it exists so nothing in this app ever assigns innerHTML with a
// value that came from the API.

const BASE = "/_aiproxy/api/v1";

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

// api fetches and unwraps the proxy's error envelope.
//
// The server's message is surfaced verbatim rather than replaced with
// something generic: it is frequently the only actionable thing available
// ("Model X is blocked by aiproxy", "no eligible account"), and a UI that
// swallows it makes every failure look identical.
export async function api(path, opts = {}) {
  const res = await fetch(BASE + path, {
    credentials: "same-origin",
    headers: opts.body ? { "content-type": "application/json" } : undefined,
    ...opts,
  });
  if (res.status === 204) return null;
  let body = null;
  try { body = await res.json(); } catch { /* non-JSON error bodies exist */ }
  if (!res.ok) {
    const msg = body?.error?.message || `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg);
  }
  return body;
}

```

- [ ] **Step 3: Add polling, events, and the router**

Appended to `app.js`:

```js
// startPolling runs fn on an interval, but ONLY while the tab is visible. A
// dashboard left open on a second monitor overnight should not spend the night
// querying SQLite on the other end of a tunnel.
export function startPolling(fn, intervalMs) {
  let timer = null;
  const tick = async () => { try { await fn(); } catch (e) { /* screens render their own errors */ } };
  const start = () => { if (timer === null) { tick(); timer = setInterval(tick, intervalMs); } };
  const stop = () => { if (timer !== null) { clearInterval(timer); timer = null; } };
  const onVisibility = () => (document.visibilityState === "visible" ? start() : stop());
  document.addEventListener("visibilitychange", onVisibility);
  onVisibility();
  return () => { stop(); document.removeEventListener("visibilitychange", onVisibility); };
}

// The SSE connection is owned HERE, not by the Activity screen, so it survives
// navigation: switching to Usage and back does not drop the feed or lose the
// events that arrived in between.
let source = null;
const eventHandlers = new Set();
const recent = [];
const RECENT_CAP = 2000;

export function subscribeEvents(handler) {
  eventHandlers.add(handler);
  ensureSource();
  return () => eventHandlers.delete(handler);
}

export function recentEvents() { return recent; }

function ensureSource() {
  if (source) return;
  let backoff = 1000;
  const connect = () => {
    source = new EventSource(BASE + "/events");
    source.onopen = () => { backoff = 1000; setConnState("live"); };
    source.onmessage = (m) => {
      let ev; try { ev = JSON.parse(m.data); } catch { return; }
      recent.push(ev);
      if (recent.length > RECENT_CAP) recent.splice(0, recent.length - RECENT_CAP);
      for (const h of eventHandlers) h(ev);
    };
    // A feed that has quietly stopped is worse than one that says it stopped:
    // the operator reads "no activity" as "nothing is happening".
    source.onerror = () => {
      setConnState("reconnecting");
      source.close(); source = null;
      setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, 30000);
    };
  };
  connect();
}

function setConnState(state) {
  const dot = document.getElementById("conn");
  if (dot) { dot.className = "conn " + state; dot.title = state; }
}

const screens = new Map();
let active = null;

export function registerScreen(name, screen) { screens.set(name, screen); }
export function currentScreen() { return (location.hash.replace(/^#\/?/, "") || "overview").split("?")[0]; }
export function navigate(name) { location.hash = "#/" + name; }

async function render() {
  const name = currentScreen();
  const screen = screens.get(name) || screens.get("overview");
  if (active && active.unmount) active.unmount();
  const main = document.getElementById("app");
  main.replaceChildren();
  active = screen;
  for (const link of document.querySelectorAll("nav a")) {
    link.classList.toggle("active", link.dataset.screen === name);
  }
  try {
    await screen.mount(main);
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) { showSignIn(); return; }
    main.replaceChildren(el("div", { class: "error" },
      el("p", {}, "This screen failed to load."), el("code", {}, e.message),
      el("button", { onclick: () => render() }, "Retry")));
  }
}

window.addEventListener("hashchange", render);
export { render };
```

- [ ] **Step 4: Write the sign-in screen and boot sequence**

`screens/signin.js`:

```js
import { api, el, render } from "../app.js";

// Sign-in is not a registered screen: it replaces the whole shell, because
// there is nothing to navigate to until it succeeds.
export function showSignIn(message) {
  document.body.classList.add("signin");
  const input = el("input", { type: "password", id: "key", autocomplete: "current-password",
                              placeholder: "proxy API key" });
  const err = el("p", { class: "error-text" }, message || "");

  const submit = async (e) => {
    e.preventDefault();
    err.replaceChildren();
    try {
      await api("/session", { method: "POST", body: JSON.stringify({ apiKey: input.value }) });
      document.body.classList.remove("signin");
      await boot();
    } catch (ex) {
      err.replaceChildren(document.createTextNode(ex.message));
    }
  };

  document.getElementById("app").replaceChildren(
    el("form", { class: "signin-card", onsubmit: submit },
      el("h1", {}, "aiproxy"),
      el("label", { for: "key" }, "Proxy API key"),
      input, err,
      el("button", { type: "submit" }, "Sign in")));
  input.focus();
}
```

Boot, at the bottom of `app.js`:

```js
// Boot probes an authenticated endpoint. A 401 means sign in; anything else
// means the proxy is reachable and we can render.
export async function boot() {
  try {
    await api("/status");
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) { showSignIn(); return; }
  }
  await render();
}
```

Wire `showSignIn` into `app.js` via a late import to avoid a cycle, and replace the shell's `<main>` with the real chrome — header, nav for the five screens, a connection dot, and a sign-out control that `DELETE`s the session and calls `showSignIn()`.

- [ ] **Step 5: Verify by hand**

```bash
go build -o /tmp/aiproxy ./cmd/aiproxy && /tmp/aiproxy --headless &
open http://127.0.0.1:3456/_aiproxy/
```

Expected: the shell loads, and on loopback with no configured key it goes straight to the (empty) overview rather than the sign-in card. Set `listen.apiKey` in a scratch config and re-run to see the sign-in path.

- [ ] **Step 6: Commit**

```bash
git add internal/web/
git commit -m "feat(web): shell, hash router, fetch layer, and sign-in"
```

---
### Task 7: Chart maths — scales, ticks, formatting

Everything in this task is a pure function from data to numbers, which is what makes the chart module testable at all. No DOM, no SVG, no rendering.

**Files:**
- Create: `internal/web/assets/chart.js`
- Create: `internal/web/assets/chart_test.js`
- Create: `internal/web/charts_test.go` (the `node --test` bridge)

**Interfaces:**
- Consumes: nothing.
- Produces, exported from `chart.js`:
  - `linearScale({domain:[lo,hi], range:[a,b]})` → `fn(v)` with `.invert(px)`
  - `timeScale({domain:[t0,t1], range:[a,b]})` → same shape
  - `niceTicks(lo, hi, target)` → `number[]`
  - `timeTicks(t0, t1, target)` → `{at, label}[]`
  - `formatTokens(n)`, `formatCostMicros(n)`, `formatMs(n)`, `formatPercent(f)`
  - `stack(points, keys)` → `{key, values:[{x, y0, y1}]}[]`
  - `colorFor(key)` → a stable CSS colour string

- [ ] **Step 1: Write the failing tests**

`chart_test.js`:

```js
import test from "node:test";
import assert from "node:assert/strict";
import {
  linearScale, timeScale, niceTicks, timeTicks,
  formatTokens, formatCostMicros, formatMs, formatPercent,
  stack, colorFor,
} from "./chart.js";

test("linearScale maps domain onto range", () => {
  const s = linearScale({ domain: [0, 100], range: [0, 200] });
  assert.equal(s(0), 0);
  assert.equal(s(50), 100);
  assert.equal(s(100), 200);
});

// A flat series — every value identical — is the case that divides by zero.
// It happens constantly in practice: one account, one model, no traffic.
test("linearScale survives a zero-width domain", () => {
  const s = linearScale({ domain: [5, 5], range: [0, 100] });
  const v = s(5);
  assert.ok(Number.isFinite(v), `expected a finite value, got ${v}`);
});

test("linearScale inverts", () => {
  const s = linearScale({ domain: [0, 10], range: [0, 500] });
  assert.equal(Math.round(s.invert(250)), 5);
});

test("niceTicks produces round numbers covering the range", () => {
  const ticks = niceTicks(0, 97, 5);
  assert.ok(ticks.length >= 3 && ticks.length <= 8, `got ${ticks.length} ticks`);
  assert.equal(ticks[0], 0);
  assert.ok(ticks[ticks.length - 1] >= 97);
  for (const t of ticks) assert.ok(Number.isFinite(t));
});

test("niceTicks handles an all-zero series", () => {
  const ticks = niceTicks(0, 0, 5);
  assert.ok(ticks.length >= 2, "an all-zero series still needs an axis");
  assert.ok(ticks.every(Number.isFinite));
});

// The four ranges the Usage screen offers. Each must produce a readable number
// of labels — not one, not forty.
test("timeTicks is sane across every offered range", () => {
  const end = Date.UTC(2026, 7, 25, 12, 0, 0);
  for (const [name, span] of [
    ["1h", 3600e3], ["24h", 86400e3], ["7d", 7 * 86400e3], ["30d", 30 * 86400e3],
  ]) {
    const ticks = timeTicks(end - span, end, 6);
    assert.ok(ticks.length >= 2 && ticks.length <= 12, `${name}: ${ticks.length} ticks`);
    for (const t of ticks) {
      assert.ok(t.at >= end - span && t.at <= end, `${name}: tick outside range`);
      assert.ok(t.label.length > 0 && t.label.length <= 12, `${name}: bad label ${t.label}`);
    }
  }
});

test("token formatting is compact and lossless enough to read", () => {
  assert.equal(formatTokens(0), "0");
  assert.equal(formatTokens(999), "999");
  assert.equal(formatTokens(1500), "1.5K");
  assert.equal(formatTokens(1_500_000), "1.5M");
  assert.equal(formatTokens(2_300_000_000), "2.3B");
});

test("cost is formatted from micros without float drift", () => {
  assert.equal(formatCostMicros(0), "$0.00");
  assert.equal(formatCostMicros(1_000_000), "$1.00");
  assert.equal(formatCostMicros(223_333_729), "$223.33");
  assert.equal(formatCostMicros(1_500), "$0.0015");
});

test("durations read as ms then seconds", () => {
  assert.equal(formatMs(0), "0ms");
  assert.equal(formatMs(240), "240ms");
  assert.equal(formatMs(3972), "3.97s");
});

test("percent formats a fraction", () => {
  assert.equal(formatPercent(0), "0%");
  assert.equal(formatPercent(0.58), "58%");
  assert.equal(formatPercent(1), "100%");
});

test("stack accumulates series in key order", () => {
  const points = [
    { x: 1, key: "a", value: 2 }, { x: 1, key: "b", value: 3 },
    { x: 2, key: "a", value: 1 }, { x: 2, key: "b", value: 1 },
  ];
  const series = stack(points, ["a", "b"]);
  assert.equal(series.length, 2);
  assert.deepEqual(series[0].values.map(v => [v.y0, v.y1]), [[0, 2], [0, 1]]);
  assert.deepEqual(series[1].values.map(v => [v.y0, v.y1]), [[2, 5], [1, 2]]);
});

// A key missing from one bucket must contribute zero, not a hole that shifts
// every series above it.
test("stack fills gaps with zero", () => {
  const series = stack([{ x: 1, key: "a", value: 2 }, { x: 2, key: "b", value: 4 }], ["a", "b"]);
  assert.deepEqual(series[0].values.map(v => v.y1), [2, 0]);
  assert.deepEqual(series[1].values.map(v => [v.y0, v.y1]), [[2, 2], [0, 4]]);
});

// An account must keep its colour across screens and reloads, or the legend on
// Usage disagrees with the cards on Overview.
test("colorFor is stable and deterministic", () => {
  assert.equal(colorFor("acct-1"), colorFor("acct-1"));
  assert.notEqual(colorFor("acct-1"), colorFor("acct-2"));
  assert.match(colorFor("acct-1"), /^(#|hsl|var|oklch)/);
});
```

- [ ] **Step 2: Write the Go bridge that runs them**

`charts_test.go`:

```go
package web

import (
	"os/exec"
	"strings"
	"testing"
)

// The chart maths are pure functions, so they are worth testing — but node is
// a TEST-TIME convenience only. It is never required to build: `go build`
// produces a working dashboard on a machine with no node at all, which is the
// no-build-step constraint the design turns on.
//
// So this skips loudly rather than failing when node is absent, and CI installs
// node to get the coverage.
func TestChartMath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: skipping the chart-maths tests (they are not a build dependency)")
	}
	out, err := exec.Command(node, "--test", "assets/").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test failed:\n%s", out)
	}
	if !strings.Contains(string(out), "pass") {
		t.Errorf("node --test produced no passing tests:\n%s", out)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/web/ -run TestChartMath -v`
Expected: FAIL — `chart.js` does not exist, so `node --test` errors on the import.

- [ ] **Step 4: Implement `chart.js`**

Write the module against the tests above. The pieces that need care:

`linearScale` must guard a zero-width domain — map everything to the middle of the range rather than producing `NaN` or `Infinity`. That case is not exotic: one account with flat traffic produces it on every render.

`niceTicks` picks a step from the 1/2/5×10ⁿ family that lands near the requested count, then walks from a floored start to a ceilinged end. For an all-zero series, fall back to a `[0, 1]` domain so the axis still draws.

`timeTicks` chooses its unit from the span — minutes under an hour, hours under two days, days beyond — and formats labels accordingly (`HH:MM`, `HH:MM`, `DD Mon`). Labels use the browser's locale via `Intl.DateTimeFormat`, and times are local, not UTC: an operator reading "the spike at 14:00" means their own clock.

`formatCostMicros` divides by 1e6 and formats to two decimals, dropping to four when the value is under a cent so a small cost does not render as `$0.00`. Do the arithmetic on integers where possible — micros exist precisely so cost is not a float.

`colorFor` hashes the key to an index into a fixed categorical palette. **This is the one part of the module governed by the `dataviz` skill** — invoke it before choosing palette values, and follow its guidance on categorical ramps and contrast in both light and dark schemes. The requirement this plan fixes is behavioural, not aesthetic: the same key always yields the same colour, and adjacent series are distinguishable in greyscale.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/web/ -v` and, if node is installed, `node --test internal/web/assets/`
Expected: PASS. Fix the implementation until every case above passes, including the zero-width, all-zero, and gap cases.

- [ ] **Step 6: Commit**

```bash
git add internal/web/
git commit -m "feat(web): chart scales, ticks, and formatting"
```

---

### Task 8: Chart marks — area, line, bar, gauge, legend, tooltip

**Files:**
- Modify: `internal/web/assets/chart.js`
- Modify: `internal/web/assets/style.css`

**Interfaces:**
- Consumes: everything from Task 7, plus `el` from `./dom.js` — NOT from `app.js`, which cannot be imported under node and would break Task 7's chart tests.
- Produces:
  - `stackedArea({points, keys, width, height, valueOf, format})` → `SVGElement`
  - `lineChart({series, width, height, format})` → `SVGElement`
  - `barChart({bars, width, height, format})` → `SVGElement`
  - `gauge({fraction, label, sublabel})` → `SVGElement`
  - `legend(keys)` → `HTMLElement`
  - `emptyChart(message)` → `SVGElement`

- [ ] **Step 1: Build the marks**

Every chart is an `<svg>` built through `el`, with `viewBox` set and `width`/`height` at 100% so it scales to its container — that is what makes the layout work at phone width without a second implementation.

Requirements each mark must meet, which are the difference between "a chart" and "a chart you can read":

- **Axes drawn once, consistently.** A left value axis with `niceTicks`, a bottom time axis with `timeTicks`, gridlines at tick positions in `--line`, labels in `--muted`.
- **Every series labelled, not only coloured.** `legend()` renders a swatch **and** the key's text. A stacked area whose only distinguishing feature is hue is unreadable in greyscale and to a meaningful fraction of operators.
- **An explicit empty state.** `emptyChart("No traffic in this range")` renders axes and a centred message. It must be visibly distinct from a failed fetch, which is the screen's job to render — a chart that shows nothing must never be ambiguous between "zero" and "broken".
- **One tooltip implementation.** A crosshair follows the pointer, snaps to the nearest bucket, and shows every series' value at that bucket with the shared formatter. Keyboard-reachable: arrow keys move the crosshair when the chart has focus, so the data is not mouse-only.
- **`title` elements on marks** for assistive technology, since an SVG with no text is opaque to a screen reader.

Consult the `dataviz` skill before choosing dimensions, spacing, and palette; this plan fixes behaviour, the skill governs the look.

- [ ] **Step 2: Add a visual smoke page**

Create `assets/charts-preview.html` — a page that renders every mark with canned data, so a human can see all six at once without driving real traffic. It is embedded like any other asset and reachable at `/_aiproxy/assets/charts-preview.html`.

This is a development aid and it earns its place: chart bugs are visual, and the alternative is generating real traffic through a live account to see whether a gradient renders.

- [ ] **Step 3: Verify by eye**

```bash
go build -o /tmp/aiproxy ./cmd/aiproxy && /tmp/aiproxy --headless &
open http://127.0.0.1:3456/_aiproxy/assets/charts-preview.html
```

Check: both colour schemes (toggle your OS theme), a narrow window, the empty state, and a single-point series.

- [ ] **Step 4: Commit**

```bash
git add internal/web/
git commit -m "feat(web): chart marks, legend, tooltip, and a preview page"
```

---
### Task 9: Overview

**Files:**
- Create: `internal/web/assets/screens/overview.js`
- Modify: `internal/web/assets/style.css`

**Interfaces:**
- Consumes: `api`, `el`, `registerScreen`, `startPolling` (Task 6); `gauge`, `colorFor`, `formatMs`, `formatPercent` (Tasks 7–8).
- Produces: a registered screen named `overview`. Exports `renderAccountCard(account, status)` so Accounts (Task 12) reuses the card rather than growing a second one.

- [ ] **Step 1: Build the screen**

Two fetches, both already existing: `GET /status` and `GET /accounts`, polled every 5s while the tab is visible.

**Header strip** from `Status`: listen address, uptime (humanised), in-flight, `ttfbP95Ms` via `formatMs`, and — only when non-zero — `metricsDropped` and `eventsDropped`, because a silent sample drop is exactly the thing spec §7.3 wants visible. Update availability renders as a badge when `update.available`, linking `update.releaseUrl`.

**Account cards**, one per `Account`:
- label, provider, status, priority, in-flight
- a `gauge()` per entry in `buckets`, labelled with the bucket name and `formatPercent`
- `rateLimitedUntil` and `pausedUntil` rendered as countdowns **when non-zero** — these are `omitempty` in the JSON, so absent means "not limited", and rendering "limited until 1 Jan 1970" for a zero is the bug to avoid
- a pin control reflecting `status.pinnedAccountId`

**The pin is the one mutation on this screen.** `POST /accounts/pin` with the id, or with an empty id to clear. Reflect the new state from the next poll rather than optimistically — a pin that appears to work and did not is worse than a half-second delay.

- [ ] **Step 2: Handle the three states**

Loading renders skeleton cards, not an empty page. A failed fetch renders the error with `error.message` verbatim and a Retry. Zero accounts renders a prompt pointing at the Accounts screen to add one — a new install lands here first, and an empty overview with no next step is a dead end.

- [ ] **Step 3: Verify by hand**

```bash
go build -o /tmp/aiproxy ./cmd/aiproxy && /tmp/aiproxy --headless &
open http://127.0.0.1:3456/_aiproxy/#/overview
```

Check against the TUI's Overview (`aiproxy`, key `1`): the same accounts, the same buckets, the same percentages. They read the same seam, so any disagreement is a bug in this screen.

- [ ] **Step 4: Commit**

```bash
git add internal/web/
git commit -m "feat(web): overview screen"
```

---

### Task 10: Activity

**Files:**
- Create: `internal/web/assets/screens/activity.js`
- Modify: `internal/web/assets/style.css`

**Interfaces:**
- Consumes: `subscribeEvents`, `recentEvents`, `el`, `registerScreen` (Task 6); `formatMs`, `formatTokens`, `colorFor` (Task 7).
- Produces: a registered screen named `activity`.

- [ ] **Step 1: Build the feed**

Attach to the shell's existing SSE subscription and seed from `recentEvents()`, so switching to this screen shows the events that arrived while you were elsewhere rather than an empty table that fills slowly.

Columns from `view.Event`: time, model, account, status, outcome, `durationMs`, `ttfbMs`, tokens in/out. Account labels come from the `/accounts` fetch, since the event carries an id.

Rows are colour-coded by outcome, and **the outcome text is always shown** — never colour alone. `ttfbMs` of `-1` is the established "no first byte was produced" sentinel and must render as `—`, not as `-1ms`.

- [ ] **Step 2: Add pause and filters**

Pause stops rendering but keeps buffering, and shows how many arrived while paused. Filters by account, model and outcome are `<select>` elements populated from what has actually been seen, not a hardcoded list — the outcome vocabulary grows, and a filter that cannot select `overloaded` is worse than no filter.

The rendered row count is capped at the shell's `RECENT_CAP`; a tab open for a week must not grow without bound.

- [ ] **Step 3: Show the connection state**

The shell's connection dot already reflects live/reconnecting. This screen additionally renders a banner while reconnecting, because "no rows are appearing" and "the feed is disconnected" look identical otherwise, and only one of them means the proxy is idle.

- [ ] **Step 4: Verify by hand**

Drive traffic through the proxy and watch rows arrive. Then kill and restart the proxy with the tab open: the banner must appear, and the feed must resume on its own without a reload.

- [ ] **Step 5: Commit**

```bash
git add internal/web/
git commit -m "feat(web): live activity feed"
```

---

### Task 11: Usage

**Files:**
- Create: `internal/web/assets/screens/usage.js`
- Modify: `internal/web/assets/style.css`

**Interfaces:**
- Consumes: `api`, `el`, `registerScreen`, `startPolling` (Task 6); `stackedArea`, `lineChart`, `barChart`, `legend`, `emptyChart`, `stack`, `formatTokens`, `formatCostMicros`, `formatMs` (Tasks 7–8).
- Produces: a registered screen named `usage`.

- [ ] **Step 1: Build the controls and fetches**

Range selector (1h/24h/7d/30d) and group-by toggle (account/model/outcome), both reflected in the hash so a view is linkable and survives a reload: `#/usage?range=24h&group=model`.

Granularity follows the range — `minute` for 1h, `hour` beyond — because those are the only two rollups the store keeps, and asking for minutes over 30 days would return a series no chart can draw.

Three fetches per view: `/usage?from&to&granularity&groupBy`, `/totals?from&to`, `/latency?from&to`. Polled at 30s, visible-tab only.

- [ ] **Step 2: Render the charts**

- **Tokens over time**, stacked by the selected grouping, input and output as separate stacks.
- **Cost over time**, stacked the same way, formatted from `costMicros`.
- **TTFB p50/p95** as two lines from `Latency`.
- **Outcome mix** as a bar chart when grouping is `outcome`.

Every chart shares the range, the grouping, and the colour assignment, so a key's colour is the same in all of them and in the legend.

- [ ] **Step 3: Render the totals table honestly**

`Totals` gives requests, tokens by kind, and `costMicros`. **When `unpricedRequests` is non-zero, the cost is annotated as incomplete** — "≥ $12.34 (3 requests unpriced)" — rather than presented as final. That field exists precisely so a cost total is never shown as complete when it is not (spec §7.4), and the TUI already honours it. A dashboard that quietly disagrees with the TUI about cost is the exact failure §2 invariant 4 exists to prevent.

- [ ] **Step 4: Verify against the TUI**

Open the TUI's Usage screen (key `3`) at the same range and grouping and compare totals. They read the same seam; any difference is a bug here.

- [ ] **Step 5: Commit**

```bash
git add internal/web/
git commit -m "feat(web): usage charts and totals"
```

---

### Task 12: Accounts

**Files:**
- Create: `internal/web/assets/screens/accounts.js`
- Modify: `internal/web/assets/style.css`

**Interfaces:**
- Consumes: `api`, `el`, `registerScreen`, `startPolling` (Task 6); `renderAccountCard` (Task 9); `lineChart` (Task 8).
- Produces: a registered screen named `accounts`.

- [ ] **Step 1: List and mutate**

Reuse `renderAccountCard` from Overview, adding controls:
- enable/disable → `POST /accounts/{id}/enabled` with `{"enabled":bool}`
- priority → `POST /accounts/{id}/priority` with `{"priority":int}`
- remove → `DELETE /accounts/{id}`
- probe now → `POST /probe`
- import → `POST /accounts/import`

Every mutation re-fetches rather than mutating local state optimistically, for the reason the pin control gives in Task 9.

- [ ] **Step 2: Confirm the destructive ones**

Remove and disable both get a confirmation naming the account. They are adjacent to controls used constantly, and one of them ends every session pinned to that account. The confirm is an in-page dialog, not `window.confirm`, so it can name the consequence: removing an account deletes its credential from `config.json` and cannot be undone without logging in again.

- [ ] **Step 3: Per-account detail**

Selecting a card opens detail with quota history from `GET /quota/history?account&from&to`, rendered as one line per bucket via `lineChart`, with reset markers where `resetsAt` changes. This is the one view with no TUI equivalent worth matching — the TUI shows current utilisation; the trend over a week is what tells you whether an account is actually carrying its share.

- [ ] **Step 4: Surface errors from mutations**

A failed mutation renders the server's message beside the control that failed, not as a page-level error. `writeMutationError` already distinguishes a 404 (unknown account) from a 500, and that distinction should reach the operator.

- [ ] **Step 5: Commit**

```bash
git add internal/web/
git commit -m "feat(web): account management"
```

---

### Task 13: Adding an account — the paste-the-code login

**Files:**
- Create: `internal/web/assets/screens/login.js`
- Modify: `internal/web/assets/screens/accounts.js` (entry point)

**Interfaces:**
- Consumes: `api`, `el` (Task 6).
- Produces: `startLogin(provider)` — the modal flow, opened from Accounts.

- [ ] **Step 1: Build the three-step flow**

The callback listener binds to port 1455 **on the aiproxy host** and the `redirect_uri` is fixed in the provider's registered OAuth app, so on a server it can never reach the operator's browser. This flow therefore always uses the paste path, which exists for exactly this reason.

1. `POST /accounts/login` with `{"provider":"anthropic"|"openai"}` → `{sessionId, url}`.
2. Render the authorize URL as both a link (`target="_blank" rel="noopener"`) and a copyable string — on a remote deploy the operator may be reading over SSH and needs to move the URL by hand.
3. A field for the returned code → `POST /accounts/login/{sessionId}/code` with `{"code":"..."}`.
4. Poll `GET /accounts/login/{sessionId}` until it reports done, then close and re-fetch the account list.

The provider's page returns the code as `code#state`; the API accepts that form and a bare code, so paste it through unchanged rather than trying to split it here.

- [ ] **Step 2: Handle the failure paths explicitly**

A login can fail on a state mismatch, a timeout, a rejected exchange, or a failed persist, and `LoginResult.Err` carries which. Render that text — it is the difference between "try again" and "your clock is wrong". A cancelled or abandoned modal must stop polling; leaving a poll running against a dead session leaks a timer per attempt.

- [ ] **Step 3: Verify by hand**

Run a real login against Anthropic through the dashboard, on loopback first. Then confirm the copyable URL works when the browser is not on the aiproxy host — that is the case this whole flow exists for.

- [ ] **Step 4: Commit**

```bash
git add internal/web/
git commit -m "feat(web): add an account by pasting the authorization code"
```

---

### Task 14: Settings

**Files:**
- Create: `internal/web/assets/screens/settings.js`
- Modify: `internal/web/assets/style.css`

**Interfaces:**
- Consumes: `api`, `el`, `registerScreen` (Task 6).
- Produces: a registered screen named `settings`.

- [ ] **Step 1: Render the form from `view.Settings`**

Read once on entry with `GET /settings` — no polling; a form that reloads under the operator mid-edit is hostile. Fields, with the input type each deserves:

- `switchThreshold` — number, 0–1, step 0.01
- `retryBudgetMs`, `inlineAbsorbMaxMs`, `headerTimeoutMs`, `bodyIdleMs` — integers, in ms, each labelled with what it bounds
- `sessionAffinity` — checkbox
- `blockedModels` — a list editor; patterns are shell globs, so say so
- `quotaProbeIntervalSeconds`, `metricsRetentionDays` — integers
- `updateCheckEnabled` — checkbox
- `privacyEnabled`, `privacyOnScanFailure`, `privacyOnUnresolved` — checkbox and two selects

- [ ] **Step 2: Label the restart-gated fields as such**

`view.Settings`'s own comments record which fields do not take effect live: `updateCheckIntervalHours` (the ticker is built once at startup) and `privacyEnabled` (the detector set, cache salt and model session are built once). Render those with a "takes effect on restart" note.

Silently accepting a change that does nothing until restart is the kind of small dishonesty that costs an hour of debugging later. The API returns an `Applied` result — surface what it says was applied rather than assuming.

- [ ] **Step 3: Save and confirm**

`POST /settings` with the whole struct. On success, show what changed and re-read. On failure, show the server's message and leave the form as edited so nothing is lost.

- [ ] **Step 4: Commit**

```bash
git add internal/web/
git commit -m "feat(web): settings screen"
```

---

### Task 15: End-to-end test, docs, and the TUI's `o` key

**Files:**
- Create: `internal/proxy/web_e2e_test.go`
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-08-17-aiproxy-design.md` (mark stage 5 done)
- Modify: `internal/tui/app.go` (verify the `o` key's URL)

**Interfaces:**
- Consumes: everything.
- Produces: no new API.

- [ ] **Step 1: Write the end-to-end test**

```go
package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/web"
)

// Drives the REAL router with the dashboard mounted, which is the seam nothing
// else covers: internal/web's own tests exercise its handler in isolation, and
// the router's tests do not know the dashboard exists.
func TestDashboardMountsAndSessionRoundTrips(t *testing.T) {
	sessions := web.NewSessions(time.Hour)
	srv := httptest.NewServer(NewRouter(HandlerOptions{
		Manager: nil, Log: quietLogger(), APIKey: "secret",
		Sessions: sessions, Web: web.Handler(sessions, "secret"),
	}))
	defer srv.Close()

	// The shell is served.
	res, err := http.Get(srv.URL + ReservedPrefix + "/")
	if err != nil {
		t.Fatalf("GET dashboard: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", res.StatusCode)
	}

	// Sign in, and the cookie authorizes a subsequent API call.
	signIn, err := http.Post(srv.URL+ReservedPrefix+"/api/v1/session",
		"application/json", strings.NewReader(`{"apiKey":"secret"}`))
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer signIn.Body.Close()
	if signIn.StatusCode != http.StatusNoContent {
		t.Fatalf("sign-in status = %d, want 204", signIn.StatusCode)
	}
	cookies := signIn.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+ReservedPrefix+"/api/v1/status", nil)
	req.AddCookie(cookies[0])
	api, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated status call: %v", err)
	}
	defer api.Body.Close()
	if api.StatusCode == http.StatusUnauthorized {
		t.Error("the session cookie did not authorize an API call")
	}
}

// An unknown path under the reserved prefix stays a 404. Mounting the
// dashboard must not relax the guarantee that nothing under this prefix
// reaches an upstream.
func TestUnknownReservedPathIsStill404WithTheDashboardMounted(t *testing.T) {
	sessions := web.NewSessions(time.Hour)
	srv := httptest.NewServer(NewRouter(HandlerOptions{
		Log: quietLogger(), Sessions: sessions, Web: web.Handler(sessions, ""),
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + ReservedPrefix + "/definitely/not/a/route")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}
```

- [ ] **Step 2: Run the whole suite**

Run: `gofmt -l . && go vet ./... && go test ./... 2>&1 | tail -20`
Expected: all clean.

- [ ] **Step 3: Check the TUI's `o` key**

The README says `o` opens a dashboard URL that nothing serves. Confirm the URL it builds is `http://<listen addr>/_aiproxy/` and fix it if not — this is the moment that key starts working, and it has been a dangling promise since stage 4.

- [ ] **Step 4: Update the README**

Replace the stage table's row 5 with `done`, delete the "Until stage 5 lands…" note, and add a Dashboard section covering: what it is, how to reach it (`http://127.0.0.1:3456/_aiproxy/`), signing in with the proxy API key, and — prominently — **running it on a server**: set `listen.apiKey`, bind to a reachable address, and understand that `--insecure-no-auth` gives away the whole control plane. Say plainly that adding an account remotely uses the paste-the-code flow because the OAuth callback cannot reach a browser on another machine.

- [ ] **Step 5: Mark stage 5 done in the design doc**

Update §13's delivery order so the original design reflects reality, and add a pointer from §9 to the stage-5 spec.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/ internal/tui/ README.md docs/
git commit -m "test(web): dashboard end to end; document stage 5"
```

---

## Self-Review

**Spec coverage.** Every section maps to a task:

| Spec | Task |
|---|---|
| §2 no new query endpoints; existing routes | 9–14 (all consume existing routes) |
| §3 architecture, file layout, mount, hash routing | 5, 6 |
| §3 `internal/web` imports only `view` | 5 (step 6) |
| §4 bind guard | 4 |
| §4 session endpoint | 3 |
| §4 `Authorized` third path | 2 |
| §4 key never reaches JS beyond sign-in | 6 (fetch layer) |
| §5 Overview / Activity / Usage / Accounts / Settings | 9, 10, 11, 12, 14 |
| §5 three explicit states per screen | 6 (router), 9–14 |
| §6 chart primitives, colour, greyscale-readable | 7, 8 |
| §7 SSE ownership, reconnect, visible-tab polling | 6, 10 |
| §8 paste-the-code login | 13 |
| §9 verbatim error messages, 401 → sign-in | 6 |
| §9 `<noscript>` | 5 |
| §10 Go-side tests | 1, 2, 3, 4, 5, 15 |
| §10 JS-side chart tests via node, skipped when absent | 7 |
| §10 end-to-end | 15 |
| §11 sequencing so something ships early | task order |

**One gap found and closed while reviewing:** §5 requires the Overview to expose the pin control, and §2's route table lists `POST .../accounts/pin`, but no task mentioned it — Task 9 now does, including the empty-id clear.

**Placeholder scan:** no "TBD", no "add error handling", no "similar to Task N". Tasks 9–14 describe screens in prose plus exact endpoints and field names rather than full JS listings — the DOM helper, chart marks and fetch layer they compose from are given in full in Tasks 6–8, and repeating a hundred lines of `el(...)` per screen would obscure the requirements rather than clarify them.

**Type consistency:** `el`, `api`, `ApiError`, `registerScreen`, `startPolling`, `subscribeEvents`, `recentEvents`, `navigate`, `render`, `boot` are defined in Task 6 and used under those names throughout. `linearScale`, `timeScale`, `niceTicks`, `timeTicks`, `stack`, `colorFor`, `formatTokens`, `formatCostMicros`, `formatMs`, `formatPercent` are defined in Task 7; `stackedArea`, `lineChart`, `barChart`, `gauge`, `legend`, `emptyChart` in Task 8. `Sessions`/`NewSessions`/`Mint`/`Valid`/`Delete`/`Count` (Task 1) match `SignInHandler`'s use (Task 3) and `web.Handler`'s (Task 5). `proxy.sessionCookie` and `web.CookieName` are pinned equal by a test in Task 3. `renderAccountCard` is produced by Task 9 and consumed by Task 12.
