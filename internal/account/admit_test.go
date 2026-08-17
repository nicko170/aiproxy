package account

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// recordingWaiter satisfies Waiter, returns immediately, and logs every wait so
// admission delays are asserted without real time passing.
type recordingWaiter struct {
	mu    sync.Mutex
	waits []time.Duration
	err   error
}

func (w *recordingWaiter) Wait(_ context.Context, d time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.waits = append(w.waits, d)
	return nil
}

func (w *recordingWaiter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.waits)
}

// clock is a manually advanced clock for ramp arithmetic.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func rampMgr(t *testing.T, r Ramp, clk *clock) *Manager {
	t.Helper()
	return New(
		[]config.Account{{ID: "a", Provider: "stub", Credential: oauthCred()}},
		map[string]provider.Provider{"stub": &stubProvider{}},
		Options{SwitchThreshold: 0.98, Ramp: r, Now: clk.now},
	)
}

func TestAdmitTakesAndReleasesSlots(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	w := &recordingWaiter{}

	for i := 0; i < 3; i++ {
		if err := m.Admit(context.Background(), "a", w); err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
	}
	if got := m.Get("a").InFlight; got != 3 {
		t.Errorf("InFlight = %d, want 3", got)
	}
	if w.count() != 0 {
		t.Errorf("ramp disabled should never wait, waited %v", w.waits)
	}

	m.Release("a")
	if got := m.Get("a").InFlight; got != 2 {
		t.Errorf("InFlight after Release = %d, want 2", got)
	}
}

// Release must not drive the counter negative; a double release would otherwise
// grant a phantom slot to the next caller.
func TestReleaseFloorsAtZero(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)

	m.Release("a")
	m.Release("a")
	if got := m.Get("a").InFlight; got != 0 {
		t.Errorf("InFlight = %d, want 0", got)
	}
}

func TestAdmitEnforcesRampCapThenGrowsWithTime(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	r := Ramp{Enabled: true, StartConc: 2, StepConc: 1, StepMS: 250, WindowMS: 5000, PollMS: 50}
	m := rampMgr(t, r, clk)
	m.BeginRamp("a")
	w := &recordingWaiter{}

	// Two slots are available at the start of the window.
	for i := 0; i < 2; i++ {
		if err := m.Admit(context.Background(), "a", w); err != nil {
			t.Fatalf("Admit %d: %v", i, err)
		}
	}
	if w.count() != 0 {
		t.Fatalf("first two admissions should not wait, waited %v", w.waits)
	}

	// A third must wait, because the cap is still 2. The waiter returns
	// immediately, so grow the cap by advancing the clock before it retries.
	done := make(chan error, 1)
	go func() { done <- m.Admit(context.Background(), "a", w) }()

	deadline := time.After(2 * time.Second)
	for w.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("third admission never waited on the ramp cap")
		case err := <-done:
			t.Fatalf("third admission returned %v without waiting", err)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	clk.advance(300 * time.Millisecond) // one step: cap becomes 3

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("third admission: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third admission did not proceed after the cap grew")
	}
	if got := m.Get("a").InFlight; got != 3 {
		t.Errorf("InFlight = %d, want 3", got)
	}
}

func TestAdmitWaitsWhileAccountIsPaused(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	m.PauseAccount("a", 2*time.Second)
	w := &recordingWaiter{}

	done := make(chan error, 1)
	go func() { done <- m.Admit(context.Background(), "a", w) }()

	deadline := time.After(2 * time.Second)
	for w.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("admission did not wait on the pause")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	clk.advance(3 * time.Second) // pause lapses

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission did not proceed after the pause lapsed")
	}
}

// The budget owns all non-transferring time. When it refuses a wait, admission
// must give up rather than block: a paused account cannot be allowed to stall a
// request past its deadline.
func TestAdmitPropagatesWaiterErrorWithoutTakingASlot(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	m.PauseAccount("a", time.Hour)

	sentinel := errors.New("budget exhausted")
	w := &recordingWaiter{err: sentinel}

	err := m.Admit(context.Background(), "a", w)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the waiter's error", err)
	}
	if got := m.Get("a").InFlight; got != 0 {
		t.Errorf("InFlight = %d, want 0 — a refused admission must not hold a slot", got)
	}
}

func TestAdmitHonoursContextCancellation(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	m.PauseAccount("a", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.Admit(ctx, "a", &recordingWaiter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
