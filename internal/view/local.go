package view

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/prober"
	"github.com/nicko170/aiproxy/internal/provider"
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
	probe   *prober.Prober

	listenAddr string
	started    time.Time
	dropped    func() int64
	now        func() time.Time

	hub *hub

	// mu serializes every mutation (SetAccountEnabled, SetPriority,
	// RemoveAccount, UpdateSettings) end to end, across both the
	// config.Store persist and the account.Manager apply. config.Store.Update
	// already serializes the persist step alone, and Manager serializes its
	// own apply step alone, but nothing previously serialized the PAIR: two
	// concurrent mutations could interleave persist-A, persist-B, apply-B,
	// apply-A, leaving the config file holding B while the live Manager holds
	// A — disagreeing until restart, which is exactly the failure the
	// persist-then-apply ordering exists to avoid. Holding mu across the
	// whole body of each mutation makes the pair atomic with respect to every
	// other mutation.
	mu sync.Mutex
}

// option configures a Local at construction. It exists so tests can inject a
// fake clock without adding a parameter every caller must pass; see withClock.
type option func(*Local)

// withClock overrides both the clock ServerStatus reads from and the instant
// NewLocal stamps as "started", so uptime and window-relative queries (p95
// TTFB) are deterministic in tests instead of racing real wall-clock time.
func withClock(now func() time.Time) option {
	return func(l *Local) {
		l.now = now
		l.started = now()
	}
}

// NewLocal builds a Local over the given services. dropped may be nil, which
// Local treats as always reporting zero (matching the pre-stage-3 status
// handler's convention). pb is the background quota prober ProbeNow
// delegates to and ServerStatus reports health from; cmd/aiproxy always
// constructs one (even with quotaProbe.intervalSeconds 0, which disables
// only its periodic loop — see prober.New's doc comment), so pb is never nil
// in production.
func NewLocal(mgr *account.Manager, ms *metrics.Store, cs *config.Store, listenAddr string, dropped func() int64, pb *prober.Prober, opts ...option) *Local {
	if dropped == nil {
		dropped = func() int64 { return 0 }
	}
	now := time.Now
	l := &Local{
		mgr:        mgr,
		metrics:    ms,
		config:     cs,
		probe:      pb,
		listenAddr: listenAddr,
		started:    now(),
		dropped:    dropped,
		now:        now,
		hub:        newHub(),
	}
	for _, o := range opts {
		o(l)
	}
	return l
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
		EventsDropped:  l.hub.droppedCount(),
		Probe:          l.probeStatus(),
	}, nil
}

// probeStatus converts the prober's own status type into the view-level
// shape ServerStatus reports (spec §6.2: "reports probe health in the UI, so
// a throttled probe is visible rather than silently wrong"). A nil prober
// (only reachable if a caller builds a Local without one) reports the zero
// value rather than panicking.
func (l *Local) probeStatus() ProbeStatus {
	if l.probe == nil {
		return ProbeStatus{Accounts: map[string]AccountProbeStatus{}}
	}
	st := l.probe.Status()
	out := ProbeStatus{
		Running:         st.Running,
		LastStartedAt:   st.LastStartedAt,
		LastCompletedAt: st.LastCompletedAt,
		Accounts:        make(map[string]AccountProbeStatus, len(st.Accounts)),
	}
	for id, a := range st.Accounts {
		out.Accounts[id] = AccountProbeStatus{
			LastError: a.LastError, LastSuccessAt: a.LastSuccessAt, NextAttemptAt: a.NextAttemptAt,
		}
	}
	return out
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
//
// mu is held across both steps (see Local.mu's doc comment): without it, two
// concurrent calls could interleave persist-A, persist-B, apply-B, apply-A,
// leaving the config file and the live Manager disagreeing until restart.
func (l *Local) SetAccountEnabled(ctx context.Context, accountID string, enabled bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.mgr.Get(accountID); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownAccount, accountID)
	}
	if _, err := l.config.Update(func(c *config.Config) error {
		return setAccountField(c, accountID, func(a *config.Account) { a.Disabled = !enabled })
	}); err != nil {
		return err
	}
	return l.mgr.SetEnabled(accountID, enabled)
}

