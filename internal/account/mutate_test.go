package account

import (
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestSetEnabledTogglesDisabledFlag(t *testing.T) {
	m := mgr(t, acct("a", 0))

	if err := m.SetEnabled("a", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, ok := m.Get("a")
	if !ok || !got.Disabled {
		t.Fatalf("got = %+v, ok=%v; want disabled", got, ok)
	}

	if err := m.SetEnabled("a", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, _ = m.Get("a")
	if got.Disabled {
		t.Error("account should be enabled again")
	}
}

func TestSetEnabledUnknownAccountReturnsError(t *testing.T) {
	m := mgr(t, acct("a", 0))
	if err := m.SetEnabled("does-not-exist", false); err == nil {
		t.Error("want an error for an unknown account id")
	}
}

func TestSetPriorityUpdatesSelectionOrder(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))

	if err := m.SetPriority("b", -5); err != nil {
		t.Fatalf("SetPriority: %v", err)
	}
	got, err := m.Select(SelectRequest{Model: "claude-sonnet"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("selected %q, want b (now the lowest priority)", got.ID)
	}
}

func TestSetPriorityUnknownAccountReturnsError(t *testing.T) {
	m := mgr(t, acct("a", 0))
	if err := m.SetPriority("does-not-exist", 1); err == nil {
		t.Error("want an error for an unknown account id")
	}
}

func TestRemoveDropsAccountFromSelectionAndLookup(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))

	if err := m.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := m.Get("a"); ok {
		t.Error("removed account should no longer be gettable")
	}
	all := m.All()
	if len(all) != 1 || all[0].ID != "b" {
		t.Errorf("All() = %+v, want only b", all)
	}
}

func TestRemoveUnknownAccountReturnsError(t *testing.T) {
	m := mgr(t, acct("a", 0))
	if err := m.Remove("does-not-exist"); err == nil {
		t.Error("want an error for an unknown account id")
	}
}

// Removing an account must also drop any session affinity pinned to it, or a
// later request for that session id silently falls through to a dead account
// id that Select can never find.
func TestRemoveDropsSessionAffinityEntriesForThatAccount(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.RecordSession("sess-1", "a")

	if err := m.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	m.mu.Lock()
	_, stillPinned := m.affinity["sess-1"]
	m.mu.Unlock()
	if stillPinned {
		t.Error("affinity entry for the removed account should have been dropped")
	}

	// The session must now be free to pin to whatever Select picks, not stuck
	// looking up a dead account id.
	got, err := m.Select(SelectRequest{Model: "claude-sonnet", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "b" {
		t.Errorf("selected %q, want b", got.ID)
	}
}

func TestSetSwitchThresholdTakesEffectImmediately(t *testing.T) {
	m := mgr(t, acct("a", 0))
	m.mu.Lock()
	m.byID["a"].Buckets["5h"] = provider.QuotaBucket{Name: "5h", Utilization: 0.5}
	m.mu.Unlock()

	// Under the default 0.98 threshold, 0.5 utilization is still eligible.
	if _, err := m.Select(SelectRequest{Model: "claude-sonnet"}); err != nil {
		t.Fatalf("Select before lowering threshold: %v", err)
	}

	m.SetSwitchThreshold(0.4)

	if _, err := m.Select(SelectRequest{Model: "claude-sonnet"}); err != ErrNoAccount {
		t.Errorf("Select after lowering threshold = %v, want ErrNoAccount", err)
	}
}

func TestSetSessionAffinityTakesEffectImmediately(t *testing.T) {
	m := mgr(t, acct("a", 0), acct("b", 1))
	m.RecordSession("sess-1", "b")

	// Affinity starts enabled: the pinned (higher-priority-number) account wins
	// over the lower-priority one for this session.
	got, err := m.Select(SelectRequest{Model: "claude-sonnet", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("selected %q, want b (pinned by affinity)", got.ID)
	}

	m.SetSessionAffinity(false)

	got, err = m.Select(SelectRequest{Model: "claude-sonnet", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.ID != "a" {
		t.Errorf("selected %q, want a (lowest priority; affinity is disabled)", got.ID)
	}
}
