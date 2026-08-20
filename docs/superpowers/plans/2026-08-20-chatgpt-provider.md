# ChatGPT Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run Codex through aiproxy on ChatGPT subscriptions with the same rotation, quota tracking and ranking Anthropic accounts already get, plus a synthetic `/v1/models` listing every model any logged-in account can reach.

**Architecture:** A second provider (`internal/provider/openai`) behind the existing `provider.Provider` interface, which needs no change. A new `Models()` method on that interface backs a per-account model catalogue held in `account.Manager` beside `Buckets`; selection gains one eligibility filter so routing follows discovered access rather than a name table. A synthetic `/v1/models` handler unions the catalogues.

**Tech Stack:** Go 1.26.5, stdlib only (no new dependencies). OAuth 2.0 + PKCE S256.

**Spec:** `docs/superpowers/specs/2026-08-20-chatgpt-provider-design.md`

## Global Constraints

- `CGO_ENABLED=0` must keep working: no cgo, no new native dependencies. It is what makes one runner cross-compile four targets, and what `install.sh` and the self-updater depend on.
- No new third-party modules. `go.mod` gains nothing.
- The TUI imports only `internal/view`. Enforced by `TestTUIImportsOnlyTheViewSeam`.
- Every `view.Source` method has exactly one control route. Enforced by `TestEveryViewSourceMethodHasAControlRoute`. Adding a `Source` method means adding a route in the same task.
- `provider.OutcomeKind` values are persisted: append new kinds, never insert.
- Credentials never appear in a log line, an error string, or a control-API response.
- `gofmt`, `go vet` and `go test ./...` clean at every commit.
- OAuth client id: `app_EMoamEEZ73f0CkXaXp7hrann`. Issuer: `https://auth.openai.com`.
- ChatGPT `used_percent` is 0..100 and MUST be divided by 100. Anthropic's header is already a fraction and must NOT be. Both directions have been got wrong once in this repo.

---

### Task 1: OpenAI provider skeleton, Name and Endpoint

**Files:**
- Create: `internal/provider/openai/openai.go`
- Create: `internal/provider/openai/openai_test.go`

**Interfaces:**
- Consumes: `provider.Provider`, `provider.Account`, `provider.Credential` from `internal/provider`.
- Produces: `openai.New(hc *http.Client) *OpenAI`; fields `TokenEndpointOverride`, `BaseURLOverride`, `ChatGPTBaseURLOverride`, `ClientVersion string`; methods `Name() string`, `Endpoint(provider.Account) *url.URL`.

- [ ] **Step 1: Write the failing test**

```go
package openai

import (
	"net/http"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestNameIsOpenAI(t *testing.T) {
	if got := New(http.DefaultClient).Name(); got != "openai" {
		t.Errorf("Name() = %q, want openai", got)
	}
}

// The account's Upstream override wins so a test (and an operator behind a
// gateway) can point one account somewhere else without touching the others.
func TestEndpointPrefersTheAccountOverride(t *testing.T) {
	o := New(http.DefaultClient)
	if got := o.Endpoint(provider.Account{}).String(); got != "https://api.openai.com/v1" {
		t.Errorf("default endpoint = %q", got)
	}
	got := o.Endpoint(provider.Account{Upstream: "http://127.0.0.1:9/v1"}).String()
	if got != "http://127.0.0.1:9/v1" {
		t.Errorf("override endpoint = %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/openai/ -run 'TestNameIsOpenAI|TestEndpointPrefers' -v`
Expected: FAIL — build error, `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package openai adapts ChatGPT subscription accounts to the proxy core.
//
// Inference goes to the Responses API on api.openai.com. Quota and the model
// catalogue come from chatgpt.com/backend-api, which is a different host and a
// private one — see Quota and Models for what happens when it moves.
package openai

import (
	"net/http"
	"net/url"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

const (
	defaultAPIBase     = "https://api.openai.com/v1"
	defaultChatGPTBase = "https://chatgpt.com/backend-api"
	defaultIssuer      = "https://auth.openai.com"

	// clientID is Codex CLI's public OAuth client. A public client has no
	// secret; PKCE is what binds the code to this flow.
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// originator identifies the caller to the Responses API. Codex sends this
	// and the backend is known to gate on it, so it is sent verbatim.
	originator = "codex_cli_rs"

	// defaultClientVersion is required by the wham/models endpoint, which
	// rejects a request without it. Overridable because a server-side version
	// gate is a plausible way for this to break.
	defaultClientVersion = "0.147.0"
)

type OpenAI struct {
	hc *http.Client

	// TokenEndpointOverride redirects the OAuth token endpoint in tests.
	TokenEndpointOverride string
	// BaseURLOverride redirects the Responses API in tests.
	BaseURLOverride string
	// ChatGPTBaseURLOverride redirects quota and model reads in tests.
	ChatGPTBaseURLOverride string
	// ClientVersion is sent to wham/models. Empty takes defaultClientVersion.
	ClientVersion string
	// LoginTimeoutOverride redirects Login's end-to-end timeout in tests.
	LoginTimeoutOverride time.Duration
	// OnLoginSuccess mirrors anthropic.OnLoginSuccess: called once, before the
	// LoginResult is sent, so cmd/aiproxy can persist the account.
	OnLoginSuccess func(provider.Credential, provider.Profile)
}

func New(hc *http.Client) *OpenAI {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &OpenAI{hc: hc}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) apiBase() string {
	if o.BaseURLOverride != "" {
		return o.BaseURLOverride
	}
	return defaultAPIBase
}

func (o *OpenAI) chatgptBase() string {
	if o.ChatGPTBaseURLOverride != "" {
		return o.ChatGPTBaseURLOverride
	}
	return defaultChatGPTBase
}

func (o *OpenAI) clientVersion() string {
	if o.ClientVersion != "" {
		return o.ClientVersion
	}
	return defaultClientVersion
}

func (o *OpenAI) Endpoint(a provider.Account) *url.URL {
	base := o.apiBase()
	if a.Upstream != "" {
		base = a.Upstream
	}
	u, err := url.Parse(base)
	if err != nil {
		u, _ = url.Parse(defaultAPIBase)
	}
	return u
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/provider/openai/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/openai/
git commit -m "feat(openai): provider skeleton with name and endpoint"
```

---

### Task 2: Authorize, RewriteBody, ParseUsage, ParseUsageBody

**Files:**
- Modify: `internal/provider/openai/openai.go`
- Modify: `internal/provider/openai/openai_test.go`

**Interfaces:**
- Consumes: `openai.OpenAI` from Task 1.
- Produces: `Authorize(*http.Request, provider.Credential)`, `RewriteBody([]byte, provider.Account) ([]byte, error)`, `ParseUsage([]byte) (*provider.UsageDelta, bool)`, `ParseUsageBody([]byte) (*provider.UsageDelta, bool)`.

**Ordering note.** ChatGPT requires a `chatgpt-account-id` header on every call, but the interface hands `Authorize` only a `Credential`, which has no such field today. Task 3 adds `Credential.AccountID` and sets the header. In THIS task `Authorize` sets the bearer and `originator` only; the header test lives in Task 3, so do not write it here.

- [ ] **Step 1: Write the failing test**

