package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

const (
	// Client id and endpoints are properties of the upstream OAuth deployment.
	ClientID      = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	TokenEndpoint = "https://platform.claude.com/v1/oauth/token"
	AuthorizeURL  = "https://claude.ai/oauth/authorize"
	Scopes        = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// ErrRefreshRejected reports that the refresh token was refused. It is not
// transient: the account needs a fresh login, and retrying wastes a budget.
//
// It wraps provider.ErrCredentialRejected so the account registry can tell a
// dead credential from a network failure without importing this package. Only
// the former may sideline an account.
var ErrRefreshRejected = fmt.Errorf("%w: refresh token rejected", provider.ErrCredentialRejected)

// NormalizeExpiresAt converts a possibly-seconds timestamp to unix millis.
// Values below 1e12 are seconds; anything at or above it is already millis.
func NormalizeExpiresAt(v int64) int64 {
	if v == 0 {
		return 0
	}
	if v < 1e12 {
		return v * 1000
	}
	return v
}

// IsExpired reports whether a credential is past its expiry. An unknown expiry
// (0) is never expired — it means "we were not told", not "assume dead".
//
// expiresAt is assumed already normalized to unix ms, per the Credential
// schema: normalization happens once at the parse boundary (see refreshOnce),
// not here. Re-normalizing here would misclassify small millisecond values as
// seconds under the same <1e12 heuristic NormalizeExpiresAt uses for raw
// upstream input.
func IsExpired(expiresAt int64, now time.Time) bool {
	if expiresAt == 0 {
		return false
	}
	return now.UnixMilli() >= expiresAt
}

// IsExpiringSoon reports whether a credential expires within threshold.
func IsExpiringSoon(expiresAt int64, now time.Time, threshold time.Duration) bool {
	if expiresAt == 0 {
		return false
	}
	return now.Add(threshold).UnixMilli() >= expiresAt
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
}

// RefreshToken exchanges a refresh token for a new access token. Server errors
// and transport failures are retried with backoff; a 4xx is a rejection and
// returns ErrRefreshRejected without retrying.
func RefreshToken(ctx context.Context, hc *http.Client, endpoint, refreshToken string) (provider.Credential, error) {
	const maxAttempts = 3
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return provider.Credential{}, ctx.Err()
			}
		}

		cred, retryable, err := refreshOnce(ctx, hc, endpoint, refreshToken)
		if err == nil {
			return cred, nil
		}
		lastErr = err
		if !retryable {
			return provider.Credential{}, err
		}
	}
	return provider.Credential{}, lastErr
}

func refreshOnce(ctx context.Context, hc *http.Client, endpoint, refreshToken string) (provider.Credential, bool, error) {
	payload, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ClientID,
	})
	if err != nil {
		return provider.Credential{}, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return provider.Credential{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := hc.Do(req)
	if err != nil {
		return provider.Credential{}, true, fmt.Errorf("refresh request: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}()

	if res.StatusCode >= 500 {
		return provider.Credential{}, true, fmt.Errorf("refresh failed with %d", res.StatusCode)
	}
	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return provider.Credential{}, false,
			fmt.Errorf("%w (%d): %s", ErrRefreshRejected, res.StatusCode, bytes.TrimSpace(body))
	}

	var tr tokenResponse
	if err := json.NewDecoder(res.Body).Decode(&tr); err != nil {
		return provider.Credential{}, true, fmt.Errorf("decode refresh response: %w", err)
	}
	if tr.AccessToken == "" {
		return provider.Credential{}, false, fmt.Errorf("%w: no access token in response", ErrRefreshRejected)
	}

	cred := provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    NormalizeExpiresAt(tr.ExpiresAt),
	}
	// An omitted refresh_token means the existing one is still valid; keeping it
	// matters because it is the only way back without a fresh login.
	if cred.RefreshToken == "" {
		cred.RefreshToken = refreshToken
	}
	if cred.ExpiresAt == 0 {
		secs := tr.ExpiresIn
		if secs == 0 {
			secs = 3600
		}
		cred.ExpiresAt = time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
	}
	return cred, false, nil
}
