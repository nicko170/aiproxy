package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// refreshThreshold is how far ahead of expiry a credential is renewed.
const refreshThreshold = 5 * time.Minute

// defaultRefreshTimeout bounds one credential refresh end to end. The provider's
// own retry and backoff sit inside this, so it caps the whole exchange rather
// than a single HTTP attempt. Without it, a token endpoint that accepts a
// connection and then never answers holds the refresh — and every request
// waiting on it — for as long as it likes.
const defaultRefreshTimeout = 30 * time.Second

// Options configures a Manager. Persist is called after a successful refresh so
// the rotated credential survives a restart; Now is injectable for tests.
type Options struct {
	Persist         func(id string, c provider.Credential) error
	Now             func() time.Time
	SwitchThreshold float64
	SessionAffinity bool
	Ramp            Ramp
	// RefreshTimeout bounds one refresh. Zero takes defaultRefreshTimeout.
	RefreshTimeout time.Duration
	// Log receives the manager's own diagnostics. Nil takes slog.Default().
	Log *slog.Logger
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
	if opts.RefreshTimeout <= 0 {
		opts.RefreshTimeout = defaultRefreshTimeout
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
//
// A refresh's lifetime is deliberately separate from any one caller's wait. The
// refresh itself runs on a background context bounded by Options.RefreshTimeout,
// while each caller — leader and follower alike — waits only as long as its own
// ctx allows. Two consequences, both wanted:
//
//   - A caller that gives up does not cancel a refresh other callers are waiting
//     on, and does not destroy work the next request would otherwise repeat. The
//     refresh runs to completion and the next caller finds the result already
//     there, which is the entire point of coalescing.
//   - A caller can bound its own wait. The proxy passes a context derived from
//     the request's remaining budget, so a slow token endpoint can no longer
//     spend more than that budget before the client hears something.
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
		return waitForRefresh(ctx, call)
	}
	call := &refreshCall{done: make(chan struct{})}
	m.refreshing[id] = call
	p := m.providers[a.Provider]
	cred := a.Credential
	timeout := m.opts.RefreshTimeout
	m.mu.Unlock()

	// Detached deliberately: see the note above. The leader waits on the result
	// exactly as a follower does, so it can abandon its wait without abandoning
	// the refresh.
	go m.runRefresh(id, a, p, cred, timeout, call)

	return waitForRefresh(ctx, call)
}

// waitForRefresh blocks until the in-flight refresh finishes or the caller's own
// context expires, whichever comes first. On expiry the refresh is left running.
func waitForRefresh(ctx context.Context, call *refreshCall) error {
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runRefresh performs one refresh and publishes its result to every waiter.
//
// It runs on its own goroutine under a background context, so no caller's
// cancellation can kill it. A panic is recovered and converted to an error
// rather than propagated: there is no longer an http.Handler on this stack for
// net/http to recover on, so an unrecovered panic here would take the whole
// process down with every other in-flight request.
func (m *Manager) runRefresh(id string, a *Account, p provider.Provider, cred provider.Credential, timeout time.Duration, call *refreshCall) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var err error
	// Cleanup runs on every exit path, including a recovered panic: without it,
	// m.refreshing[id] is left poisoned and every later EnsureFresh for this
	// account blocks until its context expires, since nothing closes call.done.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("refresh panicked for account %q: %v", id, r)
			m.mu.Lock()
			a.Status = StatusErrored
			a.LastError = err.Error()
			m.mu.Unlock()
		}
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
			// A failed Persist is NOT a failed refresh. The credential is good and
			// already in memory, so the account keeps serving; sidelining it here
			// threw away a working credential over a disk problem. It is logged
			// loudly and recorded on the account because the consequence is real:
			// the rotated token is lost on the next restart, and the refresh token
			// it replaced may already have been invalidated upstream.
			if m.opts.Persist != nil {
				if perr := m.opts.Persist(id, next); perr != nil {
					m.log().Error("could not persist a rotated credential; it is live "+
						"in memory but WILL BE LOST on restart",
						"account", a.Label, "id", id, "err", perr)
					m.mu.Lock()
					a.LastError = "persist rotated credential: " + perr.Error()
					m.mu.Unlock()
				}
			}
		}
	}
	if err != nil {
		m.mu.Lock()
		a.LastError = err.Error()
		// Only a genuine credential rejection sidelines an account. Spec §11:
		// transport errors are not credential errors. StatusErrored makes an
		// account ineligible in Select, and only a SUCCESSFUL refresh clears it —
		// which can now only be reached through Select. So marking one on a DNS
		// blip removed the account until the process restarted, and with two
		// accounts the proxy answered 429 forever. Leaving the status alone means
		// the next request simply tries again.
		if errors.Is(err, provider.ErrCredentialRejected) {
			a.Status = StatusErrored
		}
		m.mu.Unlock()
	}
}

// log returns the manager's logger, defaulting to slog's so a Manager built
// without one still reports a lost credential rather than swallowing it.
func (m *Manager) log() *slog.Logger {
	if m.opts.Log != nil {
		return m.opts.Log
	}
	return slog.Default()
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
