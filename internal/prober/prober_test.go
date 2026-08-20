package prober

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// fakeProvider is a controllable provider.Provider for prober tests: Quota's
// behaviour is driven by a queue of canned results (consumed in order, last
// one repeating), and every call is counted and optionally blocked until
// released, so a test can observe exactly how many times — and in what
// overlap — the prober called it.
type fakeProvider struct {
	mu      sync.Mutex
	results []quotaResult
	n       int
	calls   atomic.Int32

	// block, if non-nil, is read from before Quota returns, so a test can
	// hold a cycle "in flight" deliberately.
	block <-chan struct{}
	// started, if non-nil, receives once per Quota call, before blocking.
	started chan struct{}

	// requireToken, when non-empty, makes Quota reject any credential whose
	// access token differs — the way a real usage endpoint rejects a stale
	// one. Empty (the default) accepts anything, so existing tests are
	// unaffected.
	requireToken string
	// refreshTo is the credential Refresh hands back; refreshErr overrides it
	// with a failure. refreshes counts the calls.
	refreshTo  provider.Credential
	refreshErr error
	refreshes  atomic.Int32

	// models and modelsErr control the Models method's return value.
	// modelCalls counts them, which is what lets a test assert that catalogue
	// discovery ran at all — the thing that silently stopped happening whenever
	// the quota read failed.
	models     []provider.Model
	modelsErr  error
	modelCalls atomic.Int32
}

type quotaResult struct {
	quota provider.Quota
	err   error
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Refresh(context.Context, provider.Credential) (provider.Credential, error) {
	f.refreshes.Add(1)
	if f.refreshErr != nil {
		return provider.Credential{}, f.refreshErr
	}
	return f.refreshTo, nil
}
func (f *fakeProvider) Profile(context.Context, provider.Credential) (provider.Profile, error) {
	return provider.Profile{}, provider.ErrUnsupported
}
func (f *fakeProvider) Models(_ context.Context, _ provider.Credential) ([]provider.Model, error) {
	f.modelCalls.Add(1)
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return f.models, nil
}
func (f *fakeProvider) Login(context.Context) (provider.LoginSession, error) {
	return provider.LoginSession{}, provider.ErrUnsupported
}
func (f *fakeProvider) Endpoint(provider.Account) *url.URL {
	u, _ := url.Parse("https://upstream.invalid")
	return u
}
func (f *fakeProvider) Authorize(*http.Request, provider.Credential)             {}
func (f *fakeProvider) RewriteBody(b []byte, _ provider.Account) ([]byte, error) { return b, nil }
func (f *fakeProvider) ClassifyResponse(*http.Response) provider.Outcome         { return provider.Outcome{} }
func (f *fakeProvider) ParseUsage([]byte) (*provider.UsageDelta, bool)           { return nil, false }
func (f *fakeProvider) ParseUsageBody([]byte) (*provider.UsageDelta, bool)       { return nil, false }

func (f *fakeProvider) Quota(ctx context.Context, c provider.Credential) (provider.Quota, error) {
	f.calls.Add(1)
	// Both real providers answer ErrUnsupported for an API key rather than
	// relying on the caller to know they would; the prober now honours that
	// answer instead of pre-judging it, so the fake has to give it.
	if c.Type == provider.CredentialAPIKey {
		return provider.Quota{}, provider.ErrUnsupported
	}
	if f.requireToken != "" && c.AccessToken != f.requireToken {
		return provider.Quota{}, errors.New("usage: HTTP 401")
	}
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return provider.Quota{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.n
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.n++
	if i < 0 {
		return provider.Quota{}, errors.New("fakeProvider: no results configured")
	}
	return f.results[i].quota, f.results[i].err
}

func (f *fakeProvider) callCount() int      { return int(f.calls.Load()) }
func (f *fakeProvider) modelCallCount() int { return int(f.modelCalls.Load()) }
func (f *fakeProvider) refreshCount() int   { return int(f.refreshes.Load()) }

func oauthAcct(id string) config.Account {
	return config.Account{
		ID: id, Provider: "fake", Label: id,
		Credential: provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at"},
	}
}

func apiKeyAcct(id string) config.Account {
	return config.Account{
		ID: id, Provider: "fake", Label: id,
		Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk"},
	}
}

func newMgr(t *testing.T, fp *fakeProvider, accts ...config.Account) *account.Manager {
	t.Helper()
	return account.New(accts, map[string]provider.Provider{"fake": fp}, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})
}

func quotaOK(buckets ...provider.QuotaBucket) quotaResult {
	return quotaResult{quota: provider.Quota{Buckets: buckets, ObservedAt: 1}}
}

func quotaThrottled() quotaResult {
	return quotaResult{err: provider.ErrQuotaThrottled}
}

// The empty case: no accounts at all must be a no-op success, not an error —
// exactly the shape a suite that only ever seeds accounts would miss.
func TestProbeNowOnEmptyManagerSucceeds(t *testing.T) {
	fp := &fakeProvider{}
	mgr := newMgr(t, fp)
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if fp.callCount() != 0 {
		t.Errorf("callCount = %d, want 0 with no accounts", fp.callCount())
	}
}

func TestProbeNowUpdatesManagerQuotaForEachOAuthAccount(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{quotaOK(provider.QuotaBucket{Name: "5h", Utilization: 0.4})}}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	got, ok := mgr.Get("a")
	if !ok {
		t.Fatal("account a should exist")
	}
	if got.Buckets["5h"].Utilization != 0.4 {
		t.Errorf("Buckets = %+v, want 5h=0.4 fed through UpdateQuota", got.Buckets)
	}
}

