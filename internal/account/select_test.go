package account

import (
	"errors"
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
	m.Get("a").Disabled = true
	m.Get("b").Status = StatusErrored

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
