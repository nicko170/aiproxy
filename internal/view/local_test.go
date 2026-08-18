package view

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/prober"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// testHarness wires a Local over real (in-memory / temp-file) services, the
// same components cmd/aiproxy composes in production, so what these tests
// exercise is exactly what view.Local does in the running proxy.
type testHarness struct {
	t       *testing.T
	local   *Local
	mgr     *account.Manager
	ms      *metrics.Store
	cs      *config.Store
	ing     *metrics.Ingester
	probe   *prober.Prober
	dropped func() int64
}

func newHarness(t *testing.T, accts ...config.Account) *testHarness {
	t.Helper()
	return newHarnessWithClock(t, nil, accts...)
}

// newHarnessWithClock is newHarness with an injectable clock, for tests that
// need to control what ServerStatus sees as "now" (uptime, the p95 TTFB
// window). now may be nil, in which case Local uses the real wall clock.
func newHarnessWithClock(t *testing.T, now func() time.Time, accts ...config.Account) *testHarness {
	t.Helper()
	return newHarnessWithProviders(t, now, map[string]provider.Provider{"stub": stubProvider{}}, accts...)
}

// newHarnessWithProviders is the general form every other harness
// constructor delegates to: it lets a test (e.g. Login's) register an extra
// controllable provider.Provider alongside "stub" without every other test
// in this file having to know about it.
func newHarnessWithProviders(t *testing.T, now func() time.Time, providers map[string]provider.Provider, accts ...config.Account) *testHarness {
	t.Helper()
	ms, err := metrics.OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	cs := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if _, err := cs.Update(func(c *config.Config) error {
		c.Accounts = accts
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	mgr := account.New(accts, providers, account.Options{
		SwitchThreshold: 0.98,
		SessionAffinity: true,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	ing := metrics.NewIngester(ms, metrics.IngestOptions{})
	t.Cleanup(func() { ing.Close() })

	dropped := func() int64 { return ing.Dropped() }

	pb := prober.New(mgr, providers, time.Hour)

	var opts []option
	if now != nil {
		opts = append(opts, withClock(now))
	}
	local := NewLocal(mgr, ms, cs, "127.0.0.1:3456", dropped, pb, opts...)
	return &testHarness{t: t, local: local, mgr: mgr, ms: ms, cs: cs, ing: ing, probe: pb, dropped: dropped}
}

// noWaiter satisfies account.Waiter without ever actually waiting; it is only
// safe to use when the account's ramp is disabled and unpaused, so Admit
// never has a reason to call it.
type noWaiter struct{}

func (noWaiter) Wait(context.Context, time.Duration) error { return nil }

// stubProvider is the minimal provider.Provider a test account needs; view
// never calls into it directly, but account.New requires one per provider
// name referenced by an account.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	return provider.Credential{}, nil
}
func (stubProvider) Profile(context.Context, provider.Credential) (provider.Profile, error) {
	return provider.Profile{}, provider.ErrUnsupported
}
func (stubProvider) Quota(context.Context, provider.Credential) (provider.Quota, error) {
	return provider.Quota{}, provider.ErrUnsupported
}
func (stubProvider) Login(context.Context) (provider.LoginSession, error) {
	return provider.LoginSession{}, provider.ErrUnsupported
}
func (stubProvider) Endpoint(provider.Account) *url.URL {
	u, _ := url.Parse("https://upstream.invalid")
	return u
}
func (stubProvider) Authorize(*http.Request, provider.Credential)             {}
func (stubProvider) RewriteBody(b []byte, _ provider.Account) ([]byte, error) { return b, nil }
func (stubProvider) ClassifyResponse(*http.Response) provider.Outcome         { return provider.Outcome{} }
func (stubProvider) ParseUsage([]byte) (*provider.UsageDelta, bool)           { return nil, false }
func (stubProvider) ParseUsageBody([]byte) (*provider.UsageDelta, bool)       { return nil, false }

// loginableProvider is a stubProvider that answers Login with a canned,
// test-controlled session, so Local.Login's own job — looking up the named
// provider and handing its session straight back — can be exercised without
// a real PKCE/HTTP flow (already covered at the anthropic package level).
type loginableProvider struct {
	stubProvider
	session provider.LoginSession
	err     error
}

func (loginableProvider) Name() string { return "loginable" }
func (l loginableProvider) Login(context.Context) (provider.LoginSession, error) {
	return l.session, l.err
}

func acctCfg(id string, priority int) config.Account {
	return config.Account{
		ID: id, Provider: "stub", Label: id, Priority: priority,
		Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk-super-secret-" + id},
	}
}

// UptimeSeconds must reflect actual elapsed clock time, not merely be
// non-negative (a check that variable-length windows, an off-by-a-day sign
// error, or a stopped clock could never trip). An injected clock makes the
// elapsed time exact and repeatable.
func TestServerStatusReportsListenAddrDroppedCountAndExactUptime(t *testing.T) {
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	cur := base
	h := newHarnessWithClock(t, func() time.Time { return cur }, acctCfg("a", 0))

	cur = base.Add(90 * time.Second)
	st, err := h.local.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if st.ListenAddr != "127.0.0.1:3456" {
		t.Errorf("ListenAddr = %q", st.ListenAddr)
	}
	if st.UptimeSeconds != 90 {
		t.Errorf("UptimeSeconds = %d, want exactly 90 (base clock advanced by 90s)", st.UptimeSeconds)
	}
	if st.MetricsDropped != 0 {
		t.Errorf("MetricsDropped = %d, want 0", st.MetricsDropped)
	}
}

// The empty case: no requests recorded yet must report a zero latency, not an
// error. A query test suite that only ever seeds rows cannot catch a query
// that panics or errors on an empty table.
func TestServerStatusTTFBIsZeroWithNoRequests(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	st, err := h.local.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if st.TTFBP95MS != 0 {
		t.Errorf("TTFBP95MS = %d, want 0", st.TTFBP95MS)
	}
}

// A sample inside the trailing statusLatencyWindow must be reflected in the
// p95 TTFB figure — the counterpart to the "no requests" zero case above,
// which a suite that only ever tests the empty window cannot catch.
func TestServerStatusTTFBReflectsASampleInsideTheWindow(t *testing.T) {
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	h := newHarnessWithClock(t, func() time.Time { return base }, acctCfg("a", 0))

	h.ing.Record(metrics.Sample{
		StartedAt: base.Add(-time.Minute).UnixMilli(), AccountID: "a", Provider: "stub",
		Model: "claude-sonnet-5", Status: 200, Outcome: "ok", TTFBMS: 77, WaitMS: 0,
	})
	if err := h.ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	st, err := h.local.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if st.TTFBP95MS != 77 {
		t.Errorf("TTFBP95MS = %d, want 77 (the one sample inside the window)", st.TTFBP95MS)
	}
}

// A sample older than statusLatencyWindow must NOT be reflected: ServerStatus
// is a live "right now" readout, not a historical query. Before this test
// existed, inverting the window (or shrinking it to a nanosecond) left every
// other test green.
func TestServerStatusTTFBExcludesASampleOutsideTheWindow(t *testing.T) {
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	h := newHarnessWithClock(t, func() time.Time { return base }, acctCfg("a", 0))

	h.ing.Record(metrics.Sample{
		StartedAt: base.Add(-2 * time.Hour).UnixMilli(), AccountID: "a", Provider: "stub",
		Model: "claude-sonnet-5", Status: 200, Outcome: "ok", TTFBMS: 99, WaitMS: 0,
	})
	if err := h.ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	st, err := h.local.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if st.TTFBP95MS != 0 {
		t.Errorf("TTFBP95MS = %d, want 0 (the only sample is 2h old, outside statusLatencyWindow)", st.TTFBP95MS)
	}
}

// Status.InFlight must reflect account.Manager's admitted count, driven
// through the same Admit/Release path the proxy's request loop uses — not
// asserted nowhere, as it was before this test existed.
func TestServerStatusInFlightReflectsAdmittedAccounts(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	ctx := context.Background()

	if err := h.mgr.Admit(ctx, "a", noWaiter{}); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	st, err := h.local.ServerStatus(ctx)
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if st.InFlight != 1 {
		t.Errorf("InFlight = %d, want 1 after one Admit", st.InFlight)
	}

	h.mgr.Release("a")
	st2, err := h.local.ServerStatus(ctx)
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if st2.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0 after Release", st2.InFlight)
	}
}

func TestAccountsNeverIncludesCredentialMaterial(t *testing.T) {
	h := newHarness(t, config.Account{
		ID: "a", Provider: "stub", Label: "person@example.com", Priority: 0,
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "access-secret-xyz",
			RefreshToken: "refresh-secret-xyz", APIKey: "apikey-secret-xyz",
		},
	})

	accts, err := h.local.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(accts) != 1 || accts[0].ID != "a" || accts[0].Label != "person@example.com" {
		t.Fatalf("accounts = %+v", accts)
	}

	raw, err := json.Marshal(accts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	for _, secret := range []string{"access-secret-xyz", "refresh-secret-xyz", "apikey-secret-xyz"} {
		if strings.Contains(body, secret) {
			t.Errorf("accounts JSON leaked credential material %q: %s", secret, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "token") || strings.Contains(strings.ToLower(body), "authorization") {
		t.Errorf("accounts JSON should carry no credential-shaped field at all: %s", body)
	}
}

func TestAccountsOnEmptyManagerReturnsEmptyNotError(t *testing.T) {
	h := newHarness(t)

	accts, err := h.local.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(accts) != 0 {
		t.Errorf("accounts = %+v, want none", accts)
	}
}

func TestTotalsOnEmptyWindowReturnsZeroNotError(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	future := time.Now().Add(24 * time.Hour).UnixMilli()
	tot, err := h.local.Totals(context.Background(), Window{From: future, To: future + 1000})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Requests != 0 || tot.CostMicros != 0 {
		t.Errorf("Totals = %+v, want all zero", tot)
	}
}

func TestAccountQuotaHistoryUnknownAccountReturnsEmptyNotError(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	pts, err := h.local.AccountQuotaHistory(context.Background(), "does-not-exist",
		Window{From: 0, To: time.Now().Add(time.Hour).UnixMilli()})
	if err != nil {
		t.Fatalf("AccountQuotaHistory: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("points = %+v, want none", pts)
	}
}

func TestUsageSeriesRoundTripsThroughTheMetricsStore(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	now := base + 1000
	h.ing.Record(metrics.Sample{
		StartedAt: now, AccountID: "a", Provider: "stub", Model: "claude-sonnet-5",
		Status: 200, Outcome: "ok", InputTokens: 100, OutputTokens: 20, TTFBMS: 50, WaitMS: 0,
	})
	if err := h.ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := metrics.RollupOnce(context.Background(), h.ms, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}

	series, err := h.local.UsageSeries(context.Background(), SeriesQuery{
		Window:      Window{From: base, To: base + 60000},
		Granularity: GranularityMinute,
		GroupBy:     GroupByModel,
	})
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}
	var total int64
	for _, p := range series.Points {
		total += p.InputTokens
	}
	if total != 100 {
		t.Errorf("input tokens = %d, want 100", total)
	}

	tot, err := h.local.Totals(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Requests != 1 || tot.InputTokens != 100 {
		t.Errorf("Totals = %+v", tot)
	}

	lat, err := h.local.LatencyPercentiles(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatalf("LatencyPercentiles: %v", err)
	}
	if lat.TTFBP50MS != 50 {
		t.Errorf("TTFBP50MS = %d, want 50", lat.TTFBP50MS)
	}
}

func TestSetAccountEnabledPersistsThroughConfigStore(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	if err := h.local.SetAccountEnabled(context.Background(), "a", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	got, ok := h.mgr.Get("a")
	if !ok || !got.Disabled {
		t.Fatalf("manager account = %+v, ok=%v; want disabled", got, ok)
	}

	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 1 || !cfg.Accounts[0].Disabled {
		t.Errorf("persisted config = %+v, want disabled account", cfg.Accounts)
	}
}

func TestSetAccountEnabledUnknownAccountReturnsErrorAndWritesNothing(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	if err := h.local.SetAccountEnabled(context.Background(), "nope", false); err == nil {
		t.Error("want an error for an unknown account id")
	}
	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Accounts[0].Disabled {
		t.Error("config must not have been touched")
	}
}

func TestSetPriorityPersistsThroughConfigStore(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	if err := h.local.SetPriority(context.Background(), "a", 7); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}

	got, _ := h.mgr.Get("a")
	if got.Priority != 7 {
		t.Errorf("manager priority = %d, want 7", got.Priority)
	}
	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Accounts[0].Priority != 7 {
		t.Errorf("persisted priority = %d, want 7", cfg.Accounts[0].Priority)
	}
}

// Concurrent conflicting mutations must never leave the persisted config and
// the live Manager disagreeing: each SetPriority call persists through
// config.Store then applies to Manager as two separate steps, and without a
// lock spanning both, one goroutine's persist can land between another's
// persist and apply, leaving the file holding one value and the manager
// holding a different one until restart.
func TestConcurrentSetPriorityMutationsLeaveConfigAndManagerAgreeing(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(priority int) {
			defer wg.Done()
			if err := h.local.SetPriority(context.Background(), "a", priority); err != nil {
				t.Errorf("SetPriority(%d): %v", priority, err)
			}
		}(i)
	}
	wg.Wait()

	got, ok := h.mgr.Get("a")
	if !ok {
		t.Fatal("account a should still exist")
	}
	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("persisted accounts = %+v, want exactly one", cfg.Accounts)
	}
	if cfg.Accounts[0].Priority != got.Priority {
		t.Errorf("manager priority %d disagrees with persisted priority %d after %d concurrent mutations",
			got.Priority, cfg.Accounts[0].Priority, n)
	}
}

func TestRemoveAccountPersistsAndDropsFromManager(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0), acctCfg("b", 1))

	if err := h.local.RemoveAccount(context.Background(), "a"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}

	if _, ok := h.mgr.Get("a"); ok {
		t.Error("account should be gone from the manager")
	}
	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ID != "b" {
		t.Errorf("persisted accounts = %+v, want only b", cfg.Accounts)
	}
}

func TestRemoveAccountUnknownReturnsError(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	if err := h.local.RemoveAccount(context.Background(), "nope"); err == nil {
		t.Error("want an error for an unknown account id")
	}
}

func TestUpdateSettingsRejectsInvalidValuesWithoutPersisting(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	before, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	bad := Settings{
		SwitchThreshold: -1, RetryBudgetMS: 10000, InlineAbsorbMaxMS: 5000,
		HeaderTimeoutMS: 60000, BodyIdleMS: 120000, QuotaProbeIntervalSeconds: 300,
		MetricsRetentionDays: 90, UpdateCheckEnabled: true, UpdateCheckIntervalHours: 24,
	}
	if _, err := h.local.UpdateSettings(context.Background(), bad); err == nil {
		t.Error("want an error for a negative switch threshold")
	}

	after, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after.Routing.SwitchThreshold != before.Routing.SwitchThreshold {
		t.Error("an invalid settings update must not have written anything")
	}
}

func TestUpdateSettingsPersistsAndAppliesLiveTunableFieldsImmediately(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0), acctCfg("b", 1))
	h.mgr.RecordSession("sess-1", "b")

	good := Settings{
		SwitchThreshold: 0.5, RetryBudgetMS: 8000, InlineAbsorbMaxMS: 4000,
		HeaderTimeoutMS: 30000, BodyIdleMS: 60000, SessionAffinity: false,
		BlockedModels: []string{"*fable*"}, QuotaProbeIntervalSeconds: 120,
		MetricsRetentionDays: 30, UpdateCheckEnabled: true, UpdateCheckIntervalHours: 24,
	}
	if _, err := h.local.UpdateSettings(context.Background(), good); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Routing.SwitchThreshold != 0.5 || cfg.Routing.SessionAffinity != false {
		t.Errorf("persisted routing = %+v", cfg.Routing)
	}
	if cfg.Retry.BudgetMS != 8000 || cfg.QuotaProbe.IntervalSeconds != 120 || cfg.Metrics.RetentionDays != 30 {
		t.Errorf("persisted config = %+v", cfg)
	}

	// SessionAffinity is one of the two knobs Manager reads live: with it now
	// off, the session pinned to "b" must no longer win over "a"'s lower
	// priority.
	got, err := h.mgr.Select(account.SelectRequest{Model: "claude-sonnet", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "a" {
		t.Errorf("selected %q, want a; SessionAffinity=false should apply without a restart", got.ID)
	}
}

// UpdateSettings must report exactly which changed fields are live versus
// restart-gated: a changed live field appears under Live, a changed
// restart-gated field appears under NeedsRestart, and an UNCHANGED
// restart-gated field appears in neither — reporting it would tell a
// stage-4 settings screen a field is pending a restart when nothing about
// it actually changed.
func TestUpdateSettingsReportsAppliedLiveAndNeedsRestartFields(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))
	def := config.Default()

	s := Settings{
		SwitchThreshold:           0.5, // changed from the 0.98 default: live
		SessionAffinity:           def.Routing.SessionAffinity,
		RetryBudgetMS:             def.Retry.BudgetMS,
		InlineAbsorbMaxMS:         def.Retry.InlineAbsorbMaxMS,
		HeaderTimeoutMS:           def.Retry.HeaderTimeoutMS,
		BodyIdleMS:                def.Retry.BodyIdleMS,
		BlockedModels:             []string{"*fable*"}, // changed from []: restart-gated
		QuotaProbeIntervalSeconds: def.QuotaProbe.IntervalSeconds,
		MetricsRetentionDays:      def.Metrics.RetentionDays,
		UpdateCheckEnabled:        def.Update.CheckEnabled,
		UpdateCheckIntervalHours:  def.Update.CheckIntervalHours,
	}
	applied, err := h.local.UpdateSettings(context.Background(), s)
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	if !slices.Contains(applied.Live, "switchThreshold") {
		t.Errorf("Live = %v, want it to contain switchThreshold", applied.Live)
	}
	if slices.Contains(applied.Live, "sessionAffinity") {
		t.Errorf("Live = %v, must not contain sessionAffinity (unchanged)", applied.Live)
	}
	if !slices.Contains(applied.NeedsRestart, "blockedModels") {
		t.Errorf("NeedsRestart = %v, want it to contain blockedModels", applied.NeedsRestart)
	}
	for _, unchanged := range []string{"retryBudgetMs", "inlineAbsorbMaxMs", "headerTimeoutMs", "bodyIdleMs",
		"quotaProbeIntervalSeconds", "metricsRetentionDays"} {
		if slices.Contains(applied.NeedsRestart, unchanged) {
			t.Errorf("NeedsRestart = %v, must not contain unchanged field %q", applied.NeedsRestart, unchanged)
		}
	}
}

