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

// recordingWaiter satisfies Waiter and logs every wait so admission delays are
// asserted without real time passing. When advance is set, each successful
// Wait call moves the injected clock forward by the requested duration itself,
// so a blocked Admit converges deterministically instead of hot-spinning: with
// advance unset, Wait returns instantly and a caller stuck on a cap or a pause
// would otherwise spin the retry loop as fast as the scheduler allows,
// growing waits without bound until something else moves the clock.
type recordingWaiter struct {
	mu      sync.Mutex
	waits   []time.Duration
	err     error
	advance func(time.Duration)
}

func (w *recordingWaiter) Wait(_ context.Context, d time.Duration) error {
	w.mu.Lock()
	if w.err != nil {
		err := w.err
		w.mu.Unlock()
		return err
	}
	w.waits = append(w.waits, d)
	adv := w.advance
	w.mu.Unlock()
	if adv != nil {
		adv(d)
	}
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

func inFlight(t *testing.T, m *Manager, id string) int {
	t.Helper()
	acc, ok := m.Get(id)
	if !ok {
		t.Fatalf("Get(%q): account not found", id)
	}
	return acc.InFlight
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
	if got := inFlight(t, m, "a"); got != 3 {
		t.Errorf("InFlight = %d, want 3", got)
	}
	if w.count() != 0 {
		t.Errorf("ramp disabled should never wait, waited %v", w.waits)
	}

	m.Release("a")
	if got := inFlight(t, m, "a"); got != 2 {
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
	if got := inFlight(t, m, "a"); got != 0 {
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

	// A third must wait, because the cap is still 2. The waiter advances the
	// clock itself on every wait, so the cap grows deterministically as the
	// retry loop runs, rather than racing an externally-driven clk.advance
	// against a hot-spinning goroutine.
	w.advance = clk.advance
	done := make(chan error, 1)
	go func() { done <- m.Admit(context.Background(), "a", w) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("third admission: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third admission never completed as the ramp cap grew")
	}
	if w.count() == 0 {
		t.Error("third admission should have waited on the ramp cap")
	}
	if got := inFlight(t, m, "a"); got != 3 {
		t.Errorf("InFlight = %d, want 3", got)
	}
}

// The waiter's error must propagate on the ramp-cap branch too, not only on
// the pause branch: a refused wait must not take a slot regardless of which
// admission control triggered it.
func TestAdmitPropagatesWaiterErrorOnRampCapWithoutTakingASlot(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	r := Ramp{Enabled: true, StartConc: 1, StepConc: 1, StepMS: 250, WindowMS: 5000, PollMS: 50}
	m := rampMgr(t, r, clk)
	m.BeginRamp("a")

	if err := m.Admit(context.Background(), "a", &recordingWaiter{}); err != nil {
		t.Fatalf("first Admit: %v", err)
	}
	if got := inFlight(t, m, "a"); got != 1 {
		t.Fatalf("InFlight = %d, want 1", got)
	}

	// The cap is 1, so a second admission must consult the waiter and this one
	// refuses.
	sentinel := errors.New("budget exhausted")
	w := &recordingWaiter{err: sentinel}
	err := m.Admit(context.Background(), "a", w)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the waiter's error", err)
	}
	if got := inFlight(t, m, "a"); got != 1 {
		t.Errorf("InFlight = %d, want still 1 — a refused ramp wait must not take a slot", got)
	}
}

func TestAdmitWaitsWhileAccountIsPaused(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)
	m.PauseAccount("a", 2*time.Second)
	// The waiter advances the clock itself on every wait, so the pause lapses
	// deterministically as Admit's retry loop runs.
	w := &recordingWaiter{advance: clk.advance}

	done := make(chan error, 1)
	go func() { done <- m.Admit(context.Background(), "a", w) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admission did not proceed as the pause lapsed")
	}
	if w.count() == 0 {
		t.Error("admission should have waited on the pause")
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
	if got := inFlight(t, m, "a"); got != 0 {
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

// An id with no matching account must fail immediately rather than wait.
func TestAdmitUnknownAccountReturnsErrNoAccount(t *testing.T) {
	clk := &clock{t: time.Unix(1000, 0)}
	m := rampMgr(t, Ramp{Enabled: false}, clk)

	w := &recordingWaiter{}
	if err := m.Admit(context.Background(), "does-not-exist", w); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	if w.count() != 0 {
		t.Errorf("an unknown account should never wait, waited %v", w.waits)
	}
}
