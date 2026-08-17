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

func oauthCred() provider.Credential {
	return provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
}

func mgr(t *testing.T, accts ...config.Account) *Manager {
	t.Helper()
	return New(accts, map[string]provider.Provider{"stub": &stubProvider{}}, Options{
		SwitchThreshold: 0.98,
		SessionAffinity: true,
	})
}

func acct(id string, priority int) config.Account {
	return config.Account{ID: id, Provider: "stub", Label: id, Priority: priority, Credential: oauthCred()}
}

func TestSelectPrefersLowestPriority(t *testing.T) {
	m := mgr(t, acct("high", 10), acct("low", 0))

	got, err := m.Select(SelectRequest{Model: "claude-sonnet"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "low" {
		t.Errorf("selected %q, want low", got.ID)
	}
}

func TestSelectSkipsDisabledErroredAndExcluded(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1), acct("c", 2))
	// Whitebox: mutate the live account directly. Get/All/Select hand out
	// value copies now, so there is no pointer-returning accessor to mutate
	// through from outside the package.
	m.byID["a"].Disabled = true
	m.byID["b"].Status = StatusErrored

	got, err := m.Select(SelectRequest{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "c" {
		t.Errorf("selected %q, want c", got.ID)
	}

	if _, err := m.Select(SelectRequest{Exclude: map[string]bool{"c": true}}); !errors.Is(err, ErrNoAccount) {
		t.Errorf("err = %v, want ErrNoAccount once every candidate is out", err)
	}
}

func TestSelectSkipsRateLimitedUntilItLapses(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.MarkRateLimited("a", time.Hour)

	got, _ := m.Select(SelectRequest{})
	if got.ID != "b" {
		t.Fatalf("selected %q, want b while a is rate limited", got.ID)
	}

	m.ClearRateLimited("a")
	got, _ = m.Select(SelectRequest{})
	if got.ID != "a" {
		t.Errorf("selected %q, want a once the hold clears", got.ID)
	}
}

func TestSelectSkipsAccountOverSwitchThreshold(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.UpdateQuota("a", []provider.QuotaBucket{{Name: "5h", Utilization: 0.99}})

	got, _ := m.Select(SelectRequest{Model: "claude-sonnet"})
	if got.ID != "b" {
		t.Errorf("selected %q, want b — a is over the switch threshold", got.ID)
	}
}

// A spent per-model bucket must exclude the account for THAT model only. An
// account out of one model's weekly quota still serves every other model.
func TestSelectModelScopedBucketOnlyBlocksThatModel(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.UpdateQuota("a", []provider.QuotaBucket{
		{Name: "7d_fable", Utilization: 1, Status: "rejected"},
	})

	got, _ := m.Select(SelectRequest{Model: "claude-fable-5"})
	if got.ID != "b" {
		t.Errorf("fable request selected %q, want b", got.ID)
	}

	got, _ = m.Select(SelectRequest{Model: "claude-sonnet-5"})
	if got.ID != "a" {
		t.Errorf("sonnet request selected %q, want a — only fable is spent", got.ID)
	}
}

func TestSelectBreaksPriorityTiesBySoonestReset(t *testing.T) {
	m := mgr(t, acct("later", 0), acct("sooner", 0))
	now := time.Now()
	m.UpdateQuota("later", []provider.QuotaBucket{
		{Name: "7d", Utilization: 0.5, ResetsAt: now.Add(48 * time.Hour).UnixMilli()},
	})
	m.UpdateQuota("sooner", []provider.QuotaBucket{
		{Name: "7d", Utilization: 0.5, ResetsAt: now.Add(2 * time.Hour).UnixMilli()},
	})

	got, _ := m.Select(SelectRequest{})
	if got.ID != "sooner" {
		t.Errorf("selected %q, want sooner — spend the quota that expires first", got.ID)
	}
}

// An unknown reset (0) must sort last: a bucket with a known reset is spent
// before one whose reset we cannot observe, even at equal priority. The
// existing tie-break test never exercises this because both its accounts have
// known resets.
func TestSelectPrefersKnownResetOverUnknown(t *testing.T) {
	m := mgr(t, acct("known", 0), acct("unknown", 0))
	m.UpdateQuota("known", []provider.QuotaBucket{
		{Name: "7d", Utilization: 0.5, ResetsAt: time.Now().Add(2 * time.Hour).UnixMilli()},
	})
	// "unknown" carries no bucket at all, so soonestReset reports 0.

	got, err := m.Select(SelectRequest{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "known" {
		t.Errorf("selected %q, want known — an unknown reset must sort last", got.ID)
	}
}

// A paused account stays selectable (a hinted throttle should queue requests
// on the same warm account rather than push the burst elsewhere) but must not
// be preferred over a healthy one.
func TestSelectDeprioritizesPausedAccountWithoutExcludingIt(t *testing.T) {
	m := mgr(t, acct("better", 0), acct("worse", 5))
	m.PauseAccount("better", time.Hour)

	got, err := m.Select(SelectRequest{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "worse" {
		t.Errorf("selected %q, want worse — a paused account must not outrank a healthy one", got.ID)
	}

	// With nothing healthier available, the paused account is still returned
	// rather than refused outright.
	m.PauseAccount("worse", time.Hour)
	got, err = m.Select(SelectRequest{})
	if err != nil {
		t.Fatalf("Select with both paused: %v", err)
	}
	if got.ID != "better" {
		t.Errorf("selected %q, want better — priority still breaks the tie among paused accounts", got.ID)
	}
}

func TestSelectHonoursSessionAffinityThenYields(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 5))
	m.RecordSession("sess-1", "b")

	got, _ := m.Select(SelectRequest{SessionID: "sess-1"})
	if got.ID != "b" {
		t.Errorf("selected %q, want the pinned account b even at worse priority", got.ID)
	}

	// When the pinned account becomes ineligible, affinity yields.
	m.MarkRateLimited("b", time.Hour)
	got, _ = m.Select(SelectRequest{SessionID: "sess-1"})
	if got.ID != "a" {
		t.Errorf("selected %q, want a once b is ineligible", got.ID)
	}
}

// A bucket with no identifiable model token must fail closed: it binds every
// model rather than being silently ignored for all of them. modelBucketName
// (internal/provider/anthropic) guarantees such a bucket is never named with
// an unresolved "_<token>" suffix; this exercises the consequence end to end.
func TestSelectUnscopedRejectedBucketBlocksEveryModel(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.UpdateQuota("a", []provider.QuotaBucket{
		{Name: "7d", Utilization: 1, Status: "rejected"},
	})

	for _, model := range []string{"claude-sonnet-5", "claude-fable-5", ""} {
		got, err := m.Select(SelectRequest{Model: model})
		if err != nil {
			t.Fatalf("Select(%q): %v", model, err)
		}
		if got.ID != "b" {
			t.Errorf("Select(%q) = %q, want b — an unscoped rejected bucket must block every model", model, got.ID)
		}
	}
}

// Select must never hand out a live *Account: a caller reading the result
// would then race EnsureFresh writing a.Credential under the lock, and
// Credential is three plain strings — a torn read produces a garbage token,
// not merely a stale one. This is the gate for that: Select runs in a loop
// concurrently with a forced EnsureFresh on the same account, reading the
// returned Account's Credential every time. It must pass under -race, and it
// must do so because Select/Get/All return value copies, not because nothing
// happened to race.
func TestSelectDoesNotRaceWithEnsureFresh(t *testing.T) {
	m := mgr(t, acct("a", 0))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.EnsureFresh(context.Background(), "a", true)
		}
	}()

	for i := 0; i < 500; i++ {
		got, err := m.Select(SelectRequest{})
		if err != nil {
			continue // may be transiently ineligible; not what this test checks
		}
		// Read every field a consumer would read; under -race a torn write
		// would be caught here if this were still a live pointer.
		_ = got.Credential.AccessToken
		_ = got.Credential.RefreshToken
		_ = got.Credential.ExpiresAt
	}
	close(stop)
	wg.Wait()
}

func TestBucketAppliesTo(t *testing.T) {
	cases := []struct {
		bucket, model string
		want          bool
	}{
		{"5h", "claude-sonnet-5", true},
		{"7d", "claude-sonnet-5", true},
		{"7d_fable", "claude-fable-5", true},
		{"7d_fable", "claude-sonnet-5", false},
		{"7d_sonnet", "claude-sonnet-5", true},
		{"7d_fable", "", true}, // unknown model: assume every bucket binds
	}
	for _, c := range cases {
		if got := BucketAppliesTo(c.bucket, c.model); got != c.want {
			t.Errorf("BucketAppliesTo(%q, %q) = %v, want %v", c.bucket, c.model, got, c.want)
		}
	}
}
