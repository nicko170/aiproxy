package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