```go
func TestAuthorizeSetsBearerAndClearsForeignAuth(t *testing.T) {
	o := New(http.DefaultClient)
	r, _ := http.NewRequest("POST", "http://x/v1/responses", nil)
	r.Header.Set("x-api-key", "leaked-from-client")
	o.Authorize(r, provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})

	if got := r.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization = %q", got)
	}
	if r.Header.Get("x-api-key") != "" {
		t.Error("a client's own x-api-key must never reach upstream")
	}
	if got := r.Header.Get("originator"); got != "codex_cli_rs" {
		t.Errorf("originator = %q, want codex_cli_rs", got)
	}
}

// Usage arrives on the terminal response.completed SSE event, not per-delta.
func TestParseUsageReadsResponseCompleted(t *testing.T) {
	o := New(http.DefaultClient)
	ev := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":4}}}}`)
	got, ok := o.ParseUsage(ev)
	if !ok {
		t.Fatal("ParseUsage returned !ok for a response.completed event")
	}
	if got.InputTokens != 11 || got.OutputTokens != 7 || got.CacheReadTokens != 4 {
		t.Errorf("usage = %+v, want in=11 out=7 cacheRead=4", got)
	}
}

// Anything that is not the terminal event carries no usage, and reporting a
// zero delta as if it were real is how token accounting silently reads free.
func TestParseUsageIgnoresOtherEvents(t *testing.T) {
	o := New(http.DefaultClient)
	if _, ok := o.ParseUsage([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)); ok {
		t.Error("a text delta must not report usage")
	}
}

func TestParseUsageBodyReadsNonStreamingResponse(t *testing.T) {
	o := New(http.DefaultClient)
	body := []byte(`{"usage":{"input_tokens":3,"output_tokens":5}}`)
	got, ok := o.ParseUsageBody(body)
	if !ok || got.InputTokens != 3 || got.OutputTokens != 5 {
		t.Errorf("got %+v ok=%v, want in=3 out=5", got, ok)
	}
}

func TestRewriteBodyAppliesTheAccountModelMap(t *testing.T) {
	o := New(http.DefaultClient)
	in := []byte(`{"model":"proxy-fast","input":"hi"}`)
	got, err := o.RewriteBody(in, provider.Account{ModelMap: map[string]string{"proxy-fast": "gpt-5.4-mini"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"gpt-5.4-mini"`) {
		t.Errorf("body = %s, want the mapped model", got)
	}
	if !strings.Contains(string(got), `"input":"hi"`) {
		t.Errorf("body = %s, want the rest of the body preserved", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/openai/ -v`
Expected: FAIL — `o.Authorize undefined`.

- [ ] **Step 3: Write minimal implementation**

```go
func (o *OpenAI) Authorize(r *http.Request, c provider.Credential) {
	// Cleared, not overwritten: a client may have sent its own credentials to
	// the proxy and they must never travel upstream.
	r.Header.Del("Authorization")
	r.Header.Del("x-api-key")
	switch c.Type {
	case provider.CredentialAPIKey:
		r.Header.Set("Authorization", "Bearer "+c.APIKey)
	default:
		r.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}
	r.Header.Set("originator", originator)
}

// RewriteBody applies the account's model map. Only the "model" key is decoded;
// everything else stays raw bytes, so a large input array is never re-encoded.
func (o *OpenAI) RewriteBody(body []byte, a provider.Account) ([]byte, error) {
	if len(a.ModelMap) == 0 || len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, nil // not a JSON object: pass through untouched
	}
	raw, ok := top["model"]
	if !ok {
		return body, nil
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return body, nil
	}
	mapped, ok := a.ModelMap[model]
	if !ok || mapped == model {
		return body, nil
	}
	next, err := json.Marshal(mapped)
	if err != nil {
		return body, nil
	}
	top["model"] = next
	return json.Marshal(top)
}

type usageEnvelope struct {
	Type     string `json:"type"`
	Response *struct {
		Usage *responsesUsage `json:"usage"`
	} `json:"response"`
}

type responsesUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u *responsesUsage) delta() *provider.UsageDelta {
	d := &provider.UsageDelta{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputDetails != nil {
		d.CacheReadTokens = u.InputDetails.CachedTokens
	}
	return d
}

// ParseUsage reads the terminal response.completed event. Usage is reported
// once, at the end, rather than accumulating per delta — so every other event
// must report !ok. Returning a zero delta for them would be indistinguishable
// from a genuinely free request.
func (o *OpenAI) ParseUsage(sseEvent []byte) (*provider.UsageDelta, bool) {
	var env usageEnvelope
	if err := json.Unmarshal(sseEvent, &env); err != nil {
		return nil, false
	}
	if env.Type != "response.completed" || env.Response == nil || env.Response.Usage == nil {
		return nil, false
	}
	return env.Response.Usage.delta(), true
}

func (o *OpenAI) ParseUsageBody(body []byte) (*provider.UsageDelta, bool) {
	var top struct {
		Usage *responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &top); err != nil || top.Usage == nil {
		return nil, false
	}
	return top.Usage.delta(), true
}
```

Add `bytes`, `encoding/json` to imports; add `strings` to the test file.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/provider/openai/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/openai/
git commit -m "feat(openai): authorize, model map, and Responses usage parsing"
```

---

### Task 3: OAuth token exchange, refresh, and Profile from id_token claims

**Files:**
- Create: `internal/provider/openai/oauth.go`
- Create: `internal/provider/openai/oauth_test.go`
- Modify: `internal/provider/provider.go` (add `AccountID` to `Credential`)

**Interfaces:**
- Consumes: `openai.OpenAI` from Task 1.
- Produces: `Refresh(ctx, provider.Credential) (provider.Credential, error)`, `Profile(ctx, provider.Credential) (provider.Profile, error)`, `claimsFromJWT(token string) (authClaims, error)`, `authClaims{AccountID, PlanType, UserID, OrgID, Email string}`. `provider.Credential` gains `AccountID string \`json:"accountId,omitempty"\``.

- [ ] **Step 1: Write the failing test**

```go
package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

// jwt builds an unsigned JWT with the given claims. Nothing here verifies a
// signature — the token came from our own token endpoint over TLS, and this
// only reads claims we already trust.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

func idToken(t *testing.T) string {
	return jwt(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-123",
			"chatgpt_plan_type":  "plus",
			"chatgpt_user_id":    "user-abc",
			"poid":               "org-xyz",
		},
		"email": "someone@example.com",
	})
}

func TestProfileReadsIdentityFromTheIdToken(t *testing.T) {
	o := New(http.DefaultClient)
	got, err := o.Profile(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: idToken(t),
	})
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got.AccountUUID != "acc-123" || got.Plan != "plus" || got.OrgUUID != "org-xyz" {
		t.Errorf("profile = %+v", got)
	}
}

func TestRefreshExchangesTheRefreshToken(t *testing.T) {
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotForm = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"access_token":"` + idToken(t) + `","refresh_token":"rt2","expires_in":864000,"id_token":"` + idToken(t) + `"}`))
	}))
	defer srv.Close()

	o := New(http.DefaultClient)
	o.TokenEndpointOverride = srv.URL
	got, err := o.Refresh(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, RefreshToken: "rt1",
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !strings.Contains(gotForm, "grant_type=refresh_token") || !strings.Contains(gotForm, "rt1") {
		t.Errorf("form = %q", gotForm)
	}
	if got.RefreshToken != "rt2" {
		t.Errorf("refresh token not rotated: %q", got.RefreshToken)
	}
	if got.ExpiresAt == 0 {
		t.Error("ExpiresAt must be set from expires_in")
	}
	// The account id travels on the credential so Authorize can send the
	// chatgpt-account-id header without re-parsing a JWT on every request.
	if got.AccountID != "acc-123" {
		t.Errorf("AccountID = %q, want acc-123", got.AccountID)
	}
}

