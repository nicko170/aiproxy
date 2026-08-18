// Package prober runs a background loop that periodically reads each OAuth
// account's quota from its provider and feeds the result through
// account.Manager.UpdateQuota, which already fans out to the metrics OnQuota
// hook (spec §6.2, §7.3).
//
// It is its own package rather than another file in internal/account because
// a periodic loop with its own goroutine, ticker, and per-account backoff
// state is a distinct concern from account bookkeeping: account.Manager
// already has no dependency on a scheduler, only an injectable clock, and it
// should not gain one just to host a loop only cmd/aiproxy ever starts. The
// prober depends on account.Manager and the provider registry cmd/aiproxy
// already builds; account must not depend back on it.
package prober

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/provider"
)

// defaultBaseBackoff and defaultMaxBackoff bound the exponential backoff
// applied to an account whose quota read comes back throttled
// (provider.ErrQuotaThrottled). The predecessor tool polled every 30s with no
// backoff at all and had its probe permanently throttled — quota data went
// stale and account selection decided on fiction. 30s doubling up to 30
// minutes gives a throttled account room to recover without staying dark
// forever.
const (
	defaultBaseBackoff = 30 * time.Second
	defaultMaxBackoff  = 30 * time.Minute
)

// Option configures a Prober at construction.
type Option func(*Prober)

// WithClock overrides the prober's notion of "now", so backoff and status
// timestamps are deterministic in tests instead of racing the wall clock.
func WithClock(now func() time.Time) Option { return func(p *Prober) { p.now = now } }

// WithLogger overrides the prober's logger. Nil (the default) uses
// slog.Default().
func WithLogger(log *slog.Logger) Option { return func(p *Prober) { p.log = log } }

// WithBackoff overrides the base and max exponential backoff applied to a
// throttled account.
func WithBackoff(base, max time.Duration) Option {
	return func(p *Prober) { p.baseBackoff = base; p.maxBackoff = max }
}

// AccountStatus is one account's probe health, so a throttled probe is
// visible rather than silently stale (spec §6.2).
type AccountStatus struct {
	// LastError is the most recent Quota error for this account, or "" if
	// the last attempt (or every attempt so far) succeeded.
	LastError string
	// LastSuccessAt is unix ms of the last successful read, or 0 if there has
	// never been one.
	LastSuccessAt int64
	// NextAttemptAt is unix ms before which this account is skipped due to
	// backoff, or 0 when it is eligible right now (no backoff in effect).
	NextAttemptAt int64
}

// Status is the prober's overall health at a point in time.
type Status struct {
	Running         bool
	LastStartedAt   int64
	LastCompletedAt int64
	Accounts        map[string]AccountStatus
}

// Prober periodically reads quota for every OAuth account and feeds it
// through account.Manager.UpdateQuota. An interval of 0 disables the
// background loop entirely; ProbeNow still works on demand regardless (spec:
// "An interval of 0 disables it" refers to the periodic schedule, not a
// manual trigger).
type Prober struct {
	mgr       *account.Manager
	providers map[string]provider.Provider
	interval  time.Duration
	now       func() time.Time
	log       *slog.Logger

	baseBackoff, maxBackoff time.Duration

	stop      chan struct{}
	stopped   chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	mu              sync.Mutex
	running         bool
	doneCh          chan struct{}
	lastStartedAt   int64
	lastCompletedAt int64
	accounts        map[string]*accountState
}

type accountState struct {
	backoff       time.Duration
	nextAttempt   time.Time
	lastError     string
	lastSuccessAt int64
}

