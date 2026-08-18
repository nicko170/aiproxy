package view

import (
	"context"
	"fmt"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
)

// statusLatencyWindow bounds how far back ServerStatus looks for its p95
// TTFB figure. It is a live "is the proxy healthy right now" readout, not a
// historical query, so it is deliberately short and fixed rather than
// configurable.
const statusLatencyWindow = time.Hour

// Local is the v1 implementation of Source: it calls account.Manager,
// metrics.Store, and config.Store directly and fans events out over an
// in-process hub. A future view.HTTP will call the control API instead and
// read its SSE stream; constructing that in cmd/aiproxy is the only change a
// detached-daemon mode requires (spec §3.1).
type Local struct {
	mgr     *account.Manager
	metrics *metrics.Store
	config  *config.Store

	listenAddr string
	started    time.Time
	dropped    func() int64
	now        func() time.Time

	hub *hub
}

// NewLocal builds a Local over the given services. dropped may be nil, which
// Local treats as always reporting zero (matching the pre-stage-3 status
// handler's convention).
func NewLocal(mgr *account.Manager, ms *metrics.Store, cs *config.Store, listenAddr string, dropped func() int64) *Local {
	if dropped == nil {
		dropped = func() int64 { return 0 }
	}
	return &Local{
		mgr:        mgr,
		metrics:    ms,
		config:     cs,
		listenAddr: listenAddr,
		started:    time.Now(),
		dropped:    dropped,
		now:        time.Now,
		hub:        newHub(),
	}
}

// Publish feeds one completed request into the event stream. cmd/aiproxy
// calls this from the same OnResult hook that feeds metrics ingestion, so
// Subscribe-rs and the accounting store observe the same requests.
func (l *Local) Publish(ev Event) { l.hub.publish(ev) }

func (l *Local) ServerStatus(ctx context.Context) (Status, error) {
	var inFlight int
	for _, a := range l.mgr.All() {
		inFlight += a.InFlight
	}

	now := l.now()
	lat, err := l.metrics.LatencyPercentiles(ctx, metrics.Window{
		From: now.Add(-statusLatencyWindow).UnixMilli(),
		To:   now.UnixMilli(),
	})
	if err != nil {
		return Status{}, fmt.Errorf("server status: %w", err)
	}

	return Status{
		ListenAddr:     l.listenAddr,
		UptimeSeconds:  int64(now.Sub(l.started).Seconds()),
		InFlight:       inFlight,
		TTFBP95MS:      lat.TTFBP95,
		MetricsDropped: l.dropped(),
	}, nil
}

func (l *Local) Accounts(ctx context.Context) ([]Account, error) {
	src := l.mgr.All()
	out := make([]Account, 0, len(src))
	for _, a := range src {
		buckets := make(map[string]float64, len(a.Buckets))
		for name, b := range a.Buckets {
			buckets[name] = b.Utilization
		}
		out = append(out, Account{
			ID:               a.ID,
			Label:            a.Label,
			Provider:         a.Provider,
			Priority:         a.Priority,
			Disabled:         a.Disabled,
			Status:           a.Status.String(),
			LastError:        a.LastError,
			InFlight:         a.InFlight,
			RateLimitedUntil: a.RateLimitedUntil,
			PausedUntil:      a.PausedUntil,
			Buckets:          buckets,
		})
	}
	return out, nil
}

func (l *Local) UsageSeries(ctx context.Context, q SeriesQuery) (Series, error) {
	res, err := l.metrics.UsageSeries(ctx, metrics.SeriesQuery{
		Window:      metrics.Window{From: q.Window.From, To: q.Window.To},
		Granularity: metrics.Granularity(q.Granularity),
		GroupBy:     metrics.GroupBy(q.GroupBy),
	})
	if err != nil {
		return Series{}, err
	}
	out := Series{Granularity: Granularity(res.Granularity), GroupBy: GroupBy(res.GroupBy)}
	for _, p := range res.Points {
		out.Points = append(out.Points, Point{
			BucketStart: p.BucketStart, Key: p.Key, Requests: p.Requests,
			InputTokens: p.InputTokens, OutputTokens: p.OutputTokens,
			CacheReadTokens: p.CacheReadTokens, CacheWriteTokens: p.CacheWriteTokens,
			CostMicros: p.CostMicros,
		})
	}
	return out, nil
}

func (l *Local) Totals(ctx context.Context, w Window) (Totals, error) {
	t, err := l.metrics.Totals(ctx, metrics.Window{From: w.From, To: w.To})
	if err != nil {
		return Totals{}, err
	}
	return Totals{
		Requests: t.Requests, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
		CacheReadTokens: t.CacheReadTokens, CacheWriteTokens: t.CacheWriteTokens,
		CostMicros: t.CostMicros, UnpricedRequests: t.UnpricedRequests,
	}, nil
}

