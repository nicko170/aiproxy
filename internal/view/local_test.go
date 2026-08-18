package view

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/provider"
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
	dropped func() int64
}

func newHarness(t *testing.T, accts ...config.Account) *testHarness {
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

	mgr := account.New(accts, map[string]provider.Provider{"stub": stubProvider{}}, account.Options{
		SwitchThreshold: 0.98,
		SessionAffinity: true,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	ing := metrics.NewIngester(ms, metrics.IngestOptions{})
	t.Cleanup(func() { ing.Close() })

	dropped := func() int64 { return ing.Dropped() }

	local := NewLocal(mgr, ms, cs, "127.0.0.1:3456", dropped)
	return &testHarness{t: t, local: local, mgr: mgr, ms: ms, cs: cs, ing: ing, dropped: dropped}
}

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
func (stubProvider) Endpoint(provider.Account) *url.URL {
	u, _ := url.Parse("https://upstream.invalid")
	return u
}
func (stubProvider) Authorize(*http.Request, provider.Credential)             {}
func (stubProvider) RewriteBody(b []byte, _ provider.Account) ([]byte, error) { return b, nil }
func (stubProvider) ClassifyResponse(*http.Response) provider.Outcome         { return provider.Outcome{} }
func (stubProvider) ParseUsage([]byte) (*provider.UsageDelta, bool)           { return nil, false }
func (stubProvider) ParseUsageBody([]byte) (*provider.UsageDelta, bool)       { return nil, false }

func acctCfg(id string, priority int) config.Account {
	return config.Account{
		ID: id, Provider: "stub", Label: id, Priority: priority,
		Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk-super-secret-" + id},
	}
}

func TestServerStatusReportsListenAddrAndDroppedCount(t *testing.T) {
	h := newHarness(t, acctCfg("a", 0))

	st, err := h.local.ServerStatus(context.Background())
	if err != nil {
		t.Fatalf("ServerStatus: %v", err)
	}
	if st.ListenAddr != "127.0.0.1:3456" {
		t.Errorf("ListenAddr = %q", st.ListenAddr)
	}
	if st.UptimeSeconds < 0 {
		t.Errorf("UptimeSeconds = %d, want >= 0", st.UptimeSeconds)
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
		MetricsRetentionDays: 90,
	}
	if err := h.local.UpdateSettings(context.Background(), bad); err == nil {
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
		MetricsRetentionDays: 30,
	}
	if err := h.local.UpdateSettings(context.Background(), good); err != nil {
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
