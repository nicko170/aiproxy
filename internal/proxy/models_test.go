package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// modelsHarness drives the real router (so a missing or misordered route
// registration is caught) against an in-memory account.Manager, with no fake
// upstream and no live httptest.Server: /v1/models is answered entirely from
// Manager state and never dials out, so a request need only reach
// http.Handler.ServeHTTP. The full newRouterHarness in handler_test.go wires a
// FakeUpstream, a metrics store, a config store and a prober for the control
// API and retry behaviour this endpoint does not touch; reusing it here would
// only obscure what these tests are actually about.
type modelsHarness struct {
	t   *testing.T
	mgr *account.Manager
	r   http.Handler
}

// acctSpec is one account to seed into a modelsHarness, along with the
// catalogue the (fake) prober is pretending to have discovered for it.
type acctSpec struct {
	id, provider string
	models       []provider.Model
}

// acctWithModels builds an acctSpec carrying the given catalogue.
func acctWithModels(id, prov string, models ...provider.Model) acctSpec {
	return acctSpec{id: id, provider: prov, models: models}
}

// newModelsHarness wires the given accounts, all enabled, with no API key
// configured (Authorized's "no key configured" exemption applies).
func newModelsHarness(t *testing.T, specs ...acctSpec) *modelsHarness {
	t.Helper()
	return newModelsHarnessFull(t, false, "", specs...)
}

// newModelsHarnessDisabled wires the given accounts, all disabled — for
// asserting that a disabled account's catalogue never surfaces.
func newModelsHarnessDisabled(t *testing.T, specs ...acctSpec) *modelsHarness {
	t.Helper()
	return newModelsHarnessFull(t, true, "", specs...)
}

// newModelsHarnessWithKey wires the given accounts, all enabled, behind the
// given proxy API key — for exercising the Authorized gate itself, which
// newModelsHarness's empty key would bypass unconditionally.
func newModelsHarnessWithKey(t *testing.T, apiKey string, specs ...acctSpec) *modelsHarness {
	t.Helper()
	return newModelsHarnessFull(t, false, apiKey, specs...)
}

func newModelsHarnessWith(t *testing.T, disabled bool, specs ...acctSpec) *modelsHarness {
	t.Helper()
	return newModelsHarnessFull(t, disabled, "", specs...)
}

func newModelsHarnessFull(t *testing.T, disabled bool, apiKey string, specs ...acctSpec) *modelsHarness {
	t.Helper()
	accts := make([]config.Account, 0, len(specs))
	for _, s := range specs {
		accts = append(accts, config.Account{
			ID: s.id, Provider: s.provider, Label: s.id, Disabled: disabled,
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		})
	}
	// No providers wired: the models endpoint reads Manager.All() only and
	// never dispatches to a provider, so an empty map is enough here.
	mgr := account.New(accts, map[string]provider.Provider{}, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})
	// Models is populated by the prober at runtime (see account.Manager.
	// UpdateModels), never by config, so the catalogue is seeded the same way
	// production does: after construction, by id.
	for _, s := range specs {
		mgr.UpdateModels(s.id, s.models)
	}

	h := &modelsHarness{t: t, mgr: mgr}
	// Log must be non-nil: a bug that mis-registers /v1/models would fall
	// through to the catch-all proxyHandler, which logs through o.Log before
	// this test's assertions ever see the resulting response.
	h.r = NewRouter(HandlerOptions{Manager: mgr, Log: quietLogger(), APIKey: apiKey})
	return h
}

// get drives the router directly (no network socket — nothing here needs
// one) and returns the recorder, matching what the assertions in this file
// need: res.Code and res.Body.Bytes(). The request's RemoteAddr is left at
// httptest.NewRequest's default ("192.0.2.1:1234"), which is already
// non-loopback; every test using get() runs with no API key configured, so
// Authorized's "no key configured" exemption is what actually admits it.
func (h *modelsHarness) get(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.getFrom(path, "", "")
}

// getFrom is like get, but pins the request's RemoteAddr and an optional
// x-api-key header — the two inputs Authorized actually keys off — so a test
// can exercise a specific (loopback-or-not, keyed-or-not) combination
// directly rather than relying on httptest's default remote address.
func (h *modelsHarness) getFrom(path, remoteAddr, apiKey string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	rec := httptest.NewRecorder()
	h.r.ServeHTTP(rec, req)
	return rec
}

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

// refusingProvider refuses every refresh with ErrCredentialRejected, which is
// what account.Manager sidelines an account on. Only Refresh and Name are
// reachable from the test below; the rest satisfy the interface.
type refusingProvider struct{}