func (l *Local) SetPriority(ctx context.Context, accountID string, priority int) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.mgr.Get(accountID); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownAccount, accountID)
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
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.mgr.Get(accountID); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownAccount, accountID)
	}
	if _, err := l.config.Update(func(c *config.Config) error {
		for i := range c.Accounts {
			if c.Accounts[i].ID == accountID {
				c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("%w: %q", ErrUnknownAccount, accountID)
	}); err != nil {
		return err
	}
	return l.mgr.Remove(accountID)
}

// Settings reads the live-tunable subset of config back from the config
// store, so a caller can read-modify-write through the seam (see Source's
// doc comment on this method for why that matters).
func (l *Local) Settings(ctx context.Context) (Settings, error) {
	c, err := l.config.Load()
	if err != nil {
		return Settings{}, err
	}
	return settingsFromConfig(c), nil
}

func settingsFromConfig(c config.Config) Settings {
	return Settings{
		SwitchThreshold:           c.Routing.SwitchThreshold,
		RetryBudgetMS:             c.Retry.BudgetMS,
		InlineAbsorbMaxMS:         c.Retry.InlineAbsorbMaxMS,
		HeaderTimeoutMS:           c.Retry.HeaderTimeoutMS,
		BodyIdleMS:                c.Retry.BodyIdleMS,
		SessionAffinity:           c.Routing.SessionAffinity,
		BlockedModels:             c.Routing.BlockedModels,
		QuotaProbeIntervalSeconds: c.QuotaProbe.IntervalSeconds,
		MetricsRetentionDays:      c.Metrics.RetentionDays,
		UpdateCheckEnabled:        c.Update.CheckEnabled,
		UpdateCheckIntervalHours:  c.Update.CheckIntervalHours,
	}
}

// liveSettingsFields and restartSettingsFields name, in Applied's vocabulary,
// which Settings fields Manager reads live versus which are baked into
// structs built once at startup with no reload path. Keeping the field names
// here (rather than inline in UpdateSettings) is what makes "a field became
// live" a one-line change: move its name from one slice to the other and
// wire the corresponding Manager setter; UpdateSettings's diff logic does not
// change.
var (
	liveSettingsFields    = []string{"switchThreshold", "sessionAffinity"}
	restartSettingsFields = []string{
		"blockedModels", "retryBudgetMs", "inlineAbsorbMaxMs",
		"headerTimeoutMs", "bodyIdleMs", "quotaProbeIntervalSeconds", "metricsRetentionDays",
		"updateCheckEnabled", "updateCheckIntervalHours",
	}
)

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
//
// The Applied return exists because that gap must be reported as data, not
// merely as this comment: a stage-4 settings screen decodes a JSON response,
// and nothing about a 200 with no body tells it that six of nine fields it
// just sent are sitting unapplied in the config file. Applied.NeedsRestart
// contains a field name only when the caller actually changed it (comparing
// against the config as it was before this call) AND that field is one of
// the restart-gated ones — an unchanged restart-gated field is silent, same
// as an unchanged live one, so a screen that only re-renders on a nonempty
// list does the right thing by default.
func (l *Local) UpdateSettings(ctx context.Context, s Settings) (Applied, error) {
	if err := s.Validate(); err != nil {
		return Applied{}, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var applied Applied
	if _, err := l.config.Update(func(c *config.Config) error {
		before := settingsFromConfig(*c)
		applied = diffSettings(before, s)

		c.Routing.SwitchThreshold = s.SwitchThreshold
		c.Routing.SessionAffinity = s.SessionAffinity
		c.Routing.BlockedModels = s.BlockedModels
		c.Retry.BudgetMS = s.RetryBudgetMS
		c.Retry.InlineAbsorbMaxMS = s.InlineAbsorbMaxMS
		c.Retry.HeaderTimeoutMS = s.HeaderTimeoutMS
		c.Retry.BodyIdleMS = s.BodyIdleMS
		c.QuotaProbe.IntervalSeconds = s.QuotaProbeIntervalSeconds
		c.Metrics.RetentionDays = s.MetricsRetentionDays
		c.Update.CheckEnabled = s.UpdateCheckEnabled
		c.Update.CheckIntervalHours = s.UpdateCheckIntervalHours
		return nil
	}); err != nil {
		return Applied{}, err
	}

	l.mgr.SetSwitchThreshold(s.SwitchThreshold)
	l.mgr.SetSessionAffinity(s.SessionAffinity)
	return applied, nil
}

// diffSettings reports which changed fields between before and after are
// live versus restart-gated, using liveSettingsFields and
// restartSettingsFields as the classification. BlockedModels is compared
// element-wise and in order: a caller resending the same list unchanged
// (even a nil vs an empty slice, both length zero) must not be reported as a
// change.
func diffSettings(before, after Settings) Applied {
	var applied Applied
	changed := map[string]bool{
		"switchThreshold":           before.SwitchThreshold != after.SwitchThreshold,
		"sessionAffinity":           before.SessionAffinity != after.SessionAffinity,
		"blockedModels":             !stringSlicesEqual(before.BlockedModels, after.BlockedModels),
		"retryBudgetMs":             before.RetryBudgetMS != after.RetryBudgetMS,
		"inlineAbsorbMaxMs":         before.InlineAbsorbMaxMS != after.InlineAbsorbMaxMS,
		"headerTimeoutMs":           before.HeaderTimeoutMS != after.HeaderTimeoutMS,
		"bodyIdleMs":                before.BodyIdleMS != after.BodyIdleMS,
		"quotaProbeIntervalSeconds": before.QuotaProbeIntervalSeconds != after.QuotaProbeIntervalSeconds,
		"metricsRetentionDays":      before.MetricsRetentionDays != after.MetricsRetentionDays,
		"updateCheckEnabled":        before.UpdateCheckEnabled != after.UpdateCheckEnabled,
		"updateCheckIntervalHours":  before.UpdateCheckIntervalHours != after.UpdateCheckIntervalHours,
	}
	for _, name := range liveSettingsFields {
		if changed[name] {
			applied.Live = append(applied.Live, name)
		}
	}
	for _, name := range restartSettingsFields {
		if changed[name] {
			applied.NeedsRestart = append(applied.NeedsRestart, name)
		}
	}
	return applied
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func setAccountField(c *config.Config, accountID string, fn func(*config.Account)) error {
	for i := range c.Accounts {
		if c.Accounts[i].ID == accountID {
			fn(&c.Accounts[i])
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrUnknownAccount, accountID)
}

// Login looks up the named provider and returns its login session
// unaltered. That is deliberately all it does: everything that makes Login
// safe and useful — never opening a browser itself, verifying state,
// accepting a pasted code, timing out, cleaning up its listener, and
// persisting the exchanged credential through config.Store and
// account.Manager.Add before LoginResult ever reaches this call's caller —
// already happened inside the provider's own Login (see
// anthropic.Anthropic.OnLoginSuccess, wired by cmd/aiproxy). Local has
// nothing to add to that and does not hold l.mu for it: the persistence
// happens on whatever goroutine the callback or a submitted code arrives on,
// asynchronously and long after this call returns, so serializing it against
// SetPriority/RemoveAccount/etc. here would not help and holding a lock
// across an operation that can take up to two minutes would be actively
// harmful.
func (l *Local) Login(ctx context.Context, providerName string) (provider.LoginSession, error) {
	p, ok := l.mgr.Provider(providerName)
	if !ok {
		return provider.LoginSession{}, fmt.Errorf("unknown provider %q", providerName)
	}
	return p.Login(ctx)
}

// ImportCredentials reads accounts from an external credential file (spec
// §6.3), persists any not already present, and adds them to the live
// Manager without a restart — the same persist-then-apply order, and the
// same mu, as every other Source mutation (see Local.mu's doc comment).
//
// Deduping happens inside the same config.Store.Update that appends, not
// before it: re-reading and deciding under one atomic read-modify-write is
// what makes two concurrent ImportCredentials calls (or an import racing a
// Login for the same account) unable to both decide "not present yet" and
// both append it.
func (l *Local) ImportCredentials(ctx context.Context, source config.ImportSource) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var path string
	switch source {
	case config.ImportSourceLegacy:
		path = config.LegacyPath()
	case config.ImportSourceClaudeCode:
		path = config.ClaudeCodePath()
	default:
		return 0, fmt.Errorf("unknown import source %q", source)
	}
	if path == "" {
		return 0, fmt.Errorf("no path resolved for import source %q", source)
	}

	imported, err := config.ImportFile(path, source)
	if err != nil {
		return 0, err
	}
	if len(imported) == 0 {
		return 0, nil
	}

	var added []config.Account
	if _, err := l.config.Update(func(c *config.Config) error {
		seen := map[string]bool{}
		for _, a := range c.Accounts {
			if key := importDedupeKey(a); key != "" {
				seen[key] = true
			}
		}
		for _, a := range imported {
			key := importDedupeKey(a)
			if key != "" && seen[key] {
				continue
			}
			if key != "" {
				seen[key] = true
			}
			c.Accounts = append(c.Accounts, a)
			added = append(added, a)
		}
		return nil
	}); err != nil {
		return 0, err
	}

	for _, a := range added {
		if err := l.mgr.Add(a); err != nil {
			// Persisted but not live: surfaced rather than silently dropped,
			// though this should not be reachable in practice since Add's
			// only failure is a duplicate id and every imported account gets
			// a fresh one (config.NewID / ImportFile).
			return len(added), fmt.Errorf("account persisted but not added live: %w", err)
		}
	}
	return len(added), nil
}

// importDedupeKey identifies an account for ImportCredentials' dedupe
// purposes: the credential's account uuid when known, else its label —
// exactly the two fields spec calls out ("dedupe on the credential's
// account uuid, or on the label when no uuid is present"). Empty for an
// account with neither, which is treated as never a duplicate: there is
// nothing to key on.
func importDedupeKey(a config.Account) string {
	if a.Identity.AccountUUID != "" {
		return "uuid:" + a.Identity.AccountUUID
	}
	if a.Label != "" {
		return "label:" + a.Label
	}
	return ""
}

// ProbeNow triggers one out-of-band quota-probe cycle (see internal/prober).
// It does not hold l.mu: a probe cycle only reads accounts and calls
// mgr.UpdateQuota, never config.Store, so there is nothing here that could
// interleave with another Source mutation's persist-then-apply pair.
func (l *Local) ProbeNow(ctx context.Context) error {
	if l.probe == nil {
		return fmt.Errorf("quota prober not configured")
	}
	return l.probe.ProbeNow(ctx)
}