// Settings must round-trip exactly what UpdateSettings wrote — the getter
// that lets a caller read-modify-write through the seam instead of reaching
// around Source to the config store.
func TestSettingsRoundTripsWhatUpdateSettingsWrote(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	want := Settings{
		SwitchThreshold: 0.7, RetryBudgetMS: 9000, InlineAbsorbMaxMS: 3000,
		HeaderTimeoutMS: 45000, BodyIdleMS: 90000, SessionAffinity: false,
		BlockedModels: []string{"*fable*", "*mythos*"}, QuotaProbeIntervalSeconds: 200,
		MetricsRetentionDays: 45, UpdateCheckEnabled: true, UpdateCheckIntervalHours: 24,
	}
	if _, err := h.local.UpdateSettings(context.Background(), want); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	got, err := h.local.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Settings() = %+v, want %+v", got, want)
	}
}

// A read-modify-write cycle through Settings/UpdateSettings must preserve
// every field the caller did not touch — including a bool that is false and
// a slice that is non-empty, the two shapes that a caller reconstructing
// Settings from scratch (rather than reading it first) would silently zero.
func TestSettingsReadModifyWritePreservesUntouchedFields(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	initial := Settings{
		SwitchThreshold: 0.7, RetryBudgetMS: 9000, InlineAbsorbMaxMS: 3000,
		HeaderTimeoutMS: 45000, BodyIdleMS: 90000, SessionAffinity: false,
		BlockedModels: []string{"*fable*"}, QuotaProbeIntervalSeconds: 200,
		MetricsRetentionDays: 45, UpdateCheckEnabled: true, UpdateCheckIntervalHours: 24,
	}
	if _, err := h.local.UpdateSettings(context.Background(), initial); err != nil {
		t.Fatalf("UpdateSettings (seed): %v", err)
	}

	current, err := h.local.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	current.SwitchThreshold = 0.85 // the only field this caller means to change

	if _, err := h.local.UpdateSettings(context.Background(), current); err != nil {
		t.Fatalf("UpdateSettings (read-modify-write): %v", err)
	}

	final, err := h.local.Settings(context.Background())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if final.SwitchThreshold != 0.85 {
		t.Errorf("SwitchThreshold = %v, want 0.85", final.SwitchThreshold)
	}
	if final.SessionAffinity != false {
		t.Errorf("SessionAffinity = %v, want false — an untouched field must survive a read-modify-write", final.SessionAffinity)
	}
	if len(final.BlockedModels) != 1 || final.BlockedModels[0] != "*fable*" {
		t.Errorf("BlockedModels = %v, want [*fable*] — an untouched field must survive a read-modify-write", final.BlockedModels)
	}
}

