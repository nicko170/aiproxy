package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// groupRecordingSource records SetAccountGroup over an embedded nil Source,
// like loginRecordingSource.
type groupRecordingSource struct {
	view.Source
	id, group string
}

func (s *groupRecordingSource) SetAccountGroup(_ context.Context, id, group string) error {
	s.id, s.group = id, group
	return nil
}

func typeText(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}
	return m
}

func TestAccountsGKeyEditsTheGroup(t *testing.T) {
	m := fixtureModel(120, 28)
	rec := &groupRecordingSource{}
	m.src = rec
	m.screen = screenAccounts
	a, ok := m.selectedAccount()
	if !ok {
		t.Fatal("fixture has no selected account")
	}

	next, _ := m.handleKey(keyFor("g"))
	m = next.(Model)
	if !m.accts.grouping {
		t.Fatalf("g did not open the group prompt: %+v", m.accts)
	}
	m.accts.input.SetValue("")
	m = typeText(t, m, "gt")
	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.accts.grouping {
		t.Error("enter must close the prompt")
	}
	if cmd == nil {
		t.Fatal("enter must return the command that sets the group")
	}
	cmd()
	if rec.id != a.ID || rec.group != "gt" {
		t.Errorf("SetAccountGroup(%q, %q), want (%q, gt)", rec.id, rec.group, a.ID)
	}
}

func TestAccountsGroupPromptOwnsTheKeyboard(t *testing.T) {
	m := fixtureModel(120, 28)
	m.src = &groupRecordingSource{}
	m.screen = screenAccounts
	next, _ := m.handleKey(keyFor("g"))
	m = next.(Model)
	next, _ = m.handleKey(keyFor("2")) // a global screen key
	m = next.(Model)
	if m.screen != screenAccounts || !m.accts.grouping {
		t.Errorf("a global key escaped the open prompt: screen %v grouping %v", m.screen, m.accts.grouping)
	}
	next, _ = m.handleKey(keyFor("esc"))
	if next.(Model).accts.grouping {
		t.Error("esc must cancel the prompt")
	}
}

func TestAccountsShowTheGroup(t *testing.T) {
	m := fixtureModel(120, 28)
	m.accounts[0].Group = "greenthread"
	if out := m.viewAccounts(28); !strings.Contains(out, "greenthread") || !strings.Contains(out, "group") {
		t.Errorf("accounts view lacks the group column:\n%s", out)
	}
}

func TestActivityMarksSpilledRequests(t *testing.T) {
	m := fixtureModel(120, 28)
	label := strings.Repeat("x", 40)
	cell := accountCell(label, true, 18)
	if !strings.HasSuffix(cell, " ↪") {
		t.Errorf("spilled cell %q lacks the marker", cell)
	}
	if n := len([]rune(cell)); n > 18 {
		t.Errorf("spilled cell is %d wide, want ≤ 18 so the marker survives truncation", n)
	}
	if cell := accountCell("acct", false, 18); strings.Contains(cell, "↪") {
		t.Errorf("unspilled cell %q has the marker", cell)
	}
	_ = m
}

func TestSettingsDefaultRows(t *testing.T) {
	find := func(name string) settingField {
		for _, f := range settingFields() {
			if f.name == name {
				return f
			}
		}
		t.Fatalf("no %s row", name)
		return settingField{}
	}
	g, strict := "personal", false
	s := view.Settings{DefaultGroup: &g, DefaultStrict: &strict, RoutingRules: []view.RoutingRule{}}

	dg := find("defaultGroup")
	if dg.get(s) != "personal" {
		t.Errorf("defaultGroup get = %q", dg.get(s))
	}
	if err := dg.set(&s, " gt "); err != nil || *s.DefaultGroup != "gt" {
		t.Errorf("defaultGroup set: err %v value %q", err, *s.DefaultGroup)
	}

	ds := find("defaultStrict")
	if !ds.boolean || ds.get(s) != "off" {
		t.Errorf("defaultStrict boolean=%v get=%q", ds.boolean, ds.get(s))
	}
	if err := ds.set(&s, "on"); err != nil || !*s.DefaultStrict {
		t.Errorf("defaultStrict set: err %v value %v", err, *s.DefaultStrict)
	}

	rr := find("routingRules")
	if got := rr.get(s); got != "none" {
		t.Errorf("routingRules summary for [] = %q, want none", got)
	}
	s.RoutingRules = []view.RoutingRule{{Match: "a", Group: "x"}, {Match: "b", Group: "y"}}
	if got := rr.get(s); got != "2 rules" {
		t.Errorf("routingRules summary = %q, want 2 rules", got)
	}
}
