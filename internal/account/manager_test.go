package account

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// stubProvider counts Refresh calls and can be made slow, so coalescing is
// observable.
type stubProvider struct {
	refreshes atomic.Int32
	delay     time.Duration
	err       error
}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	s.refreshes.Add(1)
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.err != nil {
		return provider.Credential{}, s.err
	}
	return provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  "refreshed",
		RefreshToken: "rt-next",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}, nil
}

func (s *stubProvider) Profile(context.Context, provider.Credential) (provider.Profile, error) {
	return provider.Profile{}, provider.ErrUnsupported
}
func (s *stubProvider) Quota(context.Context, provider.Credential) (provider.Quota, error) {
	return provider.Quota{}, provider.ErrUnsupported
}
func (s *stubProvider) Endpoint(provider.Account) *url.URL {
	u, _ := url.Parse("https://upstream.invalid")
	return u
}
func (s *stubProvider) Authorize(*http.Request, provider.Credential)             {}
func (s *stubProvider) RewriteBody(b []byte, _ provider.Account) ([]byte, error) { return b, nil }
func (s *stubProvider) ClassifyResponse(*http.Response) provider.Outcome         { return provider.Outcome{} }
func (s *stubProvider) ParseUsage([]byte) (*provider.UsageDelta, bool)           { return nil, false }

func newTestManager(t *testing.T, p *stubProvider, accts ...config.Account) (*Manager, *[]provider.Credential) {
	t.Helper()
	var mu sync.Mutex
	persisted := []provider.Credential{}
	m := New(accts, map[string]provider.Provider{"stub": p}, Options{
		SwitchThreshold: 0.98,
		Persist: func(_ string, c provider.Credential) error {
			mu.Lock()
			defer mu.Unlock()
			persisted = append(persisted, c)
			return nil
		},
	})
	return m, &persisted
}

func expiredOAuth() provider.Credential {
	return provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  "stale",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
	}
}

func TestEnsureFreshRefreshesExpiredCredentialAndPersists(t *testing.T) {
	p := &stubProvider{}
	m, persisted := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got := m.Get("a").Credential.AccessToken; got != "refreshed" {
		t.Errorf("AccessToken = %q, want refreshed", got)
	}
	if n := len(*persisted); n != 1 {
		t.Errorf("persisted %d times, want 1", n)
	}
}

func TestEnsureFreshSkipsValidCredential(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "good", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0 for a valid credential", n)
	}
}

func TestEnsureFreshForceRefreshesValidCredential(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "good", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	})

	if err := m.EnsureFresh(context.Background(), "a", true); err != nil {
		t.Fatalf("EnsureFresh(force): %v", err)
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want 1", n)
	}
}

// Several concurrent requests on one account must produce ONE refresh. Without
// coalescing, a burst turns into a burst of refreshes, and the upstream rotates
// the refresh token under itself so all but one attempt fail.
func TestEnsureFreshCoalescesConcurrentCallers(t *testing.T) {
	p := &stubProvider{delay: 50 * time.Millisecond}
	m, persisted := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.EnsureFresh(context.Background(), "a", false)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1", n)
	}
	if n := len(*persisted); n != 1 {
		t.Errorf("persisted %d times, want 1", n)
	}
}

func TestEnsureFreshMarksAccountErroredOnRejection(t *testing.T) {
	p := &stubProvider{err: errors.New("invalid_grant")}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err == nil {
		t.Fatal("expected the refresh error to propagate")
	}
	if got := m.Get("a").Status; got != StatusErrored {
		t.Errorf("Status = %v, want StatusErrored", got)
	}
}

func TestEnsureFreshIgnoresAPIKeyCredentials(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk"},
	})

	if err := m.EnsureFresh(context.Background(), "a", true); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0 — an API key does not expire", n)
	}
}
