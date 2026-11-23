# Long-Lived Anthropic Login Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in long-lived (one-year) Anthropic login that mirrors `claude setup-token`, with a visible toggle in the web dashboard and TUI.

**Architecture:** Extend the existing PKCE login with `provider.LoginOptions{LongLived}`. The anthropic provider requests scope `user:inference user:profile` and adds `expires_in: 31536000` to the code exchange; the resulting credential is marked `LongLived`, stores no refresh token, and is never refreshed. The mode is carried through `view.Source.Login`, the control API, the web dialog, and the TUI. Account views expose only the expiry and marker for visibility.

**Tech Stack:** Go 1.26, Bubble Tea TUI, vanilla JS dashboard, SQLite metrics, existing GitHub release workflow.

**Spec:** `docs/superpowers/specs/2026-11-23-long-lived-login-design.md`

## Global Constraints

- Long-lived authorize scope is exactly `user:inference user:profile`.
- Long-lived code exchange sends exactly `"expires_in": 31536000`.
- A long-lived credential stores **no refresh token**, even if the response includes one, and is never refreshed. Renewal is a fresh login.
- Standard Anthropic login requests, exchange body, refresh body, and credential are byte-for-byte unchanged.
- Long-lived is Anthropic-only; OpenAI returns `provider.ErrUnsupported`.
- No new dependencies. No logging from `internal/provider/anthropic`.
- Tokens never cross the presentation seam; only `LongLived` and `ExpiresAt` metadata do.
- Release is `v0.6.0` through the existing tag-triggered workflow.

## Review Focus

- A toggle restart while the previous flow's `Done` arrives must not corrupt the new login pane (TUI sequence guard, web generation guard).
- A long-lived credential hitting 401/expiry must be sidelined in one attempt with a re-login message, not retried forever.
- If the server ignores `expires_in`, the login still succeeds, the account shows the short expiry, and a warning is logged.
- If the exchange returns a refresh token anyway, it must not be persisted or used.
- Closing/toggling the web dialog must cancel the pending session so no loopback listener or registry entry leaks.
- A long-lived account must still read profile and quota, because `user:profile` is retained.

---

### Task 1: Provider seam — login options and credential marker