func (l *Local) LatencyPercentiles(ctx context.Context, w Window) (Latency, error) {
	lat, err := l.metrics.LatencyPercentiles(ctx, metrics.Window{From: w.From, To: w.To})
	if err != nil {
		return Latency{}, err
	}
	return Latency{
		TTFBP50MS: lat.TTFBP50, TTFBP95MS: lat.TTFBP95,
		DurationP50MS: lat.DurationP50, DurationP95MS: lat.DurationP95,
	}, nil
}

func (l *Local) AccountQuotaHistory(ctx context.Context, accountID string, w Window) ([]QuotaPoint, error) {
	pts, err := l.metrics.AccountQuotaHistory(ctx, accountID, metrics.Window{From: w.From, To: w.To})
	if err != nil {
		return nil, err
	}
	out := make([]QuotaPoint, 0, len(pts))
	for _, p := range pts {
		out = append(out, QuotaPoint{At: p.At, Bucket: p.Bucket, Utilization: p.Utilization, ResetsAt: p.ResetsAt})
	}
	return out, nil
}

func (l *Local) Subscribe(ctx context.Context) (<-chan Event, error) {
	return l.hub.subscribe(ctx), nil
}

// SetAccountEnabled persists first, through the config store, and only then
// applies the change to the live Manager: a failed write must leave nothing
// changed, rather than a runtime state the next restart silently reverts.
func (l *Local) SetAccountEnabled(ctx context.Context, accountID string, enabled bool) error {
	if _, ok := l.mgr.Get(accountID); !ok {
		return fmt.Errorf("unknown account %q", accountID)
	}
	if _, err := l.config.Update(func(c *config.Config) error {
		return setAccountField(c, accountID, func(a *config.Account) { a.Disabled = !enabled })
	}); err != nil {
		return err
	}
	return l.mgr.SetEnabled(accountID, enabled)
}

func (l *Local) SetPriority(ctx context.Context, accountID string, priority int) error {
	if _, ok := l.mgr.Get(accountID); !ok {
		return fmt.Errorf("unknown account %q", accountID)
	}
	if _, err := l.config.Update(func(c *config.Config) error {
		return setAccountField(c, accountID, func(a *config.Account) { a.Priority = priority })
	}); err != nil {
		return err
	}
	return l.mgr.SetPriority(accountID, priority)
}

// RemoveAccount drops the account from the config store, the live Manager,
// and — inside Manager.Remove — any session affinity pinned to it.
func (l *Local) RemoveAccount(ctx context.Context, accountID string) error {
	if _, ok := l.mgr.Get(accountID); !ok {
		return fmt.Errorf("unknown account %q", accountID)
	}
	if _, err := l.config.Update(func(c *config.Config) error {
		for i := range c.Accounts {
			if c.Accounts[i].ID == accountID {
				c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("unknown account %q", accountID)
	}); err != nil {
		return err
	}
	return l.mgr.Remove(accountID)
}

// UpdateSettings validates before writing anything, then persists the
// live-tunable subset of config (spec §6.2), then applies to the running
// Manager whichever fields it reads live.
//
// SwitchThreshold and SessionAffinity take effect immediately: Manager reads
// both under the same mutex account.Manager.SetSwitchThreshold and
// SetSessionAffinity lock (see account/mutate.go), so there is no separate
// propagation step. Everything else here — the retry clocks, blocked models,
// the quota probe interval, and metrics retention — is baked into structs
// built once in cmd/aiproxy at startup (HandlerOptions, RetryConfig, the
// roller and pruner's fixed intervals) with no live-reload path, so those
// require a restart to take effect. That gap is deliberate scope, not an
// oversight: building a reload path for the attempt loop's transport and
// timers is a larger change than this stage, and this comment plus the
// implementation report are where that limitation is recorded rather than
// silently discovered later.
func (l *Local) UpdateSettings(ctx context.Context, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if _, err := l.config.Update(func(c *config.Config) error {
		c.Routing.SwitchThreshold = s.SwitchThreshold
		c.Routing.SessionAffinity = s.SessionAffinity
		c.Routing.BlockedModels = s.BlockedModels
		c.Retry.BudgetMS = s.RetryBudgetMS
		c.Retry.InlineAbsorbMaxMS = s.InlineAbsorbMaxMS
		c.Retry.HeaderTimeoutMS = s.HeaderTimeoutMS
		c.Retry.BodyIdleMS = s.BodyIdleMS
		c.QuotaProbe.IntervalSeconds = s.QuotaProbeIntervalSeconds
		c.Metrics.RetentionDays = s.MetricsRetentionDays
		return nil
	}); err != nil {
		return err
	}

	l.mgr.SetSwitchThreshold(s.SwitchThreshold)
	l.mgr.SetSessionAffinity(s.SessionAffinity)
	return nil
}

func setAccountField(c *config.Config, accountID string, fn func(*config.Account)) error {
	for i := range c.Accounts {
		if c.Accounts[i].ID == accountID {
			fn(&c.Accounts[i])
			return nil
		}
	}
	return fmt.Errorf("unknown account %q", accountID)
}