func TestSubscribeReceivesPublishedEvents(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := h.local.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	h.local.Publish(Event{Model: "claude-sonnet-5", Account: "a", Status: 200})

	select {
	case ev := <-ch:
		if ev.Model != "claude-sonnet-5" {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber never received the published event")
	}
}

// Login is a thin lookup-and-passthrough: it must find the named provider
// and hand its session straight back, unaltered.
func TestLoginReturnsTheNamedProvidersSession(t *testing.T) {
	want := provider.LoginSession{URL: "https://example.invalid/authorize?state=xyz"}
	providers := map[string]provider.Provider{
		"stub":      stubProvider{},
		"loginable": loginableProvider{session: want},
	}
	h := newHarnessWithProviders(t, nil, providers)

	got, err := h.local.Login(context.Background(), "loginable")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.URL != want.URL {
		t.Errorf("URL = %q, want %q", got.URL, want.URL)
	}
}

// The absent case: a provider name nothing registered must be a clear
// error, not a nil session silently returned.
func TestLoginUnknownProviderNameReturnsError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.local.Login(context.Background(), "does-not-exist"); err == nil {
		t.Error("want an error for an unregistered provider name")
	}
}

// Login must surface whatever error the provider itself returns (e.g. a
// provider with no interactive login at all).
func TestLoginPropagatesTheProvidersOwnError(t *testing.T) {
	providers := map[string]provider.Provider{
		"stub":      stubProvider{},
		"loginable": loginableProvider{err: provider.ErrUnsupported},
	}
	h := newHarnessWithProviders(t, nil, providers)

	if _, err := h.local.Login(context.Background(), "loginable"); !errors.Is(err, provider.ErrUnsupported) {
		t.Errorf("err = %v, want provider.ErrUnsupported", err)
	}
}