**Files:**
- Modify: `internal/provider/provider.go:60-76` (`Credential`), `:280-308` (`Provider`)
- Modify: `internal/provider/openai/login.go:146`
- Test: `internal/provider/openai/login_test.go`
- Compile-fix: `internal/prober/prober_test.go:82`, `internal/proxy/models_test.go:214`, `internal/view/local_test.go:124,148`, `internal/account/manager_test.go:69`, plus every `Login(context.Context)` stub in tests

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  type LoginOptions struct{ LongLived bool }
  type Credential struct { /* existing fields */ LongLived bool `json:"longLived,omitempty"` }
  Login(ctx context.Context, opts LoginOptions) (LoginSession, error)
  ```

- [ ] **Step 1: Write the failing test in `internal/provider/openai/login_test.go`**

```go
func TestLoginRejectsLongLived(t *testing.T) {
	_, err := New(http.DefaultClient).Login(context.Background(), provider.LoginOptions{LongLived: true})
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails to compile** (`Login` takes one argument today)

Run: `go test ./internal/provider/openai -run TestLoginRejectsLongLived`

- [ ] **Step 3: Implement**

Add `LoginOptions` beside `Credential`; add `LongLived` to `Credential`; change `Provider.Login` to take `opts LoginOptions`. In `openai.Login`, return `fmt.Errorf("%w: long-lived login", provider.ErrUnsupported)` when `opts.LongLived`. Add `opts provider.LoginOptions` to every other implementation/stub, ignoring it.

- [ ] **Step 4: Run tests**

Run: `go test ./...` — expect PASS (the new test plus all existing suites compile and pass).

- [ ] **Step 5: Commit**

```bash
git add internal/provider internal/prober internal/proxy internal/view internal/account
git commit -m "feat(provider): add login options and long-lived credential marker"
```

---

### Task 2: Anthropic long-lived login and refresh refusal

**Files:**
- Modify: `internal/provider/anthropic/oauth.go:15-21` (constants), `:76-102` (`RefreshToken` caller is `Refresh`), `internal/provider/anthropic/anthropic.go:70-75`
- Modify: `internal/provider/anthropic/login.go:127-212` (`Login`), `:190-194` (`loginFlow`), `:408-435` (`complete`), `:440-498` (`exchange`)
- Test: `internal/provider/anthropic/login_test.go`, `internal/provider/anthropic/oauth_test.go`

**Interfaces:**
- Consumes: `provider.LoginOptions`, `provider.Credential.LongLived` from Task 1.
- Produces:
  ```go
  const LongLivedScopes = "user:inference user:profile"
  const LongLivedTokenTTLSeconds int64 = 31536000
  func (a *Anthropic) Login(ctx context.Context, opts provider.LoginOptions) (provider.LoginSession, error)
  ```
  `loginFlow` gains `longLived bool`; a long-lived credential is `{LongLived: true, RefreshToken: ""}`. `Anthropic.Refresh` returns `ErrRefreshRejected` for `c.LongLived || c.RefreshToken == ""` without a network call.

- [ ] **Step 1: Write the failing tests**

In `login_test.go`:

```go
func TestLoginLongLivedUsesInferenceAndProfileScope(t *testing.T) {
	up := loginUpstream(t, "at", "rt")
	p := newLoginProvider(t, up)
	sess, err := p.Login(context.Background(), provider.LoginOptions{LongLived: true})
	if err != nil { t.Fatalf("Login: %v", err) }
	defer sess.Cancel()
	u, _ := url.Parse(sess.URL)
	if got := u.Query().Get("scope"); got != LongLivedScopes {
		t.Errorf("scope = %q, want %q", got, LongLivedScopes)
	}
}

func TestLoginLongLivedRequestsOneYearAndDropsRefreshToken(t *testing.T) {
	up := loginUpstream(t, "at", "rt-should-be-dropped")
	p := newLoginProvider(t, up)
	var got provider.Credential
	var gotProfile provider.Profile
	p.OnLoginSuccess = func(_ context.Context, c provider.Credential, pr provider.Profile) error {
		got, gotProfile = c, pr
		return nil
	}
	sess, err := p.Login(context.Background(), provider.LoginOptions{LongLived: true})
	if err != nil { t.Fatalf("Login: %v", err) }
	state, redirectURI := sessionParams(t, sess.URL)
	res, _ := http.Get(redirectURI + "?code=c&state=" + state)
	res.Body.Close()
	if r := awaitResult(t, sess.Done); r.Err != nil { t.Fatalf("Err = %v", r.Err) }
	if !got.LongLived || got.RefreshToken != "" {
		t.Errorf("credential = %+v, want LongLived with no refresh token", got)
	}
	if gotProfile.Email != "a@example.com" {
		t.Errorf("profile = %+v, want the profile read to still happen", gotProfile)
	}
	var sent map[string]any
	json.Unmarshal(up.Requests()[0].Body, &sent)
	if sent["expires_in"] != float64(LongLivedTokenTTLSeconds) {
		t.Errorf("expires_in = %v, want %d", sent["expires_in"], LongLivedTokenTTLSeconds)
	}
}
```

In `oauth_test.go`:

```go
func TestRefreshRefusesLongLivedCredential(t *testing.T) {
	p := New(http.DefaultClient)
	_, err := p.Refresh(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at", LongLived: true,
	})
	if !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("err = %v, want ErrRefreshRejected", err)
	}
}
```

- [ ] **Step 2: Run and confirm failures**

Run: `go test ./internal/provider/anthropic -run 'LongLived|RefreshRefuses'`
Expected: compile failure on the two-argument `Login`; `RefreshRefuses` fails until implemented.

- [ ] **Step 3: Implement**

- Add the two constants.
- `Login`: `scope := Scopes; if opts.LongLived { scope = LongLivedScopes }`; set `longLived: opts.LongLived` on `loginFlow`; use `scope` in the authorize query.
- `exchange`: change the payload to `map[string]any` with the existing string fields, plus `payload["expires_in"] = LongLivedTokenTTLSeconds` when `f.longLived`.
- After decoding: `cred.LongLived = f.longLived`; when `f.longLived`, force `cred.RefreshToken = ""`.
- `Refresh`: `if c.LongLived || c.RefreshToken == "" { return provider.Credential{}, ErrRefreshRejected }`.
- Keep the existing profile read and `OnLoginSuccess` path untouched.

- [ ] **Step 4: Run the package**

Run: `go test ./internal/provider/anthropic`
Expected: PASS, including the existing byte-for-byte standard authorize-URL test.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/anthropic
git commit -m "feat(anthropic): long-lived one-year login mode"
```

---

### Task 3: Manager sidelines a long-lived credential instead of refreshing it

**Files:**
- Modify: `internal/account/manager.go:160-165`
- Test: `internal/account/manager_test.go`

**Interfaces:**
- Consumes: `provider.Credential.LongLived`, `provider.ErrCredentialRejected`.
- Produces: no new API. New behaviour: `EnsureFresh(force=true)` on a long-lived OAuth credential with no refresh token sets `StatusErrored`, `LastError` containing `long-lived token cannot be refreshed`, and returns an error wrapping `provider.ErrCredentialRejected`. Non-forced calls still return nil.

- [ ] **Step 1: Write the failing test**

```go
func TestEnsureFreshForceSidelinesLongLivedCredential(t *testing.T) {
	p := &stubProvider{}
	m := New([]config.Account{{
		ID: "a1", Provider: "stub", Label: "one",
		Credential: provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at", LongLived: true},
	}}, map[string]provider.Provider{"stub": p}, Options{})
	if err := m.EnsureFresh(context.Background(), "a1", true); !errors.Is(err, provider.ErrCredentialRejected) {
		t.Fatalf("err = %v, want ErrCredentialRejected", err)
	}
	if n := p.refreshes.Load(); n != 0 {
		t.Errorf("provider.Refresh called %d times, want 0", n)
	}
	a, _ := m.Get("a1")
	if a.Status != StatusErrored || !strings.Contains(a.LastError, "log in again") {
		t.Errorf("account = %+v, want errored with re-login message", a)
	}
	if err := m.UpdateCredential("a1", provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at2", LongLived: true}, config.Identity{}, "one"); err != nil {
		t.Fatal(err)
	}
	if a, _ := m.Get("a1"); a.Status != StatusActive || a.LastError != "" {
		t.Errorf("after re-login account = %+v, want active", a)
	}
}
```

Uses the existing `stubProvider` (its `refreshes atomic.Int32` already counts calls).

- [ ] **Step 2: Run and confirm it fails**

Run: `go test ./internal/account -run TestEnsureFreshForceSidelinesLongLivedCredential`
Expected: FAIL — the account stays active and nil is returned.

- [ ] **Step 3: Implement**

In `EnsureFresh`, replace the early return with:

```go
if a.Credential.Type != provider.CredentialOAuth || a.Credential.RefreshToken == "" {
	if force && a.Credential.LongLived {
		const msg = "long-lived token cannot be refreshed; log in again"
		a.Status = StatusErrored
		a.LastError = msg
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", provider.ErrCredentialRejected, msg)
	}
	m.mu.Unlock()
	return nil
}
```

- [ ] **Step 4: Run the package**

Run: `go test ./internal/account`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/account
git commit -m "feat(account): sideline expired long-lived tokens for re-login"
```

---

### Task 4: View seam — login options and account metadata

**Files:**
- Modify: `internal/view/source.go:90`
- Modify: `internal/view/local.go:733-739`, `:265-289`
- Modify: `internal/view/types.go:146-160`
- Compile-fix (forward a zero option for now): `internal/proxy/login_sessions.go:78`, `internal/proxy/control.go:373`, `internal/tui/login.go:70,97`, `internal/tui/behavior_test.go:333`, `internal/proxy/login_sessions_test.go:22`
- Test: `internal/view/local_test.go`

**Interfaces:**
- Consumes: `provider.LoginOptions`, `provider.Credential.LongLived`.
- Produces:
  ```go
  Login(ctx context.Context, providerName string, opts provider.LoginOptions) (provider.LoginSession, error)
  type Account struct { /* existing fields */ LongLived bool `json:"longLived,omitempty"`; ExpiresAt int64 `json:"expiresAt,omitempty"` }
  ```
  `loginSessionRegistry.begin(src, providerName, opts)` and `startLogin(providerName, longLived bool)` exist after this task, both forwarding the option; the API body and toggle come in Tasks 5 and 6.

- [ ] **Step 1: Write the failing tests**

Extend `loginableProvider` in `internal/view/local_test.go`:

```go
type loginableProvider struct {
	stubProvider
	session provider.LoginSession
	err     error
	gotOpts *provider.LoginOptions
}
func (l loginableProvider) Login(_ context.Context, opts provider.LoginOptions) (provider.LoginSession, error) {
	if l.gotOpts != nil { *l.gotOpts = opts }
	return l.session, l.err
}

func TestLoginPassesLongLivedOptionThrough(t *testing.T) {
	want := provider.LoginSession{URL: "https://example.invalid/authorize?state=xyz"}
	var got provider.LoginOptions
	providers := map[string]provider.Provider{
		"stub":      stubProvider{},
		"loginable": loginableProvider{session: want, gotOpts: &got},
	}
	h := newHarnessWithProviders(t, nil, providers)
	if _, err := h.local.Login(context.Background(), "loginable", provider.LoginOptions{LongLived: true}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !got.LongLived { t.Error("LongLived did not reach the provider") }
}

func TestAccountsExposeLongLivedMetadataWithoutTokens(t *testing.T) {
	const access, refresh = "sekrit-access-token", "sekrit-refresh-token"
	h := newHarness(t, config.Account{
		ID: "a", Provider: "stub", Label: "a",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: access, RefreshToken: refresh,
			LongLived: true, ExpiresAt: 1234,
		},
	})
	accts, err := h.local.Accounts(context.Background())
	if err != nil || len(accts) != 1 { t.Fatalf("Accounts = %+v, %v", accts, err) }
	if !accts[0].LongLived || accts[0].ExpiresAt != 1234 {
		t.Errorf("account = %+v, want long-lived metadata", accts[0])
	}
	b, _ := json.Marshal(accts[0])
	if bytes.Contains(b, []byte(access)) || bytes.Contains(b, []byte(refresh)) {
		t.Errorf("account JSON leaked a token: %s", b)
	}
}
```

- [ ] **Step 2: Run and confirm failures**

Run: `go test ./internal/view -run 'LongLived|LoginPasses'`
Expected: compile failure on `Source.Login`/`Local.Login` arity and missing `Account` fields.

- [ ] **Step 3: Implement**

Change the interface and `Local.Login` to pass `opts` to `p.Login`. Add the two fields to `view.Account` and map them in `Local.Accounts` from `a.Credential`. Update every `Source`/`Provider` stub in tests. Make the compile-fix files forward a zero `provider.LoginOptions{}` / `longLived=false` so the whole tree builds; Tasks 5 and 6 replace those zeros with real input.

- [ ] **Step 4: Run the packages**

Run: `go test ./internal/view ./internal/proxy ./internal/tui ./internal/account`
Expected: PASS with the zero-option forwarding in place.

- [ ] **Step 5: Commit**

```bash
git add internal/view internal/proxy internal/tui internal/account
git commit -m "feat(view): carry login options and long-lived account metadata"
```

---

### Task 5: Control API — longLived begin and session cancel

**Files:**
- Modify: `internal/proxy/control.go:354-385` (begin body/handler), add cancel handler
- Modify: `internal/proxy/login_sessions.go:78-120` (`begin`), add `cancel`
- Modify: `internal/proxy/router.go:53-57`
- Test: `internal/proxy/login_sessions_test.go`, `internal/proxy/control_api_test.go`

**Interfaces:**
- Consumes: `view.Source.Login(ctx, name, provider.LoginOptions)` from Task 4.
- Produces:
  - `POST /accounts/login` accepts `{"provider":"anthropic","longLived":true}`.
  - `DELETE /accounts/login/{sessionId}` cancels and releases a pending session; 204 on success, 404 when unknown.
  - `loginSessionRegistry.begin(src, providerName, opts provider.LoginOptions)` and `loginSessionRegistry.cancel(id string) bool`.

- [ ] **Step 1: Write the failing tests**

In `control_api_test.go`, using the real harness and fake upstream:

```go
func TestControlAPILoginBeginPassesLongLived(t *testing.T) {
	// POST {"provider":"anthropic","longLived":true}
	// parse begin.URL and assert scope == anthropic.LongLivedScopes
}
```

In `login_sessions_test.go`:

```go
func TestLoginSessionRegistryCancelCallsCancelAndReleases(t *testing.T) {
	// fakeLoginSource whose session has a Cancel func that closes a channel
	// reg.begin(...) then reg.cancel(id) == true, cancel ran, second cancel == false
}
```

- [ ] **Step 2: Run and confirm failures**

Run: `go test ./internal/proxy -run 'LoginBeginPassesLongLived|RegistryCancel'`
Expected: FAIL — unknown field is ignored / no cancel method.

- [ ] **Step 3: Implement**

Add `LongLived bool json:"longLived"` to `loginBeginBody`; pass `provider.LoginOptions{LongLived: body.LongLived}` through `reg.begin`. Add `cancel`:

```go
func (r *loginSessionRegistry) cancel(id string) bool {
	r.mu.Lock()
	st, ok := r.sessions[id]
	if ok { delete(r.sessions, id) }
	r.mu.Unlock()
	if !ok { return false }
	if st.sess.Cancel != nil { st.sess.Cancel() }
	return true
}
```

Add `loginCancelHandler` and `cp.Delete("/api/v1/accounts/login/{sessionId}", loginCancelHandler(o, loginSessions))`.

- [ ] **Step 4: Run the package**

Run: `go test ./internal/proxy`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy
git commit -m "feat(control): long-lived login flag and session cancel"
```

---

### Task 6: TUI toggle, stale-message guard, account detail

**Files:**
- Modify: `internal/tui/app.go:62-103` (Model gains `loginSeq int`)
- Modify: `internal/tui/login.go` (state, messages, start, update, key handling, view)
- Modify: `internal/tui/accounts.go:325-345` (detail line)
- Test: `internal/tui/behavior_test.go`, update `internal/tui/testdata/login_*.golden` with `go test ./internal/tui -run TestFrames -update`

**Interfaces:**
- Consumes: `provider.LoginOptions` and `view.Account.LongLived/ExpiresAt`.
- Produces: `startLogin(providerName string, longLived bool)`; `ctrl+t` toggles Anthropic mode by cancelling and restarting; stale `loginDoneMsg` with a different `seq` is ignored.

- [ ] **Step 1: Write the failing tests**

```go
func TestLoginCtrlTTogglesLongLivedAndRestarts(t *testing.T) {
	rec := &loginRecordingSource{}
	m := fixtureModel(120, 28)
	m.src = rec

	next, cmd := m.startLogin("anthropic", false)
	m = next.(Model)
	cmd() // records the first options synchronously
	if rec.lastOpts.LongLived {
		t.Fatal("first start should be standard")
	}

	next, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlT})
	m = next.(Model)
	if !m.login.active || !m.login.longLived {
		t.Fatalf("login = %+v, want a restarted long-lived flow", m.login)
	}
	cmd() // records the restarted options
	if !rec.lastOpts.LongLived {
		t.Error("ctrl+t did not restart with LongLived")
	}
	if !strings.Contains(m.View(), "long-lived") {
		t.Error("login pane does not say it is long-lived")
	}
}

