package account

import (
	"context"
	"errors"
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

// Get returns a value copy of one account, safe to read without the lock. ok
// is false when no account has that id.
func (m *Manager) Get(id string) (Account, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byID[id]
	if !ok {
		return Account{}, false
	}
	return copyAccount(a), true
}

// All returns value copies of every account, safe to read without the lock.
func (m *Manager) All() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		out = append(out, copyAccount(a))
	}
	return out
}

// Snapshot returns value copies safe to hand to a UI. It is currently
// identical to All; kept as a distinct name because the two serve different
// callers and are free to diverge later.
func (m *Manager) Snapshot() []Account {
	return m.All()
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
	// A prior attempt already failed and the credential is still expired: a
	// retry would just fail the same way. Without this, a burst of sequential
	// callers against a dead refresh token each pays the full 3-attempt retry
	// in RefreshToken. force bypasses this, so a caller can still ask for a
	// fresh attempt deliberately.
	if !force && a.Status == StatusErrored && m.isExpiredLocked(a) {
		err := errors.New(a.LastError)
		m.mu.Unlock()
		return err
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
	// Cleanup runs on every exit path, including a panic unwinding through
	// p.Refresh or Persist: without it, m.refreshing[id] is left poisoned and
	// every later EnsureFresh for this account blocks until its context
	// expires, since nothing ever closes call.done.
	defer func() {
		call.err = err
		m.mu.Lock()
		delete(m.refreshing, id)
		m.mu.Unlock()
		close(call.done)
	}()

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
	return err
}

func (m *Manager) needsRefreshLocked(a *Account) bool {
	if a.Credential.ExpiresAt == 0 {
		return false // no expiry known; do not churn
	}
	now := m.opts.Now()
	return now.Add(refreshThreshold).UnixMilli() >= a.Credential.ExpiresAt
}

// isExpiredLocked reports whether the credential's expiry has actually
// passed, as opposed to needsRefreshLocked's "within threshold" definition.
// Used to decide whether a prior failure is still current: a credential that
// is merely approaching expiry deserves a fresh attempt, but one that is
// already expired and was already rejected will fail again identically.
func (m *Manager) isExpiredLocked(a *Account) bool {
	if a.Credential.ExpiresAt == 0 {
		return false
	}
	return m.opts.Now().UnixMilli() >= a.Credential.ExpiresAt
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