func (refusingProvider) Name() string { return "anthropic" }
func (refusingProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	return provider.Credential{}, fmt.Errorf("%w: token revoked", provider.ErrCredentialRejected)
}
func (refusingProvider) Profile(context.Context, provider.Credential) (provider.Profile, error) {
	return provider.Profile{}, provider.ErrUnsupported
}
func (refusingProvider) Quota(context.Context, provider.Credential) (provider.Quota, error) {
	return provider.Quota{}, provider.ErrUnsupported
}
func (refusingProvider) Models(context.Context, provider.Credential) ([]provider.Model, error) {
	return nil, provider.ErrUnsupported
}
func (refusingProvider) Login(context.Context) (provider.LoginSession, error) {
	return provider.LoginSession{}, provider.ErrUnsupported
}
func (refusingProvider) Endpoint(provider.Account) *url.URL {
	u, _ := url.Parse("https://upstream.invalid")
	return u
}
func (refusingProvider) Authorize(*http.Request, provider.Credential)             {}
func (refusingProvider) RewriteBody(b []byte, _ provider.Account) ([]byte, error) { return b, nil }
func (refusingProvider) ClassifyResponse(*http.Response) provider.Outcome         { return provider.Outcome{} }
func (refusingProvider) ParseUsage([]byte) (*provider.UsageDelta, bool)           { return nil, false }
func (refusingProvider) ParseUsageBody([]byte) (*provider.UsageDelta, bool)       { return nil, false }

// An account the upstream has REFUSED is sidelined from selection
// (account.eligibleLocked drops StatusErrored) but stays in the registry, so
// this endpoint kept advertising models only that account could serve. Every
// request naming one then failed with "no account ready" against a model the
// proxy had just listed as available.
func TestModelsEndpointOmitsErroredAccounts(t *testing.T) {
	accts := []config.Account{
		{
			ID: "dead", Provider: "anthropic", Label: "dead",
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				// Already expired, so the forced refresh below is the realistic
				// path rather than a contrived one.
				ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
			},
		},
		{
			ID: "live", Provider: "anthropic", Label: "live",
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		},
	}
	mgr := account.New(accts, map[string]provider.Provider{"anthropic": refusingProvider{}},
		account.Options{
			SwitchThreshold: 0.98,
			Persist:         func(string, provider.Credential) error { return nil },
		})
	mgr.UpdateModels("dead", []provider.Model{{ID: "claude-only-on-the-dead-account"}})
	mgr.UpdateModels("live", []provider.Model{{ID: "claude-opus-5"}})

	// Sideline "dead" the way production does: a refused refresh.
	if err := mgr.EnsureFresh(context.Background(), "dead", true); err == nil {
		t.Fatal("want the refused refresh to return an error")
	}
	if got, _ := mgr.Get("dead"); got.Status != account.StatusErrored {
		t.Fatalf("Status = %v, want StatusErrored — the test cannot assert what it is about", got.Status)
	}

	h := &modelsHarness{t: t, mgr: mgr}
	h.r = NewRouter(HandlerOptions{Manager: mgr, Log: quietLogger()})

	var body struct{ Data []struct{ ID string } }
	json.Unmarshal(h.get("/v1/models").Body.Bytes(), &body)
	ids := map[string]bool{}
	for _, d := range body.Data {
		ids[d.ID] = true
	}
	if ids["claude-only-on-the-dead-account"] {
		t.Error("a sidelined account's models must not be advertised: nothing can serve them")
	}
	if !ids["claude-opus-5"] {
		t.Errorf("data = %+v, want the healthy account's catalogue still listed", body.Data)
	}
}

// A non-loopback caller with no key must be refused. Without this gate,
// /v1/models would be the only route in the process that answers an
// unauthenticated non-loopback caller (proxyHandler, passthroughHandler and
// controlHandler all check Authorized first) — and what it answers is the
// union of every logged-in account's models, which leaks plan entitlements
// and account composition to anyone who can reach the port.
func TestModelsEndpointRefusesUnauthenticatedNonLoopbackCaller(t *testing.T) {
	h := newModelsHarnessWithKey(t, "secret",
		acctWithModels("a", "anthropic", provider.Model{ID: "claude-opus-5"}))

	res := h.getFrom("/v1/models", "203.0.113.7:1234", "")
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unauthenticated non-loopback caller", res.Code)
	}
}

// A loopback caller needs no key at all: Authorized's loopback exemption is
// documented behaviour every other handler already relies on, so a local
// agent (the TUI, a health check, a script run on the same host) needs no
// credential to see the model list either.
func TestModelsEndpointAllowsLoopbackCallerWithNoKey(t *testing.T) {
	h := newModelsHarnessWithKey(t, "secret",
		acctWithModels("a", "anthropic", provider.Model{ID: "claude-opus-5"}))

	res := h.getFrom("/v1/models", "127.0.0.1:1234", "")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a loopback caller with no key", res.Code)
	}
}