func TestLoginIgnoresStaleDoneFromCancelledFlow(t *testing.T) {
	m := fixtureModel(120, 28)
	m.loginSeq = 2
	m.login = loginState{active: true, seq: 2}
	m, _ = m.updateLogin(loginDoneMsg{ok: true, seq: 1, res: view.LoginResult{Err: errors.New("oauth: login cancelled")}})
	if m.login.err != "" {
		t.Errorf("stale done set err = %q", m.login.err)
	}
}
```

Extend `loginRecordingSource` with `lastOpts provider.LoginOptions`, record it in `Login`, and update its signature to the new `Source.Login`. Update the existing `startLogin("anthropic")` call sites in `behavior_test.go` and `frames_test.go` to pass `false`.

- [ ] **Step 2: Run and confirm failures**

Run: `go test ./internal/tui -run 'CtrlT|StaleDone'`
Expected: FAIL / compile failure.

- [ ] **Step 3: Implement**

- Add `loginSeq int` to `Model`; add `longLived bool` and `seq int` to `loginState`; add `seq int` to both login messages.
- `startLogin(providerName string, longLived bool)`: increment `m.loginSeq`, store it, pass `provider.LoginOptions{LongLived: longLived}` to `src.Login`, stamp messages with the seq.
- `updateLogin`: ignore `loginStartedMsg`/`loginDoneMsg` whose `seq != m.loginSeq`.
- `loginPickKey`: `m.startLogin(p.name, false)`.
- `loginKey`: on `ctrl+t`, only when `m.login.provider == "anthropic"`: cancel, clear state, `startLogin(provider, !longLived)`.
- `viewLogin`: title gains `· long-lived (1 year)` when on; footer line `ctrl+t long-lived: off/on` for Anthropic.
- Account detail: when `a.LongLived`, add `token: long-lived · expires YYYY-MM-DD` (omit the date when `ExpiresAt == 0`).

- [ ] **Step 4: Run and update goldens**

Run: `go test ./internal/tui -run TestFrames -update && go test ./internal/tui`
Expected: PASS; review the changed login goldens to confirm the hint reads correctly.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): long-lived login toggle and expiry display"
```

