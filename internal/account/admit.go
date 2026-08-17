package account

import (
	"context"
	"time"
)

// Waiter bounds how long admission may block. The proxy passes the request's
// budget, so a pause or a ramp cap can never keep a client waiting past its
// deadline — every delay is drawn from the same allowance.
type Waiter interface {
	Wait(ctx context.Context, d time.Duration) error
}

// Ramp paces requests onto an account that has just become current, so a fleet
// of agents failing over at the same instant does not arrive as one burst,
// trip a rate limit, and cascade onward to the next account.
type Ramp struct {
	Enabled   bool
	StartConc int // concurrent requests allowed at the start of the window
	StepConc  int // additional requests allowed per step
	StepMS    int // step length
	WindowMS  int // after this, the cap is lifted entirely
	PollMS    int // how often a blocked caller re-checks
}

func (r Ramp) withDefaults() Ramp {
	if r.StartConc <= 0 {
		r.StartConc = 2
	}
	if r.StepConc <= 0 {
		r.StepConc = 1
	}
	if r.StepMS <= 0 {
		r.StepMS = 250
	}
	if r.WindowMS <= 0 {
		r.WindowMS = 5000
	}
	if r.PollMS <= 0 {
		r.PollMS = 50
	}
	return r
}

// BeginRamp starts a pacing window for an account that just became current.
func (m *Manager) BeginRamp(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil && m.opts.Ramp.Enabled {
		a.RampStartedAt = m.opts.Now().UnixMilli()
	}
}

// PauseAccount makes new admissions wait for d without removing the account
// from selection. This is the response to a rate limit that came with a usable
// hint: concurrent requests queue rather than piling on, and the account keeps
// serving the moment the hint lapses.
//
// The ramp is armed to begin when the pause ends, so the queued requests trickle
// out instead of arriving together the instant the pause lifts.
func (m *Manager) PauseAccount(id string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return
	}
	until := m.opts.Now().Add(d).UnixMilli()
	if until > a.PausedUntil {
		a.PausedUntil = until
	}
	if m.opts.Ramp.Enabled {
		a.RampStartedAt = a.PausedUntil
	}
}

// rampCapLocked is how many concurrent requests the account currently allows.
// A lifted cap is reported as -1, meaning unlimited.
func (m *Manager) rampCapLocked(a *Account, nowMS int64) int {
	r := m.opts.Ramp
	if !r.Enabled || a.RampStartedAt == 0 {
		return -1
	}
	elapsed := nowMS - a.RampStartedAt
	if elapsed < 0 {
		// The window is armed in the future by PauseAccount; treat it as the
		// start of the window rather than a negative cap.
		elapsed = 0
	}
	if elapsed >= int64(r.WindowMS) {
		a.RampStartedAt = 0
		return -1
	}
	return r.StartConc + int(elapsed/int64(r.StepMS))*r.StepConc
}

// Admit reserves a slot on an account, waiting while the account is paused or at
// its ramp cap. Every wait goes through w, so the request's budget governs the
// total. On success the caller must pair it with exactly one Release.
func (m *Manager) Admit(ctx context.Context, id string, w Waiter) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		a := m.byID[id]
		if a == nil {
			m.mu.Unlock()
			return ErrNoAccount
		}
		nowMS := m.opts.Now().UnixMilli()

		if a.PausedUntil > nowMS {
			remaining := time.Duration(a.PausedUntil-nowMS) * time.Millisecond
			poll := time.Duration(m.opts.Ramp.PollMS*4) * time.Millisecond
			m.mu.Unlock()
			if err := w.Wait(ctx, min(remaining, poll)); err != nil {
				return err
			}
			continue
		}

		cap := m.rampCapLocked(a, nowMS)
		if cap < 0 || a.InFlight < cap {
			a.InFlight++
			m.mu.Unlock()
			return nil
		}
		m.mu.Unlock()

		if err := w.Wait(ctx, time.Duration(m.opts.Ramp.PollMS)*time.Millisecond); err != nil {
			return err
		}
	}
}

// Release returns a slot taken by Admit. It floors at zero: a double release
// must not grant a phantom slot to the next caller.
func (m *Manager) Release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil && a.InFlight > 0 {
		a.InFlight--
	}
}