// A refused refresh must be distinguishable from a network failure: one
// sidelines the account, the other says nothing about the credential.
func TestRefreshWrapsCredentialRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	o := New(http.DefaultClient)
	o.TokenEndpointOverride = srv.URL
	_, err := o.Refresh(context.Background(), provider.Credential{RefreshToken: "dead"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, provider.ErrCredentialRejected) {
		t.Errorf("err = %v, want it to wrap ErrCredentialRejected", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/openai/ -run 'Profile|Refresh' -v`
Expected: FAIL — `o.Profile undefined`.

- [ ] **Step 3: Add AccountID to Credential**

In `internal/provider/provider.go`, inside `type Credential struct`, after `ExpiresAt`:

```go
	// AccountID is the provider-side account this credential belongs to, when
	// the provider needs it on the wire. ChatGPT requires it as the
	// chatgpt-account-id header on every Responses call; Anthropic has no
	// equivalent and leaves this empty.
	AccountID string `json:"accountId,omitempty"`
```

- [ ] **Step 4: Write the implementation**

Create `internal/provider/openai/oauth.go`:

```go
package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// authClaims is the subset of the id_token we read. The namespaced key is
// OpenAI's own convention for custom claims.
type authClaims struct {
	AccountID string
	PlanType  string
	UserID    string
	OrgID     string
	Email     string
}

// claimsFromJWT decodes the payload segment. The signature is NOT verified and
// deliberately so: this token came from our own token endpoint over TLS in the
// same process, and re-verifying it here would mean shipping a JWKS client to
// re-learn something already established by the transport.
func claimsFromJWT(token string) (authClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return authClaims{}, fmt.Errorf("openai: token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return authClaims{}, fmt.Errorf("openai: decode claims: %w", err)
	}
	var payload struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
			ChatGPTUserID    string `json:"chatgpt_user_id"`
			POID             string `json:"poid"`
		} `json:"https://api.openai.com/auth"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return authClaims{}, fmt.Errorf("openai: parse claims: %w", err)
	}
	return authClaims{
		AccountID: payload.Auth.ChatGPTAccountID,
		PlanType:  payload.Auth.ChatGPTPlanType,
		UserID:    payload.Auth.ChatGPTUserID,
		OrgID:     payload.Auth.POID,
		Email:     payload.Email,
	}, nil
}

func (o *OpenAI) tokenEndpoint() string {
	if o.TokenEndpointOverride != "" {
		return o.TokenEndpointOverride
	}
	return defaultIssuer + "/oauth/token"
}

func (o *OpenAI) Profile(_ context.Context, c provider.Credential) (provider.Profile, error) {
	cl, err := claimsFromJWT(c.AccessToken)
	if err != nil {
		return provider.Profile{}, err
	}
	return provider.Profile{
		AccountUUID: cl.AccountID,
		Email:       cl.Email,
		OrgUUID:     cl.OrgID,
		Plan:        cl.PlanType,
	}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// postToken performs a form-encoded token request and turns the result into a
// Credential. An HTTP 4xx is a REFUSAL and wraps ErrCredentialRejected, which
// is what lets account.Manager sideline the account; a transport failure says
// nothing about the credential and must not.
func (o *OpenAI) postToken(ctx context.Context, form url.Values) (provider.Credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenEndpoint(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return provider.Credential{}, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")

	res, err := o.hc.Do(req)
	if err != nil {
		return provider.Credential{}, fmt.Errorf("openai: token request: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode >= 400 && res.StatusCode < 500 {
		return provider.Credential{}, fmt.Errorf("%w: token endpoint %d",
			provider.ErrCredentialRejected, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		return provider.Credential{}, fmt.Errorf("openai: token endpoint %d", res.StatusCode)
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return provider.Credential{}, fmt.Errorf("openai: parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return provider.Credential{}, fmt.Errorf("openai: token response carried no access_token")
	}

	out := provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
	}
	if tr.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	}
	// Prefer the id_token for identity, falling back to the access token: both
	// carry the namespaced claim, and a refresh response may omit id_token.
	src := tr.IDToken
	if src == "" {
		src = tr.AccessToken
	}
	if cl, err := claimsFromJWT(src); err == nil {
		out.AccountID = cl.AccountID
	}
	return out, nil
}

func (o *OpenAI) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	if c.RefreshToken == "" {
		return provider.Credential{}, fmt.Errorf("%w: no refresh token", provider.ErrCredentialRejected)
	}
	next, err := o.postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {clientID},
	})
	if err != nil {
		return provider.Credential{}, err
	}
	// Upstream may omit a rotated refresh token; keeping the old one is correct
	// and dropping it would strand the account at the next expiry.
	if next.RefreshToken == "" {
		next.RefreshToken = c.RefreshToken
	}
	if next.AccountID == "" {
		next.AccountID = c.AccountID
	}
	return next, nil
}
```

Add `"errors"` to the test file imports.

- [ ] **Step 5: Send the account id header now that Credential carries it**

In `internal/provider/openai/openai.go`, at the end of `Authorize`:

```go
	if c.AccountID != "" {
		r.Header.Set("chatgpt-account-id", c.AccountID)
	}
```

Add to `openai_test.go`:

```go
func TestAuthorizeSetsTheAccountHeader(t *testing.T) {
	o := New(http.DefaultClient)
	r, _ := http.NewRequest("POST", "http://x/v1/responses", nil)
	o.Authorize(r, provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at", AccountID: "acc-1"})
	if got := r.Header.Get("chatgpt-account-id"); got != "acc-1" {
		t.Errorf("chatgpt-account-id = %q", got)
	}
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/provider/... -count=1`
Expected: PASS, including the existing anthropic tests (Credential gained a field; nothing reads it there).

- [ ] **Step 7: Commit**

```bash
git add internal/provider/
git commit -m "feat(openai): token refresh, profile from claims, account-id header"
```

---

### Task 4: Quota from wham/usage

**Files:**
- Create: `internal/provider/openai/usage.go`
- Create: `internal/provider/openai/usage_test.go`

**Interfaces:**
- Consumes: `openai.OpenAI`, `chatgptBase()`.
- Produces: `Quota(ctx, provider.Credential) (provider.Quota, error)`, `bucketName(windowSeconds int64) string`.

- [ ] **Step 1: Write the failing test**

The fixture is the verbatim payload captured from a live Plus account.

```go
package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

const liveUsagePayload = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true, "limit_reached": false,
    "primary_window": { "used_percent": 29, "limit_window_seconds": 604800,
                        "reset_after_seconds": 89259, "reset_at": 1787282195 },
    "secondary_window": null
  },
  "credits": { "has_credits": false, "unlimited": false, "balance": "0" },
  "rate_limit_reached_type": null
}`

func usageServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			t.Errorf("path = %q, want /wham/usage", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// used_percent is 0..100 here and MUST be divided by 100. The Anthropic header
// is already a fraction and must not be; this repo has now got that division
// wrong in both directions, so both directions are pinned by a test.
func TestQuotaConvertsPercentAndDerivesTheWindowName(t *testing.T) {
	srv := usageServer(t, 200, liveUsagePayload)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL

	got, err := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if len(got.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want exactly one (secondary is null)", got.Buckets)
	}
	b := got.Buckets[0]
	if b.Name != "7d" {
		t.Errorf("name = %q, want 7d derived from 604800 seconds", b.Name)
	}
	if b.Utilization != 0.29 {
		t.Errorf("utilization = %v, want 0.29 from used_percent 29", b.Utilization)
	}
	if b.ResetsAt != 1787282195000 {
		t.Errorf("resetsAt = %d, want unix seconds converted to millis", b.ResetsAt)
	}
	if b.Status != "" {
		t.Errorf("status = %q, want empty while allowed", b.Status)
	}
}

// A null secondary window must produce NO bucket. A zero-utilization bucket
// with no reset would make a spent account look idle to the ranking.
func TestQuotaOmitsANullSecondaryWindow(t *testing.T) {
	srv := usageServer(t, 200, liveUsagePayload)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL
	got, _ := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	for _, b := range got.Buckets {
		if b.ResetsAt == 0 {
			t.Errorf("bucket %q has no reset; it should not exist", b.Name)
		}
	}
}

func TestQuotaMarksRejectedWhenTheLimitIsReached(t *testing.T) {
	body := `{"rate_limit":{"allowed":false,"limit_reached":true,
	  "primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":1787282195}}}`
	srv := usageServer(t, 200, body)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL

	got, _ := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	if len(got.Buckets) != 1 || got.Buckets[0].Name != "5h" {
		t.Fatalf("buckets = %+v, want one named 5h", got.Buckets)
	}
	if got.Buckets[0].Status != "rejected" {
		t.Errorf("status = %q, want rejected", got.Buckets[0].Status)
	}
}

// The endpoint is private and undocumented. A 429 on it is throttling of the
// probe itself, which internal/prober already backs off on.
func TestQuotaReportsThrottling(t *testing.T) {
	srv := usageServer(t, 429, `{}`)
	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL
	_, err := o.Quota(context.Background(), provider.Credential{AccessToken: "at"})
	if !errors.Is(err, provider.ErrQuotaThrottled) {
		t.Errorf("err = %v, want ErrQuotaThrottled", err)
	}
}

func TestBucketName(t *testing.T) {
	for _, c := range []struct {
		secs int64
		want string
	}{
		{18000, "5h"}, {604800, "7d"}, {3600, "1h"}, {86400, "1d"}, {0, ""}, {90, "90s"},
	} {
		if got := bucketName(c.secs); got != c.want {
			t.Errorf("bucketName(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/openai/ -run Quota -v`
Expected: FAIL — `o.Quota undefined`.

- [ ] **Step 3: Write the implementation**

```go
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrQuotaThrottled wraps the seam-level sentinel so internal/prober can back
// off without importing this package, exactly as anthropic's does.
var ErrQuotaThrottled = fmt.Errorf("%w: chatgpt usage endpoint throttled", provider.ErrQuotaThrottled)

// bucketName turns a window length into the name the rest of the system uses.
// Deriving rather than hardcoding is what lets account.windowHours and the
// expiring-allowance ranking treat an OpenAI window exactly like an Anthropic
// one, with no provider-specific branch anywhere in selection.
func bucketName(windowSeconds int64) string {
	switch {
	case windowSeconds <= 0:
		return ""
	case windowSeconds%86400 == 0:
		return strconv.FormatInt(windowSeconds/86400, 10) + "d"
	case windowSeconds%3600 == 0:
		return strconv.FormatInt(windowSeconds/3600, 10) + "h"
	default:
		return strconv.FormatInt(windowSeconds, 10) + "s"
	}
}

type usageWindow struct {
	UsedPercent       float64 `json:"used_percent"`
	LimitWindowSeconds int64  `json:"limit_window_seconds"`
	ResetAt           int64   `json:"reset_at"`
}

type usageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         bool         `json:"allowed"`
		LimitReached    bool         `json:"limit_reached"`
		PrimaryWindow   *usageWindow `json:"primary_window"`
		SecondaryWindow *usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

// toBucket converts one window. Returns ok=false for a window we cannot name or
// time, because a bucket with no reset sorts as "unknown" in the ranking and an
// invented one is worse than an absent one.
func (w *usageWindow) toBucket(limitReached bool) (provider.QuotaBucket, bool) {
	if w == nil {
		return provider.QuotaBucket{}, false
	}
	name := bucketName(w.LimitWindowSeconds)
	if name == "" || w.ResetAt == 0 {
		return provider.QuotaBucket{}, false
	}
	b := provider.QuotaBucket{
		Name: name,
		// used_percent is 0..100. See the note in the spec: the Anthropic
		// header is already a fraction and this repo has divided wrongly in
		// both directions once.
		Utilization: w.UsedPercent / 100,
		ResetsAt:    w.ResetAt * 1000,
	}
	if limitReached {
		b.Status = "rejected"
	}
	return b, true
}

func (o *OpenAI) Quota(ctx context.Context, c provider.Credential) (provider.Quota, error) {
	if c.Type == provider.CredentialAPIKey {
		return provider.Quota{}, provider.ErrUnsupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.chatgptBase()+"/wham/usage", nil)
	if err != nil {
		return provider.Quota{}, err
	}
	o.Authorize(req, c)
	req.Header.Set("Accept", "application/json")

	res, err := o.hc.Do(req)
	if err != nil {
		return provider.Quota{}, fmt.Errorf("openai: usage: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode == http.StatusTooManyRequests {
		return provider.Quota{}, ErrQuotaThrottled
	}
	if res.StatusCode != http.StatusOK {
		return provider.Quota{}, fmt.Errorf("openai: usage: HTTP %d", res.StatusCode)
	}

	var ur usageResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return provider.Quota{}, fmt.Errorf("openai: usage: %w", err)
	}
	out := provider.Quota{ObservedAt: time.Now().UnixMilli()}
	if ur.RateLimit == nil {
		return out, nil
	}
	if b, ok := ur.RateLimit.PrimaryWindow.toBucket(ur.RateLimit.LimitReached); ok {
		out.Buckets = append(out.Buckets, b)
	}
	if b, ok := ur.RateLimit.SecondaryWindow.toBucket(ur.RateLimit.LimitReached); ok {
		out.Buckets = append(out.Buckets, b)
	}
	return out, nil
}
```

Add `"errors"` to the test imports.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/provider/openai/ -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/openai/
git commit -m "feat(openai): read quota from the wham usage endpoint"
```

---

### Task 5: ClassifyResponse and the x-codex header backup path

**Files:**
- Create: `internal/provider/openai/classify.go`
- Create: `internal/provider/openai/classify_test.go`

**Interfaces:**
- Consumes: `bucketName` from Task 4.
- Produces: `ClassifyResponse(*http.Response) provider.Outcome`, `parseCodexBuckets(http.Header) []provider.QuotaBucket`.

- [ ] **Step 1: Write the failing test**

```go
package openai

import (
	"net/http"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

func hdr(kv map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range kv {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: 200, Header: h}
}

// The header path must produce the SAME buckets as the JSON path, or quota
// silently depends on which source happened to answer last.
func TestParseCodexBucketsMatchesTheUsageEndpointShape(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	res := hdr(map[string]string{
		"x-codex-primary-used-percent":    "29",
		"x-codex-primary-window-minutes":  "10080",
		"x-codex-primary-reset-at":        strconv.FormatInt(reset, 10),
	})
	got := parseCodexBuckets(res.Header)
	if len(got) != 1 {
		t.Fatalf("buckets = %+v, want 1", got)
	}
	if got[0].Name != "7d" || got[0].Utilization != 0.29 {
		t.Errorf("bucket = %+v, want 7d at 0.29", got[0])
	}
	if got[0].ResetsAt != reset*1000 {
		t.Errorf("resetsAt = %d, want millis", got[0].ResetsAt)
	}
}

func TestClassify429IsThrottled(t *testing.T) {
	out := New(http.DefaultClient).ClassifyResponse(hdrStatus(429, nil))
	if out.Kind != provider.OutcomeThrottledNoHint && out.Kind != provider.OutcomeThrottledWithHint {
		t.Errorf("kind = %v, want a throttled kind", out.Kind)
	}
}

func TestClassifyRateLimitReachedIsQuotaRejected(t *testing.T) {
	res := hdrStatus(429, map[string]string{"x-codex-rate-limit-reached-type": "usage_limit_reached"})
	out := New(http.DefaultClient).ClassifyResponse(res)
	if out.Kind != provider.OutcomeQuotaRejected {
		t.Errorf("kind = %v, want quota_rejected: this account is spent, not merely throttled", out.Kind)
	}
}

func TestClassify401IsCredentialStale(t *testing.T) {
	if got := New(http.DefaultClient).ClassifyResponse(hdrStatus(401, nil)).Kind; got != provider.OutcomeCredentialStale {
		t.Errorf("kind = %v, want credential_stale", got)
	}
}

func TestClassify5xxIsServerError(t *testing.T) {
	if got := New(http.DefaultClient).ClassifyResponse(hdrStatus(503, nil)).Kind; got != provider.OutcomeServerError {
		t.Errorf("kind = %v, want server_error", got)
	}
}
```

Add a `hdrStatus(status int, kv map[string]string) *http.Response` helper alongside `hdr`, and import `strconv`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/openai/ -run Classify -v`
Expected: FAIL — `parseCodexBuckets undefined`.

- [ ] **Step 3: Write the implementation**

```go
package openai

import (
	"net/http"
	"strconv"

	"github.com/nicko170/aiproxy/internal/provider"
)

// parseCodexBuckets reads the rate-limit headers every Responses reply carries.
// This is the backup for Quota: wham/usage is a private endpoint and may move,
// but live traffic always answers with these, so quota stays current either way.
//
// The units differ from the JSON endpoint — minutes here, seconds there — so
// both are normalised to the same QuotaBucket before anything else sees them.
func parseCodexBuckets(h http.Header) []provider.QuotaBucket {
	var out []provider.QuotaBucket
	for _, w := range []string{"primary", "secondary"} {
		pct, okPct := headerFloat(h, "x-codex-"+w+"-used-percent")
		mins, okMin := headerInt(h, "x-codex-"+w+"-window-minutes")
		reset, okRes := headerInt(h, "x-codex-"+w+"-reset-at")
		if !okPct || !okMin || !okRes {
			continue
		}
		name := bucketName(mins * 60)
		if name == "" {
			continue
		}
		out = append(out, provider.QuotaBucket{
			Name:        name,
			Utilization: pct / 100,
			ResetsAt:    reset * 1000,
		})
	}
	return out
}

func headerFloat(h http.Header, k string) (float64, bool) {
	raw := h.Get(k)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil
}

func headerInt(h http.Header, k string) (int64, bool) {
	raw := h.Get(k)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	return v, err == nil
}

func (o *OpenAI) ClassifyResponse(r *http.Response) provider.Outcome {
	out := provider.Outcome{Buckets: parseCodexBuckets(r.Header)}

	switch {
	case r.StatusCode == http.StatusTooManyRequests:
		// A named reached-type means the plan's allowance is spent, which is a
		// different thing from being asked to slow down: it holds the account
		// rather than pausing it, so rotation happens instead of waiting.
		if r.Header.Get("x-codex-rate-limit-reached-type") != "" {
			out.Kind = provider.OutcomeQuotaRejected
			return out
		}
		if secs, ok := headerInt(r.Header, "Retry-After"); ok && secs >= 0 {
			out.Kind = provider.OutcomeThrottledWithHint
			out.RetryAfter = time.Duration(secs) * time.Second
			return out
		}
		out.Kind = provider.OutcomeThrottledNoHint
	case r.StatusCode == http.StatusUnauthorized:
		out.Kind = provider.OutcomeCredentialStale
	case r.StatusCode == http.StatusForbidden:
		out.Kind = provider.OutcomeCredentialRefused
	case r.StatusCode >= 500:
		out.Kind = provider.OutcomeServerError
	case r.StatusCode >= 400:
		out.Kind = provider.OutcomeClientError
	default:
		out.Kind = provider.OutcomeOK
	}
	return out
}
```

Add `"time"` to imports.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/provider/openai/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/openai/
git commit -m "feat(openai): classify responses and read quota from x-codex headers"
```

---

### Task 6: Login (PKCE S256 against auth.openai.com)

**Files:**
- Create: `internal/provider/openai/login.go`
- Create: `internal/provider/openai/login_test.go`

**Interfaces:**
- Consumes: `postToken` and `claimsFromJWT` from Task 3.
- Produces: `Login(ctx) (provider.LoginSession, error)`; helpers `pkceVerifier() (string, error)`, `pkceChallenge(verifier string) string`, `authorizeURL(port int, challenge, state string) string`.

Read `internal/provider/anthropic/login.go` first and mirror its structure: the loopback listener, the single-send `Done` channel, `tryClaim`, `Cancel`, and `SubmitCode`. The differences are only the issuer, the parameters, and the callback port.

- [ ] **Step 1: Write the failing test**

```go
package openai

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/provider/openai/ -run 'Pkce|AuthorizeURL' -v`
Expected: FAIL — `pkceChallenge undefined`.

- [ ] **Step 3: Write the implementation**

```go
package openai

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
)

const (
	// callbackPort is the port Codex registers with the OAuth client. The
	// redirect_uri must match exactly, so this cannot be an ephemeral port.
	callbackPort = 1455
	// callbackFallbackPort is used when 1455 is already bound, which happens
	// when Codex itself is mid-login.
	callbackFallbackPort = 1457

	scopes = "openid profile email offline_access api.connectors.read api.connectors.invoke"
)

// pkceVerifier is 64 random bytes base64url-encoded without padding, matching
// Codex's own generator and landing inside RFC 7636's 43..128 characters.
func pkceVerifier() (string, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("openai: pkce verifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func redirectURI(port int) string {
	return "http://localhost:" + strconv.Itoa(port) + "/auth/callback"
}

func (o *OpenAI) issuer() string {
	if o.TokenEndpointOverride != "" {
		// Tests point the whole issuer at one server.
		return o.TokenEndpointOverride
	}
	return defaultIssuer
}

func (o *OpenAI) authorizeURL(port int, challenge, state string) string {
	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {clientID},
		"redirect_uri":               {redirectURI(port)},
		"scope":                      {scopes},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"originator":                 {originator},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return defaultIssuer + "/oauth/authorize?" + q.Encode()
}

// bindCallback takes the registered port, falling back to the alternate one
// when it is already held — typically by Codex running its own login.
func bindCallback() (net.Listener, int, error) {
	for _, p := range []int{callbackPort, callbackFallbackPort} {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			return ln, p, nil
		}
	}
	return nil, 0, fmt.Errorf("openai: ports %d and %d are both in use",
		callbackPort, callbackFallbackPort)
}
```

- [ ] **Step 4: Run those tests**

Run: `go test ./internal/provider/openai/ -run 'Pkce|AuthorizeURL' -v`
Expected: PASS.

- [ ] **Step 5: Implement Login by mirroring the Anthropic flow**

Read `internal/provider/anthropic/login.go` in full and port it, changing only:
- `bindCallback()` instead of `bindLoopback()` (fixed port, not ephemeral)
- `o.authorizeURL(port, challenge, state)` for the URL
- the exchange posts `grant_type=authorization_code`, `code`, `redirect_uri=redirectURI(port)`, `client_id`, `code_verifier` through `o.postToken`
- `o.Profile` for the profile lookup after exchange

Keep unchanged: the single-send `Done` contract, `tryClaim`, `Cancel` teardown, `SubmitCode` with pasted `code#state` parsing, and the login timeout.

- [ ] **Step 6: Write the end-to-end login test**

Mirror `TestLogin...` in `internal/provider/anthropic/login_test.go`: stand up a fake token endpoint, call `Login`, drive the callback with an `http.Get` to the returned redirect, and assert exactly one `LoginResult` arrives with no error and a profile. Also assert `Cancel` produces exactly one result carrying an error.

- [ ] **Step 7: Run tests**

Run: `go test ./internal/provider/openai/ -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/provider/openai/
git commit -m "feat(openai): PKCE login against auth.openai.com"
```

---

### Task 7: Models on the provider seam

**Files:**
- Modify: `internal/provider/provider.go`
- Create: `internal/provider/openai/models.go`
- Create: `internal/provider/openai/models_test.go`
- Create: `internal/provider/anthropic/models.go`
- Create: `internal/provider/anthropic/models_test.go`

**Interfaces:**
- Produces: `provider.Model{ID, DisplayName string; ContextWindow int}` and `Models(ctx, Credential) ([]Model, error)` on `provider.Provider`; implementations in both providers.

- [ ] **Step 1: Add the type and interface method**

In `internal/provider/provider.go`:

```go
// Model is one model an account can reach. Discovered per account rather than
// configured, because access varies by plan: two accounts on the same provider
// routinely differ, and a static table would have to be maintained against
// every plan change.
type Model struct {
	ID            string
	DisplayName   string
	ContextWindow int
}
```

Add to the `Provider` interface, after `Quota`:

```go
	// Models lists what this credential can reach. A provider with no
	// discovery endpoint returns ErrUnsupported, which callers treat as
	// "unknown", never as "none".
	Models(ctx context.Context, c Credential) ([]Model, error)
```

- [ ] **Step 2: Run the build to see every implementer break**

Run: `go build ./...`
Expected: FAIL — `*Anthropic does not implement provider.Provider`, plus any test fakes.

- [ ] **Step 3: Write the failing tests**

`internal/provider/anthropic/models_test.go`:

```go
func TestModelsReadsTheAnthropicList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"data":[
		  {"type":"model","id":"claude-opus-5","display_name":"Claude Opus 5","max_input_tokens":1000000},
		  {"type":"model","id":"claude-haiku-4-5","display_name":"Claude Haiku 4.5","max_input_tokens":200000}
		]}`)
	}))
	defer srv.Close()

	a := New(http.DefaultClient)
	a.BaseURLOverride = srv.URL
	got, err := a.Models(context.Background(), provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 2 || got[0].ID != "claude-opus-5" || got[0].ContextWindow != 1000000 {
		t.Errorf("models = %+v", got)
	}
}
```

`internal/provider/openai/models_test.go`:

```go
func TestModelsReadsTheWhamCatalogue(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/models" {
			t.Errorf("path = %q, want /wham/models", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"models":[
		  {"slug":"gpt-5.6-sol","display_name":"GPT-5.6-Sol","context_window":272000},
		  {"slug":"gpt-5.4-mini","display_name":"GPT-5.4-Mini","context_window":272000}
		]}`)
	}))
	defer srv.Close()

	o := New(http.DefaultClient)
	o.ChatGPTBaseURLOverride = srv.URL
	got, err := o.Models(context.Background(), provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	// client_version is required: without it the endpoint answers a validation
	// error rather than a catalogue.
	if !strings.Contains(gotQuery, "client_version=") {
		t.Errorf("query = %q, want client_version", gotQuery)
	}
	if len(got) != 2 || got[0].ID != "gpt-5.6-sol" || got[0].ContextWindow != 272000 {
		t.Errorf("models = %+v", got)
	}
}
```

- [ ] **Step 4: Implement both**

`internal/provider/anthropic/models.go`:

```go
func (a *Anthropic) Models(ctx context.Context, c provider.Credential) ([]provider.Model, error) {
	body, status, err := a.get(ctx, "/v1/models?limit=100", c, true)
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("models: HTTP %d", status)
	}
	var mr struct {
		Data []struct {
			ID             string `json:"id"`
			DisplayName    string `json:"display_name"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	out := make([]provider.Model, 0, len(mr.Data))
	for _, m := range mr.Data {
		out = append(out, provider.Model{ID: m.ID, DisplayName: m.DisplayName, ContextWindow: m.MaxInputTokens})
	}
	return out, nil
}
```

`internal/provider/openai/models.go`:

```go
// Models lists what this account can reach.
//
// NOT api.openai.com/v1/models: a ChatGPT OAuth token is refused there with
// "Missing scopes: api.model.read", because Codex's token carries only
// openid/profile/email/offline_access/api.connectors.*. The wham catalogue is
// the substitute and is per-account, which is what makes plan differences fall
// out for free.
func (o *OpenAI) Models(ctx context.Context, c provider.Credential) ([]provider.Model, error) {
	if c.Type == provider.CredentialAPIKey {
		return nil, provider.ErrUnsupported
	}
	u := o.chatgptBase() + "/wham/models?client_version=" + url.QueryEscape(o.clientVersion())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	o.Authorize(req, c)
	req.Header.Set("Accept", "application/json")

	res, err := o.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: models: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<21))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai: models: HTTP %d", res.StatusCode)
	}
	var mr struct {
		Models []struct {
			Slug          string `json:"slug"`
			DisplayName   string `json:"display_name"`
			ContextWindow int    `json:"context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("openai: models: %w", err)
	}
	out := make([]provider.Model, 0, len(mr.Models))
	for _, m := range mr.Models {
		out = append(out, provider.Model{ID: m.Slug, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow})
	}
	return out, nil
}
```

- [ ] **Step 5: Fix every other implementer**

Run: `go build ./... && go vet ./...`
Add `Models` returning `nil, provider.ErrUnsupported` to each test fake the compiler names (at minimum `internal/prober/prober_test.go`'s `fakeProvider` and `internal/account`'s `stubProvider`).

- [ ] **Step 6: Run tests**

Run: `go test ./... -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat(provider): add Models to the seam, implemented for both providers"
```

---

### Task 8: Model catalogue in account.Manager

**Files:**
- Modify: `internal/account/account.go` (add `Models` to `Account`)
- Modify: `internal/account/manager.go` (add `UpdateModels`)
- Modify: `internal/account/select.go` (eligibility filter)
- Modify: `internal/account/select_test.go`

**Interfaces:**
- Consumes: `provider.Model` from Task 7.
- Produces: `Account.Models []provider.Model`; `(*Manager) UpdateModels(id string, models []provider.Model)`; `Select` filtering on catalogue membership.

- [ ] **Step 1: Write the failing test**

```go
// Routing follows discovered access: an account that cannot reach a model is
// not a candidate for it, whatever its priority or quota says.
func TestSelectSkipsAnAccountWithoutTheModel(t *testing.T) {
	m := mgr(t, acct("haiku-only", 0), acct("everything", 5))
	m.UpdateModels("haiku-only", []provider.Model{{ID: "claude-haiku-4-5"}})
	m.UpdateModels("everything", []provider.Model{{ID: "claude-haiku-4-5"}, {ID: "claude-opus-5"}})

	got, err := m.Select(SelectRequest{Model: "claude-opus-5"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "everything" {
		t.Errorf("selected %q; only that account can reach claude-opus-5", got.ID)
	}
}

// An account whose catalogue has never been read must stay usable. Failing
// closed would take a freshly added account out of service until its first
// probe, which is the startup blindness the prober fix removed.
func TestSelectTreatsAnUnknownCatalogueAsEligible(t *testing.T) {
	m := mgr(t, acct("unprobed", 0))
	got, err := m.Select(SelectRequest{Model: "anything-at-all"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "unprobed" {
		t.Errorf("selected %q", got.ID)
	}
}

func TestSelectReportsNoAccountWhenNobodyHasTheModel(t *testing.T) {
	m := mgr(t, acct("a", 0))
	m.UpdateModels("a", []provider.Model{{ID: "claude-haiku-4-5"}})
	if _, err := m.Select(SelectRequest{Model: "gpt-5.6-sol"}); !errors.Is(err, ErrNoAccount) {
		t.Errorf("err = %v, want ErrNoAccount", err)
	}
}

// A discovery failure must not empty a catalogue: that would silently take
// every model on the account out of service for a reason unrelated to health.
func TestUpdateModelsIgnoresAnEmptyList(t *testing.T) {
	m := mgr(t, acct("a", 0))
	m.UpdateModels("a", []provider.Model{{ID: "claude-opus-5"}})
	m.UpdateModels("a", nil)
	if _, err := m.Select(SelectRequest{Model: "claude-opus-5"}); err != nil {
		t.Errorf("Select after a failed refresh: %v; the previous catalogue must survive", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/account/ -run 'Catalogue|WithoutTheModel|NobodyHas|UpdateModels' -v`
Expected: FAIL — `m.UpdateModels undefined`.

- [ ] **Step 3: Implement**

In `internal/account/account.go`, add to `Account`:

```go
	// Models is the most recent catalogue read for this account. Nil means
	// "not yet known", which is NOT the same as "none" — see eligibleLocked.
	Models []provider.Model
```

In `copyAccount`, deep-copy the slice alongside `Buckets`.

In `internal/account/manager.go`:

```go
// UpdateModels records a catalogue read.
//
// An empty list is ignored rather than stored. Discovery runs against a private
// endpoint on a timer, and a transient failure that emptied the catalogue would
// make every model on the account unroutable — an availability outage caused by
// a monitoring call, which is the wrong direction to fail.
func (m *Manager) UpdateModels(id string, models []provider.Model) {
	if len(models) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil {
		a.Models = append([]provider.Model(nil), models...)
	}
}
```

In `eligibleLocked`, after the rate-limit check:

```go
	if !servesModel(a, req.Model) {
		return false
	}
```

and:

```go
// servesModel reports whether this account can reach the requested model. An
// account with no catalogue yet is treated as able to serve anything: unknown
// must not mean unusable, or a new account is dead until its first probe.
func servesModel(a *Account, model string) bool {
	if model == "" || len(a.Models) == 0 {
		return true
	}
	for _, m := range a.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/account/ -count=1`
Expected: PASS. Existing tests pass unchanged because none of them set `Models`, so every account reads as "unknown catalogue".

- [ ] **Step 5: Commit**

```bash
git add internal/account/
git commit -m "feat(account): per-account model catalogue and routing on discovered access"
```

---

### Task 9: Discover catalogues on the prober cycle

**Files:**
- Modify: `internal/prober/prober.go`
- Modify: `internal/prober/prober_test.go`

**Interfaces:**
- Consumes: `(*Manager) UpdateModels` from Task 8, `Provider.Models` from Task 7.
- Produces: catalogue refresh inside `probeAll`.

- [ ] **Step 1: Write the failing test**

```go
// The catalogue is read on the same cycle as quota: both are per-account facts
// that go stale, and both are read from the same credential we just renewed.
func TestProbeRefreshesTheModelCatalogue(t *testing.T) {
	fp := &fakeProvider{
		results: []quotaResult{quotaOK()},
		models:  []provider.Model{{ID: "gpt-5.6-sol"}},
	}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if got := mgr.All()[0].Models; len(got) != 1 || got[0].ID != "gpt-5.6-sol" {
		t.Errorf("models = %+v, want the discovered catalogue", got)
	}
}

// A provider with no catalogue endpoint must not make the cycle look failed.
func TestProbeToleratesUnsupportedModels(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{quotaOK()}, modelsErr: provider.ErrUnsupported}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Errorf("ProbeNow: %v; an unsupported catalogue is not a probe failure", err)
	}
}
```

Add `models []provider.Model` and `modelsErr error` to `fakeProvider`, and make its `Models` return them.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/prober/ -run Catalogue -v`
Expected: FAIL — models never recorded.

- [ ] **Step 3: Implement**

In `probeAll`, after `p.mgr.UpdateQuota(a.ID, q.Buckets)`:

```go
		// Catalogue on the same cycle and the same freshly renewed credential.
		// A failure here is logged but does NOT fail the cycle or clear the
		// stored catalogue: UpdateModels ignores an empty list, so the last
		// known good list survives a bad read.
		models, err := prov.Models(ctx, a.Credential)
		switch {
		case errors.Is(err, provider.ErrUnsupported):
			// Nothing to discover for this provider.
		case err != nil:
			p.log.Warn("model catalogue read failed", "account", a.Label, "err", err)
		default:
			p.mgr.UpdateModels(a.ID, models)
		}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/prober/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/prober/
git commit -m "feat(prober): discover each account's model catalogue on the probe cycle"
```

---

### Task 10: Synthetic /v1/models

**Files:**
- Create: `internal/proxy/models.go`
- Create: `internal/proxy/models_test.go`
- Modify: `internal/proxy/router.go`
- Modify: `internal/view/types.go`, `internal/view/local.go`, `internal/proxy/control.go` (if a Source method is added)

**Interfaces:**
- Consumes: `Account.Models` from Task 8.
- Produces: `modelsHandler(o HandlerOptions) http.HandlerFunc` mounted at `GET /v1/models`.

Read the list from `HandlerOptions.Manager` rather than adding a `view.Source` method, so the `TestEveryViewSourceMethodHasAControlRoute` invariant is untouched.

- [ ] **Step 1: Write the failing test**

```go
// One list, two dialects. Codex parses object/owned_by; Claude Code parses
// type/id/display_name. Emitting both avoids sniffing the caller.
func TestModelsEndpointEmitsBothDialects(t *testing.T) {
	h := newModelsHarness(t,
		acctWithModels("anth", "anthropic", provider.Model{ID: "claude-opus-5", DisplayName: "Claude Opus 5", ContextWindow: 1000000}),
		acctWithModels("oai", "openai", provider.Model{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol", ContextWindow: 272000}),
	)
	res := h.get("/v1/models")
	if res.Code != 200 {
		t.Fatalf("status = %d", res.Code)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID          string `json:"id"`
			Object      string `json:"object"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
			OwnedBy     string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" {
		t.Errorf("object = %q, want list", body.Object)
	}
	if len(body.Data) != 2 {
		t.Fatalf("data = %+v, want both models", body.Data)
	}
	for _, m := range body.Data {
		if m.ID == "" || m.Object != "model" || m.Type != "model" || m.DisplayName == "" || m.OwnedBy == "" {
			t.Errorf("model %+v is missing a field one of the two dialects needs", m)
		}
	}
}

// Two accounts on the same plan list the same models; the endpoint is a
// catalogue, not a tally.
func TestModelsEndpointDedupes(t *testing.T) {
	h := newModelsHarness(t,
		acctWithModels("a", "anthropic", provider.Model{ID: "claude-opus-5"}),
		acctWithModels("b", "anthropic", provider.Model{ID: "claude-opus-5"}),
	)
	var body struct{ Data []struct{ ID string } }
	json.Unmarshal(h.get("/v1/models").Body.Bytes(), &body)
	if len(body.Data) != 1 {
		t.Errorf("data = %+v, want one entry", body.Data)
	}
}

// A disabled account's models are not reachable, so listing them would invite
// a request that cannot be served.
func TestModelsEndpointOmitsDisabledAccounts(t *testing.T) {
	h := newModelsHarnessDisabled(t, acctWithModels("off", "anthropic", provider.Model{ID: "claude-opus-5"}))
	var body struct{ Data []struct{ ID string } }
	json.Unmarshal(h.get("/v1/models").Body.Bytes(), &body)
	if len(body.Data) != 0 {
		t.Errorf("data = %+v, want nothing from a disabled account", body.Data)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proxy/ -run ModelsEndpoint -v`
Expected: FAIL — the route does not exist, so the catch-all proxies upstream.

- [ ] **Step 3: Implement**

```go
// modelsHandler answers GET /v1/models from the union of every enabled
// account's discovered catalogue, rather than relaying upstream.
//
// The emitted object carries BOTH vendors' field names: object/owned_by/created
// for an OpenAI-shaped parser, type/id/display_name for an Anthropic-shaped
// one. That is a shape neither vendor documents, and it is chosen so the
// endpoint does not have to guess which client is calling — a guess that would
// be wrong for any client we have not seen.
func modelsHandler(o HandlerOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			ID            string `json:"id"`
			Object        string `json:"object"`
			Type          string `json:"type"`
			DisplayName   string `json:"display_name"`
			OwnedBy       string `json:"owned_by"`
			ContextWindow int    `json:"context_window,omitempty"`
		}
		seen := map[string]bool{}
		out := []entry{}
		for _, a := range o.Manager.All() {
			if a.Disabled {
				continue
			}
			for _, m := range a.Models {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				name := m.DisplayName
				if name == "" {
					name = m.ID
				}
				out = append(out, entry{
					ID: m.ID, Object: "model", Type: "model",
					DisplayName: name, OwnedBy: a.Provider, ContextWindow: m.ContextWindow,
				})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
	}
}
```

`HandlerOptions.Manager` is the `*account.Manager`; `All()` returns value copies carrying `Provider`, `Disabled` and `Models`.

- [ ] **Step 4: Mount the route**

In `internal/proxy/router.go`, before the catch-all:

```go
	// Answered locally, not relayed: the union of every account's catalogue is
	// something no single upstream can produce.
	r.Get("/v1/models", modelsHandler(o))
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/proxy/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/proxy/
git commit -m "feat(proxy): synthetic /v1/models unioning every account's catalogue"
```

---

### Task 11: Wire the provider, config, and Codex credential import

**Files:**
- Modify: `cmd/aiproxy/main.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/import.go`, `internal/config/import_test.go`
- Modify: `internal/view/alias.go`, `internal/view/local.go`
- Modify: `internal/warmer/warmer.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: `openai` in the providers map; `config.Providers{OpenAI{ClientVersion}}`; `config.ImportSourceCodex`; `config.CodexPath()`.

- [ ] **Step 1: Write the failing test**

```go
func TestImportCodexCredentials(t *testing.T) {
	p := writeFile(t, t.TempDir(), "auth.json", `{
	  "auth_mode":"chatgpt",
	  "tokens":{"id_token":"idt","access_token":"at","refresh_token":"rt","account_id":"acc-1"},
	  "last_refresh":"2026-08-12T04:36:46Z"}`)

	got, err := ImportFile(p, ImportSourceCodex)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want 1 account", got)
	}
	if got[0].Provider != "openai" {
		t.Errorf("provider = %q, want openai", got[0].Provider)
	}
	if got[0].Credential.AccessToken != "at" || got[0].Credential.RefreshToken != "rt" {
		t.Errorf("credential not carried across: %+v", got[0].Credential)
	}
	if got[0].Credential.AccountID != "acc-1" {
		t.Errorf("AccountID = %q; the chatgpt-account-id header needs it", got[0].Credential.AccountID)
	}
}

// An api-key-mode auth.json has no OAuth tokens to adopt.
func TestImportCodexSkipsApiKeyMode(t *testing.T) {
	p := writeFile(t, t.TempDir(), "auth.json", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x","tokens":null}`)
	got, err := ImportFile(p, ImportSourceCodex)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run Codex -v`
Expected: FAIL — `undefined: ImportSourceCodex`.

- [ ] **Step 3: Implement the import source**

```go
const ImportSourceCodex ImportSource = "codex"

// CodexPath is the Codex CLI's credential file.
func CodexPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

type codexFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   *struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}
```

Add the `case ImportSourceCodex:` branch to `ImportFile`, returning a single account with `Provider: "openai"`, `Label: "imported (codex)"`, and the OAuth credential including `AccountID`. Skip when `Tokens == nil` or `AccessToken == ""`.

- [ ] **Step 4: Expose it through the view and TUI**

Add `ImportSourceCodex = config.ImportSourceCodex` to `internal/view/alias.go`, and a `case config.ImportSourceCodex: path = config.CodexPath()` to `ImportCredentials`.

The Accounts screen currently imports Claude Code directly on `i` (the menu was removed with the teamclaude source). With two sources again, restore a two-key prompt: `c` Claude Code, `x` Codex. Update the footer hints and the help text alongside it.

- [ ] **Step 5: Wire the provider and config**

In `internal/config/config.go`:

```go
// Providers holds per-provider settings that are not per-account.
type Providers struct {
	OpenAI OpenAIProvider `json:"openai"`
}

// OpenAIProvider configures the ChatGPT provider. ClientVersion is sent to the
// private model-catalogue endpoint, which rejects a request without one; it is
// configurable because a server-side version gate is a plausible way for
// catalogue discovery to break between releases.
type OpenAIProvider struct {
	ClientVersion string `json:"clientVersion"`
}
```

Add `Providers Providers \`json:"providers"\`` to `Config` and
`Providers: Providers{OpenAI: OpenAIProvider{ClientVersion: "0.147.0"}}` to the defaults.

In `cmd/aiproxy/main.go`, in the providers map:

```go
	oai := openai.New(&http.Client{Timeout: 30 * time.Second})
	oai.ClientVersion = cfg.Providers.OpenAI.ClientVersion
	providers := map[string]provider.Provider{
		"anthropic": anth,
		"openai":    oai,
	}
```

- [ ] **Step 6: Keep warming off ChatGPT accounts**

Whether OpenAI's windows are first-use-anchored is UNVERIFIED (spec §10). Warming a fixed window wastes a request every cycle. In `internal/warmer/warmer.go`, in `usable`:

```go
	// Warming assumes a window that starts on first use, which is confirmed for
	// Anthropic and unverified for OpenAI. Until it is established, a ChatGPT
	// account is not warmed: the cost of being wrong is a billable request
	// every cycle that buys nothing.
	if a.Provider != "anthropic" {
		return false
	}
```

with a test asserting an `openai` account is never warmed.

- [ ] **Step 7: Document**

Add a README section covering: logging in with ChatGPT, pointing Codex at the proxy with the `model_providers` TOML block, the synthetic `/v1/models`, that routing follows discovered per-account access, that `providers.openai.clientVersion` exists and why, and that a Messages request naming a GPT model will fail until translation lands.

- [ ] **Step 8: Full verification**

```bash
gofmt -l . | grep -v vendor
go build ./... && go vet ./...
go test ./... -count=1
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -o /dev/null ./cmd/aiproxy || echo "FAIL $t"
done
```

Expected: all clean, all four targets build.

- [ ] **Step 9: Commit**

```bash
git add -A
git commit -m "feat: wire the ChatGPT provider, codex import, and provider config"
```

---

## Self-Review

**Spec coverage.** §3 OAuth → Tasks 3 and 6. §4 inference headers → Tasks 1, 2, 3. §5 quota including the percent trap and the null secondary window → Task 4; header backup → Task 5. §6 catalogue → Tasks 7, 8, 9. §7 routing → Task 8. §8 synthetic `/v1/models` → Task 10. §9 config, TUI, Codex import → Task 11. §10 risks: warming disabled for OpenAI in Task 11 step 6; `ParseUsage` in Task 2; private-endpoint degradation in Tasks 4, 5, 8. §11 testing is distributed across the tasks it belongs to.

**One spec item deliberately deferred:** the privacy filter's `SkipKey` review against a Responses body. It needs a real captured body to review against, it cannot break anything while the filter is off by default, and folding a guess into this plan would be worse than a follow-up with evidence. Flagged here rather than silently dropped.

**Type consistency.** `provider.Model{ID, DisplayName, ContextWindow}` is used identically in Tasks 7, 8, 9, 10. `UpdateModels(id string, models []provider.Model)` matches between Tasks 8 and 9. `bucketName(int64) string` is defined in Task 4 and consumed in Task 5. `Credential.AccountID` is added in Task 3 and consumed in Tasks 2 (header) and 11 (import).

**Known plan risk.** Task 7 changes the `provider.Provider` interface, so every implementer and test fake breaks at once. That is deliberate — the compiler enumerates the call sites — and Task 7 step 5 exists to sweep them.