---

### Task 7: Web dashboard toggle and account card

**Files:**
- Modify: `internal/web/assets/screens/login.js`
- Modify: `internal/web/assets/screens/overview.js:27-56`

**Interfaces:**
- Consumes: control API `longLived` body field and `DELETE /accounts/login/{id}`.
- Produces: a "Long-lived (1 year)" checkbox on the Anthropic login screen that cancels the current session and restarts with the new mode; a `token` fact on long-lived account cards.

- [ ] **Step 1: Implement `login.js`**

Add closure state `gen = 0`, `mode = false`, `sessionId`. Add `cancelSession()` calling `api('/accounts/login/'+sessionId, {method:'DELETE'}).catch(() => {})`. `begin(provider, longLived)` captures `const my = ++gen`, stops the poll, cancels the old session, posts `{provider, longLived}`; after every await, return early if `my !== gen`. Render, for Anthropic only:

```js
const toggle = el("input", { type: "checkbox", checked: longLived });
toggle.addEventListener("change", () => begin(provider, toggle.checked));
body.append(el("label", { class: "small muted" }, toggle, " Long-lived (1 year)"));
```

Poll captures `my` and stops if stale. Close handler cancels the pending session before removing the dialog. Set `sessionId = null` on `done`.

- [ ] **Step 2: Implement `overview.js`**

