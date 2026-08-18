package updater

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// defaultInitialDelay is how long after Start the first check waits. A
// proxy's first seconds belong to binding its listener and loading accounts,
// not to an errand nobody is waiting on.
const defaultInitialDelay = 30 * time.Second

// defaultInterval is the fallback when a config supplies a non-positive
// interval, which would otherwise spin the loop.
const defaultInterval = 24 * time.Hour

// State is the cached answer to "is there a newer release", shaped so
// view.Local can map it straight onto view.UpdateStatus.
type State struct {
	// Current is the running binary's version, so a UI can render "0.1.0 →
	// 0.2.0" without a second source for the left-hand side.
	Current string
	// Latest is the most recent release seen, or "" if none has been.
	Latest string
	// PageURL links to that release's page.
	PageURL string
	// Available is true only when Latest sorts above Current AND the check is
	// enabled AND no update has been installed since.
	Available bool
	// CheckedAt is unix ms of the last completed check attempt, 0 if none.
	CheckedAt int64
	// Err is the last check's failure, or "" — it does NOT clear Latest or
	// Available, so a transient network failure cannot make an available
	// update disappear from the UI.
	Err string
	// Disabled reflects update.checkEnabled.
	Disabled bool
	// DevBuild is true when the running binary was never stamped with a
	// version, which is reported rather than shown as a misleading "up to
	// date".
	DevBuild bool
}

// Checker keeps State fresh on a long cadence so nothing on the render path
// or the request path ever waits on github.com (design property 4). Its
// lifecycle is deliberately identical to metrics.Roller, metrics.Pruner, and
// prober.Prober: constructed in cmd/aiproxy's buildHandler, Start()/Stop()
// from run().
type Checker struct {
	client       *Client
	interval     time.Duration
	initialDelay time.Duration
	now          func() time.Time
	log          *slog.Logger

	stop      chan struct{}
	stopped   chan struct{}
	kick      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	mu      sync.Mutex
	enabled bool
	state   State
}

// CheckerOption configures a Checker at construction.
type CheckerOption func(*Checker)

// WithCheckerClock overrides the clock CheckedAt is stamped from, so tests do
// not race the wall clock.
func WithCheckerClock(now func() time.Time) CheckerOption {
	return func(ck *Checker) { ck.now = now }
}

// WithCheckerLogger overrides the logger. Nil (the default) uses slog.Default.
func WithCheckerLogger(log *slog.Logger) CheckerOption {
	return func(ck *Checker) { ck.log = log }
}

// WithInitialDelay overrides the wait before the first check.
func WithInitialDelay(d time.Duration) CheckerOption {
	return func(ck *Checker) { ck.initialDelay = d }
}

// NewChecker builds a Checker over c. enabled is update.checkEnabled and can
// be changed later through SetEnabled; interval is update.checkIntervalHours
// and cannot, which is why it is reported as restart-gated in view.Applied.
func NewChecker(c *Client, enabled bool, interval time.Duration, opts ...CheckerOption) *Checker {
	if interval <= 0 {
		interval = defaultInterval
	}
	ck := &Checker{
		client:       c,
		interval:     interval,
		initialDelay: defaultInitialDelay,
		now:          time.Now,
		log:          slog.Default(),
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
		kick:         make(chan struct{}, 1),
		enabled:      enabled,
		state:        State{Current: c.Current(), Disabled: !enabled},
	}
	for _, o := range opts {
		o(ck)
	}
	return ck
}

// Start begins the background loop. Like prober.Prober.Start it always spawns
// its goroutine, even when checking is disabled, so Stop is always safe
// afterward and SetEnabled(true) has a loop to wake. Guarded by a sync.Once:
// a duplicate call is a no-op rather than a second loop racing the first.
//
// Stop must follow Start — the same contract prober.Prober documents.
func (ck *Checker) Start() {
	ck.startOnce.Do(func() { go ck.loop() })
}

// Stop halts the loop and waits for it to finish.
func (ck *Checker) Stop() {
	ck.stopOnce.Do(func() {
		close(ck.stop)
		<-ck.stopped
	})
}

// State returns the cached answer. It never performs I/O: this is what
// ServerStatus calls, on the TUI's two-second cadence.
func (ck *Checker) State() State {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	return ck.state
}

// SetEnabled turns checking on or off live, which is what makes
// update.checkEnabled a live-tunable setting rather than a restart-gated one.
// Turning it on kicks an immediate check: an operator who just enabled it
// should not wait up to a day for the first answer. Turning it off clears
// Available, so the UI stops offering something the operator just declined to
// look for.
func (ck *Checker) SetEnabled(on bool) {
	ck.mu.Lock()
	changed := ck.enabled != on
	ck.enabled = on
	ck.state.Disabled = !on
	if !on {
		ck.state.Available = false
	}
	ck.mu.Unlock()

	if changed && on {
		select {
		case ck.kick <- struct{}{}:
		default: // a kick is already pending; one is enough
		}
	}
}

// Apply runs one check-and-install cycle and folds the outcome back into the
// cache, so a successful install stops the UI offering the same update. The
// remaining action after this returns is a restart, and that is the caller's
// message to deliver.
//
// Serializing concurrent callers is the seam's job, not this method's; see
// view.Local.ApplyUpdate and ErrUpdateInProgress.
func (ck *Checker) Apply(ctx context.Context) (Result, error) {
	rel, err := ck.client.Check(ctx)
	if err != nil {
		return Result{}, err
	}
	res, err := ck.client.Apply(ctx, rel)
	if err != nil {
		return res, err
	}

	ck.mu.Lock()
	ck.state.Latest = res.Version
	ck.state.Available = false
	ck.state.Err = ""
	ck.mu.Unlock()

	ck.log.Info("updated in place", "from", res.PreviousVersion, "to", res.Version, "path", res.Path)
	return res, nil
}

func (ck *Checker) loop() {
	defer close(ck.stopped)

	// One context for the whole loop, cancelled by Stop, so a check in flight
	// at shutdown is abandoned rather than holding the process open.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-ck.stop
		cancel()
	}()

	timer := time.NewTimer(ck.initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ck.stop:
			return
		case <-ck.kick:
			if !timer.Stop() {
				// Drain a timer that already fired, so the Reset below is not
				// immediately satisfied by a stale value.
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		timer.Reset(ck.interval)

		ck.mu.Lock()
		enabled := ck.enabled
		ck.mu.Unlock()
		if !enabled {
			// The whole of property 5: a disabled checker reaches no further
			// than this line.
			continue
		}
		ck.checkOnce(ctx)
	}
}

// checkOnce performs one check and folds the result into the cache. A failure
// records the error WITHOUT clearing a previous good answer: a flaky link
// must not make an available update disappear.
func (ck *Checker) checkOnce(ctx context.Context) {
	rel, err := ck.client.Check(ctx)

	ck.mu.Lock()
	defer ck.mu.Unlock()
	ck.state.CheckedAt = ck.now().UnixMilli()

	switch {
	case errors.Is(err, ErrDevBuild):
		// Not a failure, and not something to retry differently: reported as
		// its own state so the UI can say "dev build" rather than "up to date".
		ck.state.DevBuild = true
		ck.state.Available = false
		ck.state.Err = ""
	case errors.Is(err, ErrNoReleases):
		ck.state.Available = false
		ck.state.Err = ""
	case err != nil:
		ck.state.Err = err.Error()
		ck.log.Debug("update check failed", "err", err)
	default:
		ck.state.Err = ""
		ck.state.Latest = rel.Version
		ck.state.PageURL = rel.PageURL
		ck.state.Available = Compare(rel.Version, ck.state.Current) > 0
	}
}
