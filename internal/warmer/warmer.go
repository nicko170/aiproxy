// Package warmer starts an idle account's rate-limit window before that account
// is needed.
//
// Anthropic anchors the 5-hour window to an account's FIRST request, not to a
// wall clock. An account nobody has touched therefore has no window running at
// all — its quota is not ageing toward a reset, it is simply stopped. Observed
// live: two accounts on the same plan reset at 21:00 and 00:20, and a
// model-scoped window that had never been used reported resetsAt=0.
//
// That is a throughput loss whenever traffic is concentrated on one account:
// the standby's window only begins at the moment you fail over onto it, so its
// reset is pushed a full five hours past the point of switching, rather than
// having been counting down all along. Sending one trivial request starts the
// clock, which is worth doing precisely when the account in use is far enough
// through its own window that a handover is coming.
//
// This package SPENDS: it makes a real, billable inference request that no
// client asked for. It is kept out of internal/prober deliberately — that
// package's contract is a zero-spend read of the usage endpoint, and burying a
// paid request inside it would make that promise false.
package warmer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/provider"
)

// bucket5h is the unified five-hour window. Warming targets this one because it
// is the window that actually gates a busy session; the weekly windows are long
// enough that they are effectively always running.
const bucket5h = "5h"

// DefaultThreshold is how far through its own window the busiest account must be
// before a standby is warmed.
//
// Warming earlier costs nothing extra in tokens but commits the standby's window
// sooner, and a window committed is a window expiring — warm at 10% and the
// standby may reset again before the handover ever happens. Half-spent is the
// point where a handover is close enough to be worth preparing for.
const DefaultThreshold = 0.5

// DefaultModel is the cheapest model that still starts the unified window. The
// window does not care which model was used, so this is chosen purely for cost:
// a warm measured 8 input and 1 output token.
const DefaultModel = "claude-haiku-4-5-20251001"

// DefaultInterval is how often the trigger condition is re-checked. This is a
// read of in-memory state, not a request — a cycle sends nothing unless an
// account is both over the threshold and has a standby with a stopped clock.
const DefaultInterval = time.Minute

// failureCooldown holds an account back after a failed warm. Without it a
// permanently failing account is retried every interval forever, which turns one
// broken credential into a steady stream of billable attempts.
const failureCooldown = 15 * time.Minute

// warmBody is the smallest request that still starts a window. Verified against
// the live API: an OAuth token accepts this without the Claude Code system
// prompt, answering 200 for 8 input and 1 output token.
const warmBody = `{"model":%q,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`

// anthropicVersion is required on every Messages request. Normally the client
// supplies it and the proxy forwards it; a warm has no client behind it.
const anthropicVersion = "2023-06-01"

type Options struct {
	Enabled   bool
	Threshold float64
	Model     string
	Interval  time.Duration
	Log       *slog.Logger
	Now       func() time.Time
}

// AccountStatus is one account's warming history, so a warm that is failing is
// visible rather than silently absent.
type AccountStatus struct {
	LastError    string
	LastWarmedAt int64
}

type Status struct {
	Enabled  bool
	Accounts map[string]AccountStatus
}

type Warmer struct {
	mgr       *account.Manager
	providers map[string]provider.Provider
	rt        http.RoundTripper
	opts      Options
	log       *slog.Logger
	now       func() time.Time

	stop      chan struct{}
	stopped   chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	mu    sync.Mutex
	state map[string]*accountState
}

type accountState struct {
	lastError    string
	lastWarmedAt int64
	nextAttempt  time.Time
}

func New(mgr *account.Manager, providers map[string]provider.Provider, rt http.RoundTripper, opts Options) *Warmer {
	if opts.Threshold <= 0 {
		opts.Threshold = DefaultThreshold
	}
	if opts.Model == "" {
		opts.Model = DefaultModel
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Warmer{
		mgr: mgr, providers: providers, rt: rt, opts: opts,
		log: opts.Log, now: opts.Now,
		stop: make(chan struct{}), stopped: make(chan struct{}),
		state: map[string]*accountState{},
	}
}

func (w *Warmer) Start() {
	w.startOnce.Do(func() {
		go func() {
			defer close(w.stopped)
			if !w.opts.Enabled {
				<-w.stop
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-w.stop
				cancel()
			}()

			t := time.NewTicker(w.opts.Interval)
			defer t.Stop()
			for {
				select {
				case <-w.stop:
					return
				case <-t.C:
					if err := w.WarmNow(ctx); err != nil && !errors.Is(err, context.Canceled) {
						w.log.Debug("warm cycle", "err", err)
					}
				}
			}
		}()
	})
}

func (w *Warmer) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.stopped
}