Build the facts as an array; when `a.longLived`, append:

```js
el("dt", {}, "token"),
el("dd", {}, "1-year" + (a.expiresAt ? " · expires " + new Date(a.expiresAt).toISOString().slice(0, 10) : ""))
```

- [ ] **Step 3: Verify**

Run: `go test ./internal/web ./internal/proxy` (embedded assets compile and control routes pass).
Manual: `go build -o /tmp/aiproxy ./cmd/aiproxy`, run with a temp config, open the dashboard, check the toggle restarts the URL and that a long-lived account card shows the token fact.

- [ ] **Step 4: Commit**

```bash
git add internal/web/assets/screens/login.js internal/web/assets/screens/overview.js
git commit -m "feat(web): long-lived login toggle and token expiry fact"
```

---

### Task 8: Login downgrade warning and docs

**Files:**
- Modify: `cmd/aiproxy/accounts.go:32-55`, `cmd/aiproxy/main.go:382-388`
- Test: `cmd/aiproxy/accounts_test.go`
- Modify: `README.md` (quick start, TUI keys, remote login paragraph, control API list)

**Interfaces:**
- Consumes: `provider.Credential.LongLived/ExpiresAt`.
- Produces: `onLoginSuccess(store *config.Store, mgr *account.Manager, providerName string, log *slog.Logger)`; warns when a long-lived result expires in under 300 days.