// The single most likely place to leak a credential is this flow. This runs
// a genuine login end to end — a real anthropic.Provider, a fake token
// endpoint that hands back a known sentinel access/refresh token, a real
// loopback callback — through Local.Login exactly as production drives it,
// and checks the actual bytes of everything this layer produces: the
// LoginSession (its URL), the LoginResult, and anything logged during the
// flow. (The remaining leg — that the sentinel never reaches a control-API
// poll response — is exercised with the same rigor, real secrets included,
// by internal/proxy's TestControlAPILoginFlowSucceedsAndNeverLeaksCredentialMaterial;
// view has no HTTP layer of its own to poll.)
//
// Before this fix, `secret` was never placed into any real input — the
// session and result here were hand-built stubs — so the strings.Contains
// half of this test was inert and could not have failed no matter what
// leaked; only the reflection field-name check below had any teeth.
func TestLoginSessionNeverCarriesCredentialMaterial(t *testing.T) {
	const secret = "sk-ant-super-secret-value"
	up := testutil.NewFakeUpstream(t,
		testutil.Script{Status: 200, Body: `{"access_token":"` + secret + `","refresh_token":"rt-` + secret + `","expires_in":3600}`},
		testutil.Script{Status: 200, Body: `{"account":{"uuid":"acct-1","email":"a@example.com",
			"display_name":"A"},"organization":{"uuid":"org-1","name":"Acme"}}`},
	)
	real := anthropic.New(http.DefaultClient)
	real.TokenEndpointOverride = up.URL()
	real.BaseURLOverride = up.URL()
	real.LoginTimeoutOverride = 5 * time.Second

	// Captures anything logged anywhere through the default logger for the
	// duration of the flow; nothing in this path logs today, but a future
	// regression that adds a log call anywhere Login's real code runs
	// through must be caught here, not just by the code that writes it.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	providers := map[string]provider.Provider{
		"stub":      stubProvider{},
		"anthropic": real,
	}
	h := newHarnessWithProviders(t, nil, providers)

	sess, err := h.local.Login(context.Background(), "anthropic")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	u, err := url.Parse(sess.URL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	state := u.Query().Get("state")
	redirectURI := u.Query().Get("redirect_uri")
	if state == "" || redirectURI == "" {
		t.Fatalf("authorize URL missing state/redirect_uri: %s", sess.URL)
	}

	cbRes, err := http.Get(redirectURI + "?code=auth-code-1&state=" + state)
	if err != nil {
		t.Fatalf("simulate callback: %v", err)
	}
	cbRes.Body.Close()

	var result provider.LoginResult
	select {
	case res, ok := <-sess.Done:
		if !ok {
			t.Fatal("Done closed with no value sent")
		}
		result = res
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for LoginResult")
	}
	if result.Err != nil {
		t.Fatalf("LoginResult.Err = %v, want nil", result.Err)
	}
	if result.Profile.Email != "a@example.com" {
		t.Errorf("Profile = %+v, want the real exchange's profile", result.Profile)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blob := string(raw) + " " + sess.URL + " " + logBuf.String()
	for _, leak := range []string{secret, "rt-" + secret} {
		if strings.Contains(blob, leak) {
			t.Errorf("credential material %q leaked into %q", leak, blob)
		}
	}
	// LoginResult's own shape enforces this at compile time (Profile, Err —
	// no credential field exists to leak), but assert it structurally too so
	// a future field addition to the type is caught here rather than only by
	// code review.
	rt := reflect.TypeOf(result)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if name != "Profile" && name != "Err" {
			t.Errorf("LoginResult gained an unexpected field %q; verify it cannot carry credential material", name)
		}
	}
}

