// Package prober runs a background loop that periodically reads each account's
// quota and model catalogue from its provider, feeding them through
// account.Manager.UpdateQuota — which already fans out to the metrics OnQuota
// hook (spec §6.2, §7.3) — and UpdateModels.
//
// The two reads are independent: neither gates the other, and neither is
// gated on the account's credential TYPE. Whether an endpoint supports a given
// credential is the provider's answer to give (ErrUnsupported), not this
// package's to guess; see probeAll.
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

// Prober periodically reads quota and the model catalogue for every account and
// feeds them through account.Manager. An interval of 0 disables the
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
// reads every account's quota and catalogue; 0 disables that loop.
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
			// One cycle immediately, before the ticker. Waiting a full interval
			// left the proxy with NO quota data for its first interval —
			// buckets are not persisted across restarts, so this is absent
			// data rather than merely stale, and selection cannot apply
			// switchThreshold to an account it knows nothing about. The first
			// requests after a restart could therefore be sent to an account
			// that was already spent.
			//
			// Inside the goroutine and under stopCtx, so Start stays
			// non-blocking and a Stop during this first cycle still cancels it.
			p.runCycle(stopCtx)

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
		// NOT filtered by credential type here. It used to skip anything that
		// was not OAuth, on the grounds that an API key has no usage endpoint
		// (spec §4.4) — true for Quota, and Quota still answers ErrUnsupported
		// for one, which this loop treats as "nothing to read" without logging.
		// But it is NOT true for Models: anthropic.Models works perfectly with
		// an API key (it authenticates the same /v1/models call with x-api-key),
		// so this pre-judgement left an API-key-only deployment with a
		// permanently empty catalogue — /v1/models answered nothing, and an
		// empty catalogue also disables account.servesModel filtering silently,
		// since "unknown" must mean "can serve anything".
		//
		// Deciding which credential types an endpoint supports is the
		// provider's job — it is the only thing that knows — and both providers
		// already return ErrUnsupported when they cannot. The prober asks and
		// honours the answer.
		prov, ok := p.providers[a.Provider]
		if !ok || prov == nil {
			continue
		}
		if !p.eligible(a.ID, now) {
			continue
		}

		// Renew before reading. This loop is the one caller of Quota, and it
		// runs on a timer rather than in response to traffic, so an access
		// token routinely expires while the proxy sits idle with no request to
		// renew it on the inference path. Reading with the stale token answers
		// HTTP 401 — which is not a throttling error, so it carries no backoff
		// and simply fails again every interval. Utilization then stays frozen
		// at its last reading, most visibly across a quota reset, until an
		// inference request happens to refresh the credential on its own path.
		//
		// EnsureFresh is a no-op for a credential that is not near expiry and
		// for an API key, and it coalesces with a concurrent refresh on the
		// request path, so in the common case this costs nothing. Its side
		// effect is worth having in its own right: the probe now keeps
		// credentials warm, so the first request after an idle spell no longer
		// pays for a refresh on the critical path.
		if err := p.mgr.EnsureFresh(ctx, a.ID, false); err != nil {
			p.recordError(a.ID, fmt.Errorf("refresh: %w", err), now)
			p.log.Warn("credential refresh failed before quota probe",
				"account", a.Label, "id", a.ID, "err", err)
			errs = append(errs, fmt.Errorf("probe account %s: refresh: %w", a.ID, err))
			// Nothing to read with: probing on a credential already known to be
			// rejected spends a call to learn what the refresh just reported.
			continue
		}
		// Re-read after EnsureFresh, exactly as the attempt loop does: a is a
		// value copy taken before the refresh, so its Credential is the token
		// that was just superseded.
		if fresh, ok := p.mgr.Get(a.ID); ok {
			a = fresh
		}

		// Quota and the catalogue are read INDEPENDENTLY, on the same cycle and
		// the same freshly renewed credential. Neither gates the other: the
		// quota read used to `continue` on any error, so once wham/usage started
		// failing — which spec §10 explicitly anticipates, it is a private
		// endpoint — the catalogue was never refreshed again even though
		// wham/models was perfectly healthy. They are different endpoints on
		// different hosts and fail for different reasons.
		//
		// The eligible() backoff above still gates BOTH, deliberately: it arms
		// only on ErrQuotaThrottled, and a host that is throttling us is
		// throttling the catalogue endpoint beside it. A plain failure — the
		// case spec §10 anticipates — arms no backoff at all, so the catalogue
		// keeps refreshing every cycle through it; a throttle delays the
		// catalogue by at most maxBackoff rather than stopping it.
		if err := p.probeQuota(ctx, prov, a, now); err != nil {
			errs = append(errs, fmt.Errorf("probe account %s: %w", a.ID, err))
		}
		p.probeModels(ctx, prov, a)
	}
	// An account removed from mgr between cycles otherwise stays in
	// p.accounts (and so in Status().Accounts) forever — a ghost entry for
	// something that no longer exists. Prune against the account set this
	// cycle actually saw.
	p.pruneAccounts(current)
	return errs
}

// probeQuota reads one account's quota and records the outcome. A returned
// error is the cycle's error for this account; nil covers both a successful
// read and a provider that has no usage endpoint for this credential.
func (p *Prober) probeQuota(ctx context.Context, prov provider.Provider, a account.Account, now time.Time) error {
	q, err := prov.Quota(ctx, a.Credential)
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		// No usage endpoint for this credential — an API key, typically (spec
		// §4.4). Neither a success nor a failure: there is nothing to read, so
		// recording an error here would log the same non-event every cycle
		// forever, and recording a success would reset a real backoff.
		return nil
	case err != nil:
		p.recordError(a.ID, err, now)
		// Logged in addition to being recorded in Status(): a headless
		// instance (spec §1: runs `--headless` with logging to stderr) has
		// no UI polling Status at all, so this is the only place a
		// throttled or failing probe is visible for it. err is always
		// either provider.ErrQuotaThrottled or a transport failure, never
		// anything containing credential material (see anthropic's Quota
		// and this package's doc comment on that guarantee).
		p.log.Warn("quota probe failed", "account", a.Label, "id", a.ID, "err", err)
		return err
	}
	p.recordSuccess(a.ID, now)
	// A read that succeeds but names no window is worth saying out loud. It is
	// indistinguishable downstream from never having probed at all —
	// UpdateQuota stores nothing for an empty slice — so the UI reports "no
	// quota reading yet" for an account that was in fact read successfully
	// seconds ago. Upstream genuinely does report no windows sometimes (a plan
	// whose window has not started), but so does a parser that failed to
	// recognise a payload, and the two must not look the same in a log.
	if len(q.Buckets) == 0 {
		p.log.Info("quota read returned no windows",
			"account", a.Label, "provider", a.Provider)
	}
	p.mgr.UpdateQuota(a.ID, q.Buckets)
	return nil
}

// probeModels refreshes one account's catalogue.
//
// A failure is logged but does NOT fail the cycle, is NOT recorded as the
// account's probe error, and does NOT clear the stored catalogue: UpdateModels
// ignores an empty list, so the last known good list survives a bad read.
// Keeping it out of recordError matters — AccountStatus.LastError is the quota
// read's health, and a models failure must not arm the quota backoff.
func (p *Prober) probeModels(ctx context.Context, prov provider.Provider, a account.Account) {
	models, err := prov.Models(ctx, a.Credential)
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		// Nothing to discover for this provider and this credential type.
	case err != nil:
		p.log.Warn("model catalogue read failed", "account", a.Label, "err", err)
	default:
		p.mgr.UpdateModels(a.ID, models)
	}
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