- [ ] **Step 1: Write the failing test**

```go
func TestOnLoginSuccessWarnsWhenLongLivedTokenIsShort(t *testing.T) {
	var buf bytes.Buffer
	hook := onLoginSuccess(store, mgr, "anthropic", slog.New(slog.NewTextHandler(&buf, nil)))
	err := hook(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at", LongLived: true,
		ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
	}, provider.Profile{Email: "a@example.com", AccountUUID: "u1"})
	if err != nil { t.Fatal(err) }
	if !strings.Contains(buf.String(), "shorter") { t.Errorf("log = %q, want a downgrade warning", buf.String()) }
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./cmd/aiproxy -run TestOnLoginSuccessWarns`
Expected: compile failure / no warning.

- [ ] **Step 3: Implement**

Add the `log *slog.Logger` parameter (fallback `slog.Default()` when nil); warn before persisting when `cred.LongLived && cred.ExpiresAt != 0 && cred.ExpiresAt < time.Now().Add(300*24*time.Hour).UnixMilli()`. Update both main.go call sites to pass `log`.

README:
- Quick start: mention that the Anthropic sign-in has a "Long-lived (1 year)" toggle, that it is inference + profile only, and that it must be re-authorised when it expires.
- TUI: `ctrl+t` toggles long-lived on the Anthropic login pane.
- Remote add: mention ticking the long-lived box before pasting the code.
- Control API list: add `DELETE /_aiproxy/api/v1/accounts/login/{sessionId}`.