// An API-key account has no usage endpoint, and the PROVIDER is what says so:
// it answers ErrUnsupported, which is neither a success nor a failure and must
// not be recorded as either — a recorded error would log the same non-event
// every cycle forever.
//
// The prober no longer refuses to ask. It used to skip non-OAuth accounts
// outright, which also skipped the catalogue read, so an API-key-only
// deployment had a permanently empty /v1/models — even though anthropic.Models
// authenticates perfectly well with an API key. Deciding what a credential can
// reach belongs to the provider; the prober asks and honours the answer.
func TestProbeNowTreatsUnsupportedQuotaAsANonEventButStillReadsModels(t *testing.T) {
	fp := &fakeProvider{
		results: []quotaResult{quotaOK()},
		models:  []provider.Model{{ID: "claude-opus-5", DisplayName: "Opus"}},
	}
	mgr := newMgr(t, fp, apiKeyAcct("k"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if got := p.Status().Accounts["k"].LastError; got != "" {
		t.Errorf("LastError = %q, want empty: ErrUnsupported is not a probe failure", got)
	}
	if got := p.Status().Accounts["k"].LastSuccessAt; got != 0 {
		t.Errorf("LastSuccessAt = %d, want 0: nothing was read, so nothing succeeded", got)
	}
	if fp.modelCallCount() != 1 {
		t.Fatalf("Models calls = %d, want 1: an API key can still have a catalogue", fp.modelCallCount())
	}
	a, _ := mgr.Get("k")
	if len(a.Models) != 1 || a.Models[0].ID != "claude-opus-5" {
		t.Errorf("catalogue = %+v, want the discovered model", a.Models)
	}
}

// IMPORTANT 5. A failing quota read used to `continue` before Models was ever
// called, so once wham/usage started failing — which spec §10 anticipates, it
// is a private endpoint — the catalogue was never refreshed again even though
// wham/models was healthy. An empty catalogue also silently disables
// account.servesModel filtering, since "unknown" has to mean "serves anything".
func TestProbeNowReadsModelsEvenWhenTheQuotaReadFails(t *testing.T) {
	fp := &fakeProvider{
		results: []quotaResult{quotaThrottled()},
		models:  []provider.Model{{ID: "gpt-5-codex", DisplayName: "Codex"}},
	}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err == nil {
		t.Fatal("want the throttled quota read to still be reported as the cycle's error")
	}
	if fp.modelCallCount() != 1 {
		t.Fatalf("Models calls = %d, want 1: the catalogue read must not depend on the quota read", fp.modelCallCount())
	}
	a, _ := mgr.Get("a")
	if len(a.Models) != 1 || a.Models[0].ID != "gpt-5-codex" {
		t.Errorf("catalogue = %+v, want the discovered model despite the quota failure", a.Models)
	}
}

// A failing catalogue read must not be mistaken for a failing quota read: it is
// not recorded as the account's probe error and must not arm the quota backoff.
func TestProbeNowKeepsAModelsFailureOutOfTheQuotaBackoff(t *testing.T) {
	fp := &fakeProvider{
		results:   []quotaResult{quotaOK(provider.QuotaBucket{Name: "5h", Utilization: 0.2})},
		modelsErr: errors.New("models: HTTP 500"),
	}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	st := p.Status().Accounts["a"]
	if st.LastError != "" {
		t.Errorf("LastError = %q, want empty: the quota read succeeded", st.LastError)
	}
	if st.NextAttemptAt != 0 {
		t.Errorf("NextAttemptAt = %d, want 0: a models failure must not arm the quota backoff", st.NextAttemptAt)
	}
	a, _ := mgr.Get("a")
	if a.Buckets["5h"].Utilization != 0.2 {
		t.Errorf("Buckets = %+v, want the successful quota read preserved", a.Buckets)
	}
}

// The core failure this prober exists to prevent: hammering a throttled
// account. Backoff must apply per account, grow exponentially while
// throttling continues, and reset the instant a probe succeeds.
func TestProbeNowBacksOffExponentiallyOnThrottlingAndResetsOnSuccess(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{
		quotaThrottled(), quotaThrottled(), quotaOK(provider.QuotaBucket{Name: "5h", Utilization: 0.1}),
	}}
	mgr := newMgr(t, fp, oauthAcct("a"))

	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	cur := base
	clock := func() time.Time { return cur }
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour,
		WithClock(clock), WithBackoff(10*time.Second, time.Hour))

	// Cycle 1: throttled. Backoff is set to the base (10s).
	if err := p.ProbeNow(context.Background()); err == nil {
		t.Error("want a non-nil error reporting the throttled account")
	}
	if fp.callCount() != 1 {
		t.Fatalf("callCount = %d, want 1", fp.callCount())
	}
	st := p.Status().Accounts["a"]
	if st.NextAttemptAt != cur.Add(10*time.Second).UnixMilli() {
		t.Errorf("NextAttemptAt = %d, want base backoff of 10s from now", st.NextAttemptAt)
	}

	// Still inside the backoff window: ProbeNow must skip this account
	// entirely rather than calling Quota again.
	cur = cur.Add(5 * time.Second)
	if err := p.ProbeNow(context.Background()); err != nil {
		t.Errorf("ProbeNow while backing off should not itself error: %v", err)
	}
	if fp.callCount() != 1 {
		t.Fatalf("callCount = %d, want still 1 (account is backing off)", fp.callCount())
	}

	// Past the first backoff window: the second throttled response DOUBLES
	// the backoff (to 20s), not resets it to the base.
	cur = cur.Add(10 * time.Second) // now base+15s, past the first 10s window
	if err := p.ProbeNow(context.Background()); err == nil {
		t.Error("want a non-nil error: still throttled")
	}
	if fp.callCount() != 2 {
		t.Fatalf("callCount = %d, want 2", fp.callCount())
	}
	st = p.Status().Accounts["a"]
	wantNext := cur.Add(20 * time.Second).UnixMilli()
	if st.NextAttemptAt != wantNext {
		t.Errorf("NextAttemptAt = %d, want %d (backoff doubled to 20s)", st.NextAttemptAt, wantNext)
	}

	// Past the doubled window: this time the fake succeeds. Backoff must
	// reset entirely (NextAttemptAt back to 0, no pending backoff) and the
	// account's quota must be updated.
	cur = cur.Add(25 * time.Second)
	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	st = p.Status().Accounts["a"]
	if st.NextAttemptAt != 0 {
		t.Errorf("NextAttemptAt = %d, want 0 after a success resets backoff", st.NextAttemptAt)
	}
	if st.LastError != "" {
		t.Errorf("LastError = %q, want cleared after success", st.LastError)
	}
	if st.LastSuccessAt == 0 {
		t.Error("LastSuccessAt should be stamped after a success")
	}
	got, _ := mgr.Get("a")
	if got.Buckets["5h"].Utilization != 0.1 {
		t.Errorf("Buckets = %+v, want the post-recovery quota applied", got.Buckets)
	}
}