func TestProbeNowTriggersTheProberAndReturnsItsError(t *testing.T) {
	fp := &fakeQuotaProvider{err: errors.New("boom")}
	providers := map[string]provider.Provider{"stub": fp}
	h := newHarnessWithProviders(t, nil, providers, acctCfgWithProvider("a", "stub"))

	err := h.local.ProbeNow(context.Background())
	if err == nil {
		t.Fatal("want the prober's error surfaced")
	}
	if fp.calls != 1 {
		t.Errorf("Quota called %d times, want exactly 1", fp.calls)
	}
}

func TestProbeNowOnSuccessUpdatesAccountQuota(t *testing.T) {
	fp := &fakeQuotaProvider{buckets: []provider.QuotaBucket{{Name: "5h", Utilization: 0.3}}}
	providers := map[string]provider.Provider{"stub": fp}
	h := newHarnessWithProviders(t, nil, providers, acctCfgWithProvider("a", "stub"))

	if err := h.local.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	got, _ := h.mgr.Get("a")
	if got.Buckets["5h"].Utilization != 0.3 {
		t.Errorf("Buckets = %+v, want 5h=0.3", got.Buckets)
	}
}

// fakeQuotaProvider is a stubProvider whose Quota is controllable, for
// exercising Local.ProbeNow without a real prober cycle's full generality
// (already covered in internal/prober's own tests).
type fakeQuotaProvider struct {
	stubProvider
	buckets []provider.QuotaBucket
	err     error
	calls   int
}