// New builds a Prober over mgr and the same provider registry cmd/aiproxy
// already constructed. interval is how often the background loop (Start)
// reads every OAuth account's quota; 0 disables that loop.
func New(mgr *account.Manager, providers map[string]provider.Provider, interval time.Duration, opts ...Option) *Prober {
	p := &Prober{
		mgr: mgr, providers: providers, interval: interval,
		now: time.Now, log: slog.Default(),
		baseBackoff: defaultBaseBackoff, maxBackoff: defaultMaxBackoff,
		stop: make(chan struct{}), stopped: make(chan struct{}),
		accounts: map[string]*accountState{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Start begins the background loop, if configured with a positive interval.
// It always spawns its own goroutine (even when disabled) so Stop is always
// safe to call afterward, mirroring metrics.Roller's Start/Stop convention.
//
// Guarded by startOnce: a duplicate Start call is a no-op rather than
// spawning a second loop racing the first one's ticks against the same
// account state. metrics.Roller has the identical double-call hazard but
// documents the "call it once" contract instead of enforcing it (see its
// Start doc comment for why); this package chose sync.Once so the failure
// mode of a second call is silence, not a second competing goroutine.
func (p *Prober) Start() {
	p.startOnce.Do(func() {
		go func() {
			defer close(p.stopped)
			// stopCtx is cancelled the instant p.stop closes, and is what
			// every cycle this loop runs is given instead of
			// context.Background() — see runCycle/probeAll's ctx parameter
			// and Stop's doc comment for why: without this, Stop had to wait
			// for whatever Quota call happened to be in flight to finish on
			// its own, which could stall process shutdown for however long
			// that call takes (its own timeout is per-account, not
			// per-cycle).
			stopCtx, cancelStopCtx := context.WithCancel(context.Background())
			defer cancelStopCtx()
			go func() {
				<-p.stop
				cancelStopCtx()
			}()

			if p.interval <= 0 {
				<-p.stop
				return
			}
			t := time.NewTicker(p.interval)
			defer t.Stop()
			for {
				select {
				case <-p.stop:
					return
				case <-t.C:
					p.runCycle(stopCtx)
				}
			}
		}()
	})
}

// Stop ends the background loop and waits for it to exit. Must be paired
// with a prior Start, exactly like metrics.Roller.
//
// Guarded by stopOnce so a duplicate Stop call cannot panic by closing
// p.stop twice; every call, duplicate or not, still blocks until the loop
// has actually exited (reading from an already-closed p.stopped returns
// immediately, so this is safe to call any number of times).
func (p *Prober) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.stopped
}

// ProbeNow triggers one out-of-band probe cycle, independent of whether the
// background loop is running or even configured (interval 0). If a cycle is
// already running — the background loop's or another ProbeNow's — this
// waits for it to finish rather than starting a second one concurrently, so
// overlapping cycles never stack (a real failure mode: a manual trigger
// racing a tick would otherwise double the load on an already-struggling
// usage endpoint at the exact moment backoff matters most).
func (p *Prober) ProbeNow(ctx context.Context) error {
	return p.runCycle(ctx)
}

// Status returns a snapshot of the prober's health.
func (p *Prober) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := Status{
		Running:         p.running,
		LastStartedAt:   p.lastStartedAt,
		LastCompletedAt: p.lastCompletedAt,
		Accounts:        make(map[string]AccountStatus, len(p.accounts)),
	}
	for id, st := range p.accounts {
		var next int64
		if !st.nextAttempt.IsZero() {
			next = st.nextAttempt.UnixMilli()
		}
		out.Accounts[id] = AccountStatus{
			LastError: st.lastError, LastSuccessAt: st.lastSuccessAt, NextAttemptAt: next,
		}
	}
	return out
}

// runCycle reads quota for every eligible OAuth account once. If another
// cycle is already running, it joins that cycle (waiting on the same done
// signal) instead of running a second one, honoring ctx while it waits.
func (p *Prober) runCycle(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		done := p.doneCh
		p.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.running = true
	done := make(chan struct{})
	p.doneCh = done
	p.lastStartedAt = p.now().UnixMilli()
	p.mu.Unlock()

	errs := p.probeAll(ctx)

	p.mu.Lock()
	p.running = false
	p.lastCompletedAt = p.now().UnixMilli()
	p.mu.Unlock()
	close(done)

	return errors.Join(errs...)
}

func (p *Prober) probeAll(ctx context.Context) []error {
	now := p.now()
	var errs []error
	live := p.mgr.All()
	current := make(map[string]bool, len(live))
	for _, a := range live {
		current[a.ID] = true
		// API-key accounts have no usage endpoint at all (spec §4.4); skip
		// them without even asking the provider, so this never logs a
		// spurious error every cycle for an account that can never answer.
		if a.Credential.Type != provider.CredentialOAuth {
			continue
		}
		prov, ok := p.providers[a.Provider]
		if !ok || prov == nil {
			continue
		}
		if !p.eligible(a.ID, now) {
			continue
		}

		q, err := prov.Quota(ctx, a.Credential)
		if err != nil {
			if errors.Is(err, provider.ErrUnsupported) {
				// Defensive: the credential-type check above already filters
				// these out for every provider that follows the same
				// convention as anthropic's.
				continue
			}
			p.recordError(a.ID, err, now)
			// Logged in addition to being recorded in Status(): a headless
			// instance (spec §1: runs `--headless` with logging to stderr) has
			// no UI polling Status at all, so this is the only place a
			// throttled or failing probe is visible for it. err is always
			// either provider.ErrQuotaThrottled or a transport failure, never
			// anything containing credential material (see anthropic's Quota
			// and this package's doc comment on that guarantee).
			p.log.Warn("quota probe failed", "account", a.Label, "id", a.ID, "err", err)
			errs = append(errs, fmt.Errorf("probe account %s: %w", a.ID, err))
			continue
		}
		p.recordSuccess(a.ID, now)
		p.mgr.UpdateQuota(a.ID, q.Buckets)
	}
	// An account removed from mgr between cycles otherwise stays in
	// p.accounts (and so in Status().Accounts) forever — a ghost entry for
	// something that no longer exists. Prune against the account set this
	// cycle actually saw.
	p.pruneAccounts(current)
	return errs
}

// pruneAccounts drops every entry in p.accounts whose id is not in current,
// so Status().Accounts never outlives the account it describes.
func (p *Prober) pruneAccounts(current map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.accounts {
		if !current[id] {
			delete(p.accounts, id)
		}
	}
}

func (p *Prober) eligible(id string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.accounts[id]
	if !ok {
		return true
	}
	return !now.Before(st.nextAttempt)
}

// recordError records the failure and, only when it is a throttling error
// (provider.ErrQuotaThrottled), applies or doubles this account's exponential
// backoff. A non-throttle error (a network blip, a transient 5xx) is
// recorded but does not change NextAttemptAt: the account is simply retried
// at the ordinary cadence next cycle, since that is a different failure mode
// than being rate-limited and does not call for backing off the same way.
func (p *Prober) recordError(id string, err error, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stateLocked(id)
	st.lastError = err.Error()
	if !errors.Is(err, provider.ErrQuotaThrottled) {
		return
	}
	if st.backoff <= 0 {
		st.backoff = p.baseBackoff
	} else {
		st.backoff *= 2
		if st.backoff > p.maxBackoff {
			st.backoff = p.maxBackoff
		}
	}
	st.nextAttempt = now.Add(st.backoff)
}

// recordSuccess clears any error and resets backoff entirely — a single
// successful read is enough to trust the endpoint again, per spec §6.2
// ("backs off exponentially ... reset on success").
func (p *Prober) recordSuccess(id string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stateLocked(id)
	st.lastError = ""
	st.backoff = 0
	st.nextAttempt = time.Time{}
	st.lastSuccessAt = now.UnixMilli()
}

func (p *Prober) stateLocked(id string) *accountState {
	st, ok := p.accounts[id]
	if !ok {
		st = &accountState{}
		p.accounts[id] = st
	}
	return st
}