// A network error (as opposed to a throttling 429) must be recorded but must
// NOT trigger backoff — the account is retried at the ordinary cadence next
// cycle, since a one-off blip is not the same failure mode as being
// rate-limited.
func TestProbeNowRecordsANonThrottleErrorWithoutBackingOff(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{{err: errors.New("dial tcp: connection refused")}}}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err == nil {
		t.Error("want the network error surfaced")
	}
	st := p.Status().Accounts["a"]
	if st.LastError == "" {
		t.Error("LastError should record the failure")
	}
	if st.NextAttemptAt != 0 {
		t.Errorf("NextAttemptAt = %d, want 0: a non-throttle error must not back off", st.NextAttemptAt)
	}
}

// Overlapping cycles must not stack: a ProbeNow call that arrives while
// another cycle (background or manual) is already running must join it
// rather than launching a second concurrent call to the same account's
// Quota.
func TestProbeNowWhileRunningJoinsRatherThanStackingASecondCycle(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 4)
	fp := &fakeProvider{
		results: []quotaResult{quotaOK()},
		block:   block, started: started,
	}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	firstDone := make(chan error, 1)
	go func() { firstDone <- p.ProbeNow(context.Background()) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first ProbeNow never started its Quota call")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- p.ProbeNow(context.Background()) }()

	// Give the second call every chance to (wrongly) start its own Quota
	// call before we unblock the first.
	time.Sleep(50 * time.Millisecond)
	close(block)

	select {
	case err := <-firstDone:
		if err != nil {
			t.Errorf("first ProbeNow: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first ProbeNow never returned")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Errorf("second ProbeNow: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second ProbeNow never returned (joined cycle never signalled)")
	}

	if got := fp.callCount(); got != 1 {
		t.Errorf("Quota was called %d times, want exactly 1 (no stacked cycle)", got)
	}
}

// A caller waiting to join an in-flight cycle must still respect its own
// context: a cancelled/expired ctx must return promptly rather than hang
// until the in-flight cycle happens to finish.
func TestProbeNowJoiningAnInFlightCycleRespectsCallerContext(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	defer close(block) // release the first cycle at test end either way
	fp := &fakeProvider{results: []quotaResult{quotaOK()}, block: block, started: started}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	go p.ProbeNow(context.Background())
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("cycle never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.ProbeNow(ctx)
	if err == nil {
		t.Fatal("want a context error while the first cycle is still blocked")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ProbeNow took %v to respect a 30ms context deadline", elapsed)
	}
}

// An interval of 0 disables the background loop (spec §6.2), but a manual
// ProbeNow must still work — the on-demand trigger is independent of
// whether periodic probing is configured.
func TestIntervalZeroDisablesTheBackgroundLoopButNotProbeNow(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{quotaOK()}}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, 0)

	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	if fp.callCount() != 0 {
		t.Errorf("callCount = %d, want 0: interval 0 must never tick", fp.callCount())
	}

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if fp.callCount() != 1 {
		t.Errorf("callCount = %d, want 1: ProbeNow must still work with the loop disabled", fp.callCount())
	}
}