func (f *fakeQuotaProvider) Quota(context.Context, provider.Credential) (provider.Quota, error) {
	f.calls++
	if f.err != nil {
		return provider.Quota{}, f.err
	}
	return provider.Quota{Buckets: f.buckets}, nil
}

func acctCfgWithProvider(id, providerName string) config.Account {
	return config.Account{
		ID: id, Provider: providerName, Label: id,
		Credential: provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"},
	}
}

// writeFileT writes body to dir/name, creating parent directories as needed.
func writeFileT(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestImportCredentialsAddsAccountsToManagerWithoutRestart(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFileT(t, config.LegacyPath(), `{"accounts":[
		{"name":"a@example.com (Acme)","type":"oauth","accessToken":"at-1","refreshToken":"rt-1",
		 "accountUuid":"uuid-1"}
	]}`)

	added, err := h.local.ImportCredentials(context.Background(), config.ImportSourceLegacy)
	if err != nil {
		t.Fatalf("ImportCredentials: %v", err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}

	all := h.mgr.All()
	if len(all) != 1 || all[0].Label != "a@example.com (Acme)" {
		t.Errorf("manager accounts = %+v, want the imported account live", all)
	}
	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Errorf("persisted accounts = %+v, want 1", cfg.Accounts)
	}
}

// Importing the same source twice must not duplicate accounts: dedupe on
// the credential's account uuid.
func TestImportCredentialsTwiceDedupesOnAccountUUID(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeFileT(t, config.LegacyPath(), `{"accounts":[
		{"name":"a@example.com","type":"oauth","accessToken":"at-1","refreshToken":"rt-1",
		 "accountUuid":"uuid-1"}
	]}`)

	if _, err := h.local.ImportCredentials(context.Background(), config.ImportSourceLegacy); err != nil {
		t.Fatalf("first ImportCredentials: %v", err)
	}
	added, err := h.local.ImportCredentials(context.Background(), config.ImportSourceLegacy)
	if err != nil {
		t.Fatalf("second ImportCredentials: %v", err)
	}
	if added != 0 {
		t.Errorf("second import added = %d, want 0 (already present)", added)
	}
	if len(h.mgr.All()) != 1 {
		t.Errorf("manager accounts = %+v, want exactly 1 (no duplicate)", h.mgr.All())
	}
}

// The claude-code source carries no account uuid at all, so dedupe must
// fall back to the label — the case a suite that only tests the uuid path
// would miss.
func TestImportCredentialsTwiceDedupesOnLabelWhenNoUUID(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeFileT(t, config.ClaudeCodePath(),
		`{"claudeAiOauth":{"accessToken":"at-9","refreshToken":"rt-9","subscriptionType":"max"}}`)

	if _, err := h.local.ImportCredentials(context.Background(), config.ImportSourceClaudeCode); err != nil {
		t.Fatalf("first ImportCredentials: %v", err)
	}
	added, err := h.local.ImportCredentials(context.Background(), config.ImportSourceClaudeCode)
	if err != nil {
		t.Fatalf("second ImportCredentials: %v", err)
	}
	if added != 0 {
		t.Errorf("second import added = %d, want 0 (deduped on label)", added)
	}
	if len(h.mgr.All()) != 1 {
		t.Errorf("manager accounts = %+v, want exactly 1", h.mgr.All())
	}
}

// The absent case: no source file at all must be a clear error and add
// nothing, not a panic or a silent zero.
func TestImportCredentialsMissingFileReturnsErrorAndAddsNothing(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // no teamclaude.json written here

	added, err := h.local.ImportCredentials(context.Background(), config.ImportSourceLegacy)
	if err == nil {
		t.Error("want an error when the source file does not exist")
	}
	if added != 0 {
		t.Errorf("added = %d, want 0", added)
	}
	if len(h.mgr.All()) != 0 {
		t.Error("manager should have gained no accounts")
	}
}

func TestImportCredentialsUnknownSourceReturnsError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.local.ImportCredentials(context.Background(), config.ImportSource("bogus")); err == nil {
		t.Error("want an error for an unknown import source")
	}
}

