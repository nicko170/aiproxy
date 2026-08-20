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
	Name      string
	OrgName   string
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
	// Identity appears in TWO different places depending on which token this
	// is, and both are read because callers hold whichever they were given.
	//
	// The id_token carries `email` and `name` at the top level, plus an
	// `organizations` array with a human-readable title. The access_token
	// carries neither of those: its email and name live under the
	// api.openai.com/profile namespace, and it has no organizations at all.
	// Reading only the top level meant Profile — which is handed the ACCESS
	// token — produced an empty email, and every ChatGPT account was labelled
	// "logged-in account" instead of a person.
	var payload struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
			ChatGPTUserID    string `json:"chatgpt_user_id"`
			POID             string `json:"poid"`
			Organizations    []struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				IsDefault bool   `json:"is_default"`
			} `json:"organizations"`
		} `json:"https://api.openai.com/auth"`
		Profile struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"https://api.openai.com/profile"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return authClaims{}, fmt.Errorf("openai: parse claims: %w", err)
	}
	out := authClaims{
		AccountID: payload.Auth.ChatGPTAccountID,
		PlanType:  payload.Auth.ChatGPTPlanType,
		UserID:    payload.Auth.ChatGPTUserID,
		OrgID:     payload.Auth.POID,
		Email:     payload.Email,
		Name:      payload.Name,
	}
	if out.Email == "" {
		out.Email = payload.Profile.Email
	}
	if out.Name == "" {
		out.Name = payload.Profile.Name
	}
	// Prefer the default org's title; fall back to the first named one.
	for _, org := range payload.Auth.Organizations {
		if org.Title == "" {
			continue
		}
		if org.IsDefault || out.OrgName == "" {
			out.OrgName = org.Title
		}
		if org.IsDefault {
			break
		}
	}
	return out, nil
}

func (o *OpenAI) tokenEndpoint() string {
	if o.TokenEndpointOverride != "" {
		return o.TokenEndpointOverride
	}
	return defaultIssuer + "/oauth/token"
}

// Profile identifies the person behind a credential.
//
// It asks /v1/me first and falls back to the token's own claims. The endpoint
// is worth the round trip because it is the only source of a human-readable
// ORGANISATION name — the access token carries none, and without it an account
// can only ever be labelled by bare email. It also answers for a ChatGPT OAuth
// token, unlike /v1/models on the same host, which refuses for want of the
// api.model.read scope.
//
// A failure there is NOT fatal. Profile is called at the end of login, and a
// transient identity read must not throw away a credential the user has just
// authorised in a browser: a degraded label is recoverable, a lost login is a
// second trip through the whole flow.
func (o *OpenAI) Profile(ctx context.Context, c provider.Credential) (provider.Profile, error) {
	cl, err := claimsFromJWT(c.AccessToken)
	if err != nil {
		return provider.Profile{}, err
	}
	p := provider.Profile{
		AccountUUID: cl.AccountID,
		Email:       cl.Email,
		DisplayName: cl.Name,
		OrgUUID:     cl.OrgID,
		OrgName:     cl.OrgName,
		Plan:        cl.PlanType,
	}
	if me, err := o.me(ctx, c); err == nil {
		if me.Email != "" {
			p.Email = me.Email
		}
		if me.Name != "" {
			p.DisplayName = me.Name
		}
		if org, ok := me.defaultOrg(); ok {
			if org.Title != "" {
				p.OrgName = org.Title
			}
			if org.ID != "" {
				p.OrgUUID = org.ID
			}
		}
	}
	return p, nil
}

type meResponse struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Orgs  struct {
		Data []meOrg `json:"data"`
	} `json:"orgs"`
}

type meOrg struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	IsDefault bool   `json:"is_default"`
}

// defaultOrg picks the org to label the account with: the one marked default,
// else the first. An account can belong to several, and picking arbitrarily
// would make the label change between logins for no visible reason.
func (m meResponse) defaultOrg() (meOrg, bool) {
	for _, o := range m.Orgs.Data {
		if o.IsDefault {
			return o, true
		}
	}
	if len(m.Orgs.Data) > 0 {
		return m.Orgs.Data[0], true
	}
	return meOrg{}, false
}

// meTimeout bounds the identity lookup. Profile runs inside the login flow, so
// an unbounded call here would let a slow or hanging /v1/me consume the login's
// own deadline and fail a browser authorisation that had already succeeded.
const meTimeout = 10 * time.Second

func (o *OpenAI) me(ctx context.Context, c provider.Credential) (meResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, meTimeout)
	defer cancel()

	// apiBase is deliberately bare (the core appends the client's path), so the
	// version prefix belongs here — the same trap that once sent every
	// inference request to /v1/v1/responses.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.apiBase()+"/v1/me", nil)
	if err != nil {
		return meResponse{}, err
	}
	o.Authorize(req, c)
	req.Header.Set("Accept", "application/json")

	res, err := o.hc.Do(req)
	if err != nil {
		return meResponse{}, fmt.Errorf("openai: me: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return meResponse{}, fmt.Errorf("openai: me: HTTP %d", res.StatusCode)
	}
	var out meResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return meResponse{}, fmt.Errorf("openai: me: %w", err)
	}
	return out, nil
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