// Start used to wait a full interval before its first cycle, and nothing calls
// ProbeNow at boot, so with the default 300s interval the proxy spent its first
// five minutes with no quota data at all. Buckets are not persisted across
// restarts, so this is not merely stale data but absent data: selection cannot
// apply switchThreshold to an account it knows nothing about, and the first
// requests after a restart can be sent to an account that is already spent.
func TestStartProbesImmediatelyRatherThanWaitingAnInterval(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{quotaOK(provider.QuotaBucket{Name: "5h", Utilization: 0.3})}}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, 10*time.Minute)

	p.Start()
	defer p.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for fp.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fp.callCount(); got != 1 {
		t.Fatalf("callCount = %d, want 1 promptly after Start, not one interval later", got)
	}
	if got := mgr.All()[0].Buckets["5h"].Utilization; got != 0.3 {
		t.Errorf("utilization = %v, want the startup probe to have populated it", got)
	}
}

func TestStartTicksAtTheConfiguredIntervalAndStopEndsIt(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{quotaOK()}}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, 20*time.Millisecond)

	p.Start()
	time.Sleep(110 * time.Millisecond)
	p.Stop()

	got := fp.callCount()
	if got < 2 {
		t.Errorf("callCount = %d, want at least 2 ticks in 110ms at a 20ms interval", got)
	}

	// Nothing further should happen after Stop.
	after := fp.callCount()
	time.Sleep(60 * time.Millisecond)
	if fp.callCount() != after {
		t.Error("a tick fired after Stop")
	}
}