- [ ] **Step 4: Run the package**

Run: `go test ./cmd/aiproxy`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/aiproxy README.md
git commit -m "feat(login): warn on short long-lived tokens; document the mode"
```

---

### Task 9: Full verification

- [ ] **Step 1: Format and vet**

```bash
gofmt -l .            # must print nothing
go vet ./...
staticcheck ./...
```

- [ ] **Step 2: Full race suite**

```bash
go test -race ./...
```
Expected: PASS.

- [ ] **Step 3: Cross-build**

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/aiproxy-darwin ./cmd/aiproxy
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -o /tmp/aiproxy-linux  ./cmd/aiproxy
```

- [ ] **Step 4: Fix anything that fails, commit the fixes, and rerun.**

---

### Task 10: Ship `v0.6.0`

**Files:** none.

- [ ] **Step 1: Merge to main**

```bash
git checkout main
git merge --no-ff <branch> -m "merge: long-lived Anthropic login"
```

- [ ] **Step 2: Tag and push**

```bash
git push origin main
git tag v0.6.0
git push origin v0.6.0
```

- [ ] **Step 3: Watch the release workflow**

```bash
gh run watch --exit-status      # the run triggered by the tag push
gh release view v0.6.0
```

Expected: four tarballs and `checksums.txt`, all published.

- [ ] **Step 4: Verify the published binary**

```bash
curl -fsSL https://raw.githubusercontent.com/nicko170/aiproxy/main/install.sh | AIPROXY_VERSION=0.6.0 BINDIR=/tmp/aiproxy-bin sh
/tmp/aiproxy-bin/aiproxy --version   # 0.6.0
```

Report the release URL and the toggle's location for the server test.
