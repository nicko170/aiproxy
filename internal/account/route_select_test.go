package account

import (
	"errors"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
)

func grouped(id string, priority int, group string) config.Account {
	a := acct(id, priority)
	a.Group = group
	return a
}

// a is personal and ranks first by priority; g is the gt group's account.
func groupedMgr(t *testing.T) *Manager {
	t.Helper()
	return mgr(t, grouped("a", 0, "personal"), grouped("g", 5, "gt"))
}

func mustSelect(t *testing.T, m *Manager, req SelectRequest) string {
	t.Helper()
	got, err := m.Select(req)
	if err != nil {
		t.Fatalf("Select(%+v): %v", req, err)
	}
	return got.ID
}

func TestSelectPrefersTheRequestedGroupOverPriority(t *testing.T) {
	m := groupedMgr(t)
	if got := mustSelect(t, m, SelectRequest{Group: "gt"}); got != "g" {
		t.Errorf("gt work went to %q, want g despite its worse priority", got)
	}
	if got := mustSelect(t, m, SelectRequest{Group: "personal"}); got != "a" {
		t.Errorf("personal work went to %q, want a", got)
	}
}

func TestSelectSpillsWhenTheGroupIsSpent(t *testing.T) {
	m := groupedMgr(t)
	m.MarkRateLimited("g", time.Hour)
	if got := mustSelect(t, m, SelectRequest{Group: "gt"}); got != "a" {
		t.Errorf("spent gt work went to %q, want it to spill onto a", got)
	}
}

func TestSelectStrictRefusesToSpill(t *testing.T) {
	m := groupedMgr(t)
	m.MarkRateLimited("g", time.Hour)
	if _, err := m.Select(SelectRequest{Group: "gt", Strict: true}); !errors.Is(err, ErrNoAccount) {
		t.Errorf("strict gt work with gt spent: err = %v, want ErrNoAccount", err)
	}
}

func TestSelectSpillsWhenTheGroupHasNoAccounts(t *testing.T) {
	m := groupedMgr(t)
	if got := mustSelect(t, m, SelectRequest{Group: "ghost"}); got != "a" {
		t.Errorf("work for an empty group went to %q, want the best of all (a)", got)
	}
}

func TestSelectAffinityYieldsToTheRecoveredGroup(t *testing.T) {
	m := groupedMgr(t)
	m.RecordSession("s", "a")
	if got := mustSelect(t, m, SelectRequest{SessionID: "s", Group: "gt"}); got != "g" {
		t.Errorf("selected %q, want g: affinity to a spilled account must yield once gt can serve", got)
	}
}

func TestSelectAffinityHoldsWhileTheGroupIsSpent(t *testing.T) {
	m := mgr(t, grouped("a", 0, "personal"), grouped("b", 1, "personal"), grouped("g", 5, "gt"))
	m.RecordSession("s", "b")
	m.MarkRateLimited("g", time.Hour)
	if got := mustSelect(t, m, SelectRequest{SessionID: "s", Group: "gt"}); got != "b" {
		t.Errorf("selected %q, want b: with gt spent the spilled session keeps its warm account", got)
	}
}

func TestSelectStrictIgnoresAffinityOutsideTheGroup(t *testing.T) {
	m := mgr(t, grouped("a", 0, "personal"), grouped("g1", 5, "gt"), grouped("g2", 6, "gt"))
	m.RecordSession("s", "a")
	if got := mustSelect(t, m, SelectRequest{SessionID: "s", Group: "gt", Strict: true}); got != "g1" {
		t.Errorf("selected %q, want g1", got)
	}
}

func TestSelectPinBeatsANonStrictGroup(t *testing.T) {
	m := groupedMgr(t)
	if err := m.Pin("a"); err != nil {
		t.Fatal(err)
	}
	if got := mustSelect(t, m, SelectRequest{Group: "gt"}); got != "a" {
		t.Errorf("selected %q, want the pinned a", got)
	}
}

func TestSelectStrictIgnoresAPinOutsideTheGroup(t *testing.T) {
	m := groupedMgr(t)
	if err := m.Pin("a"); err != nil {
		t.Fatal(err)
	}
	if got := mustSelect(t, m, SelectRequest{Group: "gt", Strict: true}); got != "g" {
		t.Errorf("strict gt work went to %q, want g: a strict rule beats the pin", got)
	}
	if m.Pinned() != "a" {
		t.Error("strict work must skip the pin, not clear it")
	}
	if got := mustSelect(t, m, SelectRequest{}); got != "a" {
		t.Errorf("unrouted work went to %q, want the still-pinned a", got)
	}
}

func TestSelectPausedGroupAccountStillBeatsSpilling(t *testing.T) {
	m := groupedMgr(t)
	m.PauseAccount("g", time.Hour)
	if got := mustSelect(t, m, SelectRequest{Group: "gt"}); got != "g" {
		t.Errorf("selected %q, want the paused g: a paused group account queues rather than spills", got)
	}
}