// Stop immediately after Start, before any tick or ProbeNow ever ran, must
// not hang — the disabled-interval loop and the ticking loop both have to
// observe the stop signal on every path, not only mid-tick.
// The prober is the one place a probe failure is logged at all (Status()
// alone is invisible to a headless instance with no UI polling it), and a
// log line is exactly the kind of place a credential leaks by accident. This
// asserts the account's access token never appears in anything the prober
// logs, even on a throttled/failing account.
func TestProbeNowNeverLogsCredentialMaterial(t *testing.T) {
	const secretToken = "sk-ant-super-secret-probe-token"
	fp := &fakeProvider{results: []quotaResult{quotaThrottled()}}
	acct := oauthAcct("a")
	acct.Credential.AccessToken = secretToken
	mgr := newMgr(t, fp, acct)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour, WithLogger(log))

	if err := p.ProbeNow(context.Background()); err == nil {
		t.Fatal("want the throttled error surfaced")
	}
	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("prober logged credential material: %s", buf.String())
	}
	if buf.Len() == 0 {
		t.Error("want the throttled probe to have logged something (a headless instance has no other visibility into it)")
	}
}

func TestStopImmediatelyAfterStartDoesNotHang(t *testing.T) {
	fp := &fakeProvider{}
	mgr := newMgr(t, fp)
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	done := make(chan struct{})
	go func() { p.Start(); p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start/Stop hung")
	}
}

// I1: Stop must not wait for an in-flight cycle to run to completion on its
// own. Before the fix, the background loop always called runCycle with
// context.Background(), so a cycle blocked on a slow/hung Quota call (a real
// failure mode: the control-plane client's own timeout is per-account, not
// per-cycle) stalled Stop — and so process shutdown — for however long that
// call happened to take. This blocks Quota deliberately and asserts Stop
// still returns promptly.
func TestStopReturnsPromptlyWhileACycleIsBlockedOnQuota(t *testing.T) {
	block := make(chan struct{}) // deliberately never closed
	started := make(chan struct{}, 1)
	fp := &fakeProvider{results: []quotaResult{quotaOK()}, block: block, started: started}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, 10*time.Millisecond)

	p.Start()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background loop never started its Quota call")
	}

	stopDone := make(chan struct{})
	go func() { p.Stop(); close(stopDone) }()

	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked on the in-flight cycle instead of cancelling it")
	}
}

// Minor: Start/Stop must be safe to call more than once — Roller has the
// same contract (this package makes both idempotent via sync.Once rather
// than merely documenting "call once", so a duplicate call is silent instead
// of panicking on a double close or racing a second competing loop).
func TestStartAndStopAreSafeToCallTwice(t *testing.T) {
	fp := &fakeProvider{}
	mgr := newMgr(t, fp)
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	done := make(chan struct{})
	go func() {
		p.Start()
		p.Start()
		p.Stop()
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a duplicate Start or Stop call hung or panicked")
	}
}

// Minor: an account removed from the live Manager must eventually disappear
// from Status().Accounts too, not accumulate as a ghost entry forever.
func TestStatusPrunesAccountsNoLongerInTheManager(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{quotaOK()}}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if _, ok := p.Status().Accounts["a"]; !ok {
		t.Fatal("account a should be recorded in Status() after a probe")
	}

	if err := mgr.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if _, ok := p.Status().Accounts["a"]; ok {
		t.Error("Status().Accounts still has account \"a\" after it was removed from the Manager")
	}
}

// expiringOAuthAcct is an OAuth account whose access token expires at
// expiresAt, carrying a refresh token so a renewal is actually possible.
func expiringOAuthAcct(id, token string, expiresAt time.Time) config.Account {
	return config.Account{
		ID: id, Provider: "fake", Label: id,
		Credential: provider.Credential{
			Type:         provider.CredentialOAuth,
			AccessToken:  token,
			RefreshToken: "rt",
			ExpiresAt:    expiresAt.UnixMilli(),
		},
	}
}