func (w *Warmer) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := Status{Enabled: w.opts.Enabled, Accounts: make(map[string]AccountStatus, len(w.state))}
	for id, st := range w.state {
		out.Accounts[id] = AccountStatus{LastError: st.lastError, LastWarmedAt: st.lastWarmedAt}
	}
	return out
}

// WarmNow runs one cycle regardless of whether the background loop is enabled,
// so the trigger can be exercised on demand.
func (w *Warmer) WarmNow(ctx context.Context) error {
	accts := w.mgr.All()
	if !w.overThreshold(accts) {
		return nil
	}
	cand, ok := w.pick(accts)
	if !ok {
		return nil
	}
	return w.warm(ctx, cand)
}

// overThreshold reports whether any usable account is far enough through its
// five-hour window to justify preparing a standby.
func (w *Warmer) overThreshold(accts []account.Account) bool {
	for _, a := range accts {
		if !w.usable(a) {
			continue
		}
		if b, ok := a.Buckets[bucket5h]; ok && b.Utilization >= w.opts.Threshold {
			return true
		}
	}
	return false
}

// pick chooses the account to warm: usable, its window stopped, and not inside a
// failure cooldown. Ordered by priority then ID so the choice is the one traffic
// would fail over to, and so it does not change between cycles for no reason.
func (w *Warmer) pick(accts []account.Account) (account.Account, bool) {
	now := w.now()
	var cands []account.Account
	for _, a := range accts {
		if !w.usable(a) || w.clockRunning(a) {
			continue
		}
		w.mu.Lock()
		st, seen := w.state[a.ID]
		cooling := seen && now.Before(st.nextAttempt)
		w.mu.Unlock()
		if cooling {
			continue
		}
		cands = append(cands, a)
	}
	if len(cands) == 0 {
		return account.Account{}, false
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Priority != cands[j].Priority {
			return cands[i].Priority < cands[j].Priority
		}
		return cands[i].ID < cands[j].ID
	})
	return cands[0], true
}

// usable excludes accounts a warm could not help or should not touch. API-key
// credentials are skipped for the same reason internal/prober skips them: the
// unified window is an OAuth-plan concept.
func (w *Warmer) usable(a account.Account) bool {
	if a.Disabled || a.Status == account.StatusErrored {
		return false
	}
	if a.Credential.Type != provider.CredentialOAuth {
		return false
	}
	p := w.providers[a.Provider]
	return p != nil
}

// clockRunning reports whether this account's five-hour window has already
// started. A window that has never been used reports no reset time at all, which
// is the signal that warming has something to do.
func (w *Warmer) clockRunning(a account.Account) bool {
	b, ok := a.Buckets[bucket5h]
	return ok && b.ResetsAt != 0
}

func (w *Warmer) warm(ctx context.Context, a account.Account) error {
	prov := w.providers[a.Provider]
	if err := w.mgr.EnsureFresh(ctx, a.ID, false); err != nil {
		w.record(a.ID, fmt.Errorf("refresh: %w", err))
		return err
	}
	// Re-read: the snapshot predates the refresh, so its credential may be the
	// token that was just superseded.
	if fresh, ok := w.mgr.Get(a.ID); ok {
		a = fresh
	}

	pa := a.ToProvider()
	body, err := prov.RewriteBody(fmt.Appendf(nil, warmBody, w.opts.Model), pa)
	if err != nil {
		w.record(a.ID, err)
		return err
	}
	target := strings.TrimSuffix(prov.Endpoint(pa).String(), "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		w.record(a.ID, err)
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.ContentLength = int64(len(body))
	prov.Authorize(req, a.Credential)

	res, err := w.rt.RoundTrip(req)
	if err != nil {
		w.record(a.ID, err)
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))

	// The response carries this account's fresh rate-limit headers, so the new
	// window is recorded immediately rather than waiting for the next probe.
	// This is also what stops the next cycle warming the same account again.
	out := prov.ClassifyResponse(res)
	w.mgr.UpdateQuota(a.ID, out.Buckets)

	if res.StatusCode != http.StatusOK {
		err := fmt.Errorf("warm %s: HTTP %d", a.Label, res.StatusCode)
		w.record(a.ID, err)
		return err
	}
	w.record(a.ID, nil)
	w.log.Info("warmed an idle account's rate-limit window",
		"account", a.Label, "model", w.opts.Model)
	return nil
}

func (w *Warmer) record(id string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.state[id]
	if !ok {
		st = &accountState{}
		w.state[id] = st
	}
	if err != nil {
		st.lastError = err.Error()
		st.nextAttempt = w.now().Add(failureCooldown)
		return
	}
	st.lastError = ""
	st.lastWarmedAt = w.now().UnixMilli()
	st.nextAttempt = time.Time{}
}
