package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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

// accessToken is shaped like a real access_token: identity lives under the
// profile namespace and there is NO top-level email, which is what made
// Profile return an empty label for every ChatGPT account.
func accessToken(t *testing.T) string {
	return jwt(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-123",
			"chatgpt_plan_type":  "plus",
			"poid":               "org-xyz",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "someone@example.com",
			"name":  "Some One",
		},
	})
}

func meServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me" {
			t.Errorf("path = %q, want /v1/me — apiBase is bare, so the version prefix must be added here", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The access token carries no top-level email, so reading only that produced an
// empty Profile and the placeholder "logged-in account" label.
func TestProfileReadsIdentityFromTheProfileNamespace(t *testing.T) {
	o := New(http.DefaultClient)
	o.BaseURLOverride = meServer(t, 500, `{}`).URL // force the claims fallback

	got, err := o.Profile(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: accessToken(t),
	})
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got.Email != "someone@example.com" {
		t.Errorf("Email = %q; the access token keeps it under the profile namespace", got.Email)
	}
	if got.DisplayName != "Some One" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
}

// /v1/me is the only source of a human-readable org name, which is what turns
// a bare email into the "email (Org)" label every other account gets.
func TestProfilePrefersTheMeEndpoint(t *testing.T) {
	o := New(http.DefaultClient)
	o.BaseURLOverride = meServer(t, 200, `{"email":"me@example.com","name":"Real Name","orgs":{"data":[
	  {"id":"org-other","title":"Other","is_default":false},
	  {"id":"org-def","title":"Personal","is_default":true}]}}`).URL

	got, err := o.Profile(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: accessToken(t),
	})
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if got.Email != "me@example.com" || got.DisplayName != "Real Name" {
		t.Errorf("profile = %+v, want the /v1/me values to win over the claims", got)
	}
	if got.OrgName != "Personal" {
		t.Errorf("OrgName = %q, want the DEFAULT org's title, not the first listed", got.OrgName)
	}
	if got.OrgUUID != "org-def" {
		t.Errorf("OrgUUID = %q, want the default org's id", got.OrgUUID)
	}
}

// Profile runs at the end of login. A failed identity read must degrade the
// label, never discard a credential the user just authorised in a browser.
func TestProfileSurvivesAFailingMeEndpoint(t *testing.T) {
	o := New(http.DefaultClient)
	o.BaseURLOverride = meServer(t, 503, `nonsense`).URL

	got, err := o.Profile(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: accessToken(t),
	})
	if err != nil {
		t.Fatalf("Profile must not fail when /v1/me does: %v", err)
	}
	if got.Email == "" || got.AccountUUID != "acc-123" || got.Plan != "plus" {
		t.Errorf("profile = %+v, want the claims-derived fallback intact", got)
	}
}

// The id_token shape: identity at the top level plus an organizations array.
func TestClaimsReadTheIdTokenShapeToo(t *testing.T) {
	tok := jwt(t, map[string]any{
		"email": "id@example.com",
		"name":  "Id Person",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acc-9",
			"organizations": []any{
				map[string]any{"id": "o1", "title": "First", "is_default": false},
				map[string]any{"id": "o2", "title": "Default Org", "is_default": true},
			},
		},
	})
	cl, err := claimsFromJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Email != "id@example.com" || cl.Name != "Id Person" {
		t.Errorf("claims = %+v", cl)
	}
	if cl.OrgName != "Default Org" {
		t.Errorf("OrgName = %q, want the default org's title", cl.OrgName)
	}
}