// The prober read a.Credential straight off the manager snapshot and never
// asked for it to be renewed, so an access token that expired while the proxy
// sat idle made every cycle fail with HTTP 401 — and, since that is not a
// throttling error, fail again every interval with no backoff. Utilization
// then stayed frozen at its last reading until an inference request happened
// to refresh the token on its own path, which is exactly when an operator
// least wants to discover their quota numbers were fiction.
func TestProbeRefreshesAnExpiredCredentialBeforeReading(t *testing.T) {
	fp := &fakeProvider{
		results:      []quotaResult{quotaOK(provider.QuotaBucket{Name: "5h", Utilization: 0.42})},
		requireToken: "fresh",
		refreshTo: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "fresh", RefreshToken: "rt2",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}
	mgr := newMgr(t, fp, expiringOAuthAcct("a", "stale", time.Now().Add(-time.Minute)))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if got := fp.refreshCount(); got != 1 {
		t.Errorf("refreshes = %d, want 1: the probe must renew an expired credential", got)
	}
	got := mgr.All()
	if len(got) != 1 || got[0].Buckets["5h"].Utilization != 0.42 {
		t.Errorf("buckets = %+v, want the 5h reading recorded", got[0].Buckets)
	}
	if st := p.Status().Accounts["a"]; st.LastError != "" {
		t.Errorf("LastError = %q, want empty", st.LastError)
	}
}

// A credential nowhere near expiry must not be renewed. The probe runs on a
// timer against every account, so a refresh-on-every-cycle would rotate the
// refresh token continuously and turn a read-only health check into the
// noisiest writer in the system.
func TestProbeDoesNotRefreshACredentialThatIsStillValid(t *testing.T) {
	fp := &fakeProvider{
		results:      []quotaResult{quotaOK(provider.QuotaBucket{Name: "5h", Utilization: 0.1})},
		requireToken: "current",
	}
	mgr := newMgr(t, fp, expiringOAuthAcct("a", "current", time.Now().Add(time.Hour)))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if got := fp.refreshCount(); got != 0 {
		t.Errorf("refreshes = %d, want 0 for a credential an hour from expiry", got)
	}
}

// When the renewal itself fails there is nothing to probe with, so the cycle
// records the failure rather than spending a call on a credential already
// known to be rejected.
func TestProbeRecordsARefreshFailureWithoutCallingQuota(t *testing.T) {
	fp := &fakeProvider{
		results:      []quotaResult{quotaOK()},
		requireToken: "fresh",
		refreshErr:   errors.New("refresh token revoked"),
	}
	mgr := newMgr(t, fp, expiringOAuthAcct("a", "stale", time.Now().Add(-time.Minute)))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err == nil {
		t.Fatal("ProbeNow returned nil, want the refresh failure surfaced")
	}
	if got := fp.callCount(); got != 0 {
		t.Errorf("Quota called %d times, want 0 when the credential could not be renewed", got)
	}
	st := p.Status().Accounts["a"]
	if !strings.Contains(st.LastError, "refresh") {
		t.Errorf("LastError = %q, want it to name the refresh failure", st.LastError)
	}
}

// The catalogue is read on the same cycle as quota: both are per-account facts
// that go stale, and both are read from the same credential we just renewed.
func TestProbeRefreshesTheModelCatalogue(t *testing.T) {
	fp := &fakeProvider{
		results: []quotaResult{quotaOK()},
		models:  []provider.Model{{ID: "gpt-5.6-sol"}},
	}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Fatalf("ProbeNow: %v", err)
	}
	if got := mgr.All()[0].Models; len(got) != 1 || got[0].ID != "gpt-5.6-sol" {
		t.Errorf("models = %+v, want the discovered catalogue", got)
	}
}

// A provider with no catalogue endpoint must not make the cycle look failed.
func TestProbeToleratesUnsupportedModels(t *testing.T) {
	fp := &fakeProvider{results: []quotaResult{quotaOK()}, modelsErr: provider.ErrUnsupported}
	mgr := newMgr(t, fp, oauthAcct("a"))
	p := New(mgr, map[string]provider.Provider{"fake": fp}, time.Hour)

	if err := p.ProbeNow(context.Background()); err != nil {
		t.Errorf("ProbeNow: %v; an unsupported catalogue is not a probe failure", err)
	}
}
