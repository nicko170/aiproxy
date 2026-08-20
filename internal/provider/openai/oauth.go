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