// The update settings round-trip through the seam like every other field, and
// the interval is restart-gated because the checker's ticker is built once.
func TestUpdateSettingsRoundTripsTheUpdateBlock(t *testing.T) {
	local := newHarness(t).local
	s, err := local.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.UpdateCheckEnabled || s.UpdateCheckIntervalHours != 24 {
		t.Fatalf("defaults not surfaced: %+v", s)
	}

	s.UpdateCheckIntervalHours = 6
	applied, err := local.UpdateSettings(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied.NeedsRestart, "updateCheckIntervalHours") {
		t.Errorf("NeedsRestart = %v, want updateCheckIntervalHours", applied.NeedsRestart)
	}

	back, err := local.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if back.UpdateCheckIntervalHours != 6 {
		t.Errorf("interval did not persist: %d", back.UpdateCheckIntervalHours)
	}
}

// A zero interval is refused before it is written: a bad value on disk
// survives a restart, which is worse than a rejected call.
func TestValidateRejectsANonPositiveUpdateInterval(t *testing.T) {
	s := Settings{
		SwitchThreshold: 0.9, RetryBudgetMS: 1000, HeaderTimeoutMS: 1000, BodyIdleMS: 1000,
		QuotaProbeIntervalSeconds: 300, MetricsRetentionDays: 90,
		UpdateCheckEnabled: true, UpdateCheckIntervalHours: 0,
	}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a zero updateCheckIntervalHours")
	}
}
