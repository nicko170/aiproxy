package account

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// refreshThreshold is how far ahead of expiry a credential is renewed.
const refreshThreshold = 5 * time.Minute

// Options configures a Manager. Persist is called after a successful refresh so
// the rotated credential survives a restart; Now is injectable for tests.
type Options struct {
	Persist         func(id string, c provider.Credential) error
	Now             func() time.Time
	SwitchThreshold float64
	SessionAffinity bool
	Ramp            Ramp
}

// refreshCall is one in-flight refresh other callers wait on.
type refreshCall struct {
	done chan struct{}
	err  error
}

type Manager struct {
	mu        sync.Mutex
	accounts  []*Account
	byID      map[string]*Account
	providers map[string]provider.Provider
	opts      Options

	refreshing map[string]*refreshCall
	// affinity maps a client session id to the account that served it.
	affinity map[string]string
}

func New(accts []config.Account, providers map[string]provider.Provider, opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SwitchThreshold <= 0 {
		opts.SwitchThreshold = 0.98
	}
	opts.Ramp = opts.Ramp.withDefaults()

	m := &Manager{
		byID:       map[string]*Account{},
		providers:  providers,
		opts:       opts,
		refreshing: map[string]*refreshCall{},
		affinity:   map[string]string{},
	}
	for _, c := range accts {
		a := fromConfig(c)
		m.accounts = append(m.accounts, a)
		m.byID[a.ID] = a
	}
	return m
}

func (m *Manager) Get(id string) *Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[id]
}

// All returns the live account pointers. Callers must not mutate them.
func (m *Manager) All() []*Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Account, len(m.accounts))
	copy(out, m.accounts)
	return out
}

// Snapshot returns value copies safe to hand to a UI.
func (m *Manager) Snapshot() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		copyAcct := *a
		copyAcct.Buckets = make(map[string]provider.QuotaBucket, len(a.Buckets))
		for k, v := range a.Buckets {
			copyAcct.Buckets[k] = v
		}
		out = append(out, copyAcct)
	}
	return out
}

// EnsureFresh renews an account's credential when it is expired or close to it,
// or unconditionally when force is set.
//
// Concurrent callers for the same account are coalesced into a single refresh.
// Without that, a burst of requests produces a burst of refreshes; the upstream
// rotates the refresh token on each one, so every attempt but one is left
// holding a token that has already been superseded.
func (m *Manager) EnsureFresh(ctx context.Context, id string, force bool) error {
	m.mu.Lock()
	a := m.byID[id]
	if a == nil {
		m.mu.Unlock()
		return fmt.Errorf("unknown account %q", id)
	}
	// An API key does not expire, so there is nothing to refresh.
	if a.Credential.Type != provider.CredentialOAuth || a.Credential.RefreshToken == "" {
		m.mu.Unlock()
		return nil
	}
	if !force && !m.needsRefreshLocked(a) {
		m.mu.Unlock()
		return nil
	}
	if call, ok := m.refreshing[id]; ok {
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.refreshing[id] = call
	p := m.providers[a.Provider]
	cred := a.Credential
	m.mu.Unlock()

	var err error
	if p == nil {
		err = fmt.Errorf("no provider %q for account %q", a.Provider, id)
	} else {
		var next provider.Credential
		next, err = p.Refresh(ctx, cred)
		if err == nil {
			m.mu.Lock()
			a.Credential = next
			a.Status = StatusActive
			a.LastError = ""
			m.mu.Unlock()
			if m.opts.Persist != nil {
				err = m.opts.Persist(id, next)
			}
		}
	}
	if err != nil {
		m.mu.Lock()
		a.Status = StatusErrored
		a.LastError = err.Error()
		m.mu.Unlock()
	}

	m.mu.Lock()
	delete(m.refreshing, id)
	m.mu.Unlock()

	call.err = err
	close(call.done)
	return err
}

func (m *Manager) needsRefreshLocked(a *Account) bool {
	if a.Credential.ExpiresAt == 0 {
		return false // no expiry known; do not churn
	}
	now := m.opts.Now()
	return now.Add(refreshThreshold).UnixMilli() >= a.Credential.ExpiresAt
}

// UpdateQuota records observed buckets for an account.
func (m *Manager) UpdateQuota(id string, buckets []provider.QuotaBucket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return
	}
	if a.Buckets == nil {
		a.Buckets = map[string]provider.QuotaBucket{}
	}
	for _, b := range buckets {
		a.Buckets[b.Name] = b
	}
}
