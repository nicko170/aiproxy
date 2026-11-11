package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// settingsRecordingSource records every UpdateSettings and answers Settings
// with the last one, over an embedded nil Source.
type settingsRecordingSource struct {
	view.Source
	pushed []view.Settings
}

func (s *settingsRecordingSource) UpdateSettings(_ context.Context, next view.Settings) (view.Applied, error) {
	s.pushed = append(s.pushed, next)
	return view.Applied{Live: []string{"routingRules"}}, nil
}

func (s *settingsRecordingSource) Settings(context.Context) (view.Settings, error) {
	return s.pushed[len(s.pushed)-1], nil
}

func (s *settingsRecordingSource) last(t *testing.T) []view.RoutingRule {
	t.Helper()
	if len(s.pushed) == 0 {
		t.Fatal("nothing was pushed")
	}
	return s.pushed[len(s.pushed)-1].RoutingRules
}

// openRules puts the model on Settings with rules seeded and the editor open.
func openRules(t *testing.T, rules ...view.RoutingRule) (Model, *settingsRecordingSource) {
	t.Helper()
	m := fixtureModel(120, 28)
	rec := &settingsRecordingSource{}
	m.src = rec
	m.screen = screenSettings
	m.settings.loaded = true
	if rules == nil {
		rules = []view.RoutingRule{}
	}
	m.settings.current.RoutingRules = rules
	for i, f := range settingFields() {
		if f.name == "routingRules" {
			m.settings.sel = i
		}
	}
	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.settings.rules == nil {
		t.Fatal("enter on routingRules did not open the editor")
	}
	return m, rec
}

// press sends keys and runs any command they return, feeding its message back
// so the model sees the seam's answer.
func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		next, cmd := m.handleKey(msg)
		m = next.(Model)
		if cmd != nil {
			if out := cmd(); out != nil {
				if am, ok := out.(appliedMsg); ok {
					m, _ = m.updateSettings(am)
				}
			}
		}
	}
	return m
}

func typeInto(t *testing.T, m Model, text string) Model {
	t.Helper()
	m.settings.rules.input.SetValue(text)
	return m
}

func TestRulesEditorAddsARule(t *testing.T) {
	m, rec := openRules(t)
	m = press(t, m, "a")
	m = typeInto(t, m, "Primary working directory: /Users/n/code/GT")
	m = press(t, m, "enter")
	m = typeInto(t, m, "gt")
	m = press(t, m, "enter")

	want := []view.RoutingRule{{Match: "Primary working directory: /Users/n/code/GT", Group: "gt"}}
	if got := rec.last(t); len(got) != 1 || got[0] != want[0] {
		t.Errorf("pushed %+v, want %+v", got, want)
	}
}

func TestRulesEditorEditsDeletesTogglesAndReorders(t *testing.T) {
	m, rec := openRules(t, view.RoutingRule{Match: "A", Group: "ga"}, view.RoutingRule{Match: "B", Group: "gb"})

	m = press(t, m, "e")
	m = typeInto(t, m, "A2")
	m = press(t, m, "enter", "enter") // keep the group
	if got := rec.last(t); got[0].Match != "A2" || got[0].Group != "ga" {
		t.Fatalf("edit pushed %+v", got)
	}

	m = press(t, m, "s")
	if got := rec.last(t); !got[0].Strict {
		t.Fatalf("s did not make rule 0 strict: %+v", got)
	}

	m = press(t, m, "J")
	if got := rec.last(t); got[0].Match != "B" || got[1].Match != "A2" {
		t.Fatalf("J did not move rule 0 down: %+v", got)
	}
	if m.settings.rules.sel != 1 {
		t.Errorf("selection did not follow the moved rule: sel %d", m.settings.rules.sel)
	}

	m = press(t, m, "K")
	if got := rec.last(t); got[0].Match != "A2" {
		t.Fatalf("K did not move it back up: %+v", got)
	}

	m = press(t, m, "d", "y")
	if got := rec.last(t); len(got) != 1 || got[0].Match != "B" {
		t.Errorf("d y did not delete the selected rule: %+v", got)
	}
}

func TestRulesEditorDeleteNeedsConfirmation(t *testing.T) {
	m, rec := openRules(t, view.RoutingRule{Match: "A", Group: "ga"})
	press(t, m, "d", "n")
	if len(rec.pushed) != 0 {
		t.Errorf("d then n pushed %+v; any key but y must keep the rule", rec.pushed)
	}
}

func TestRulesEditorDeletingTheLastRuleSendsAnEmptyList(t *testing.T) {
	m, rec := openRules(t, view.RoutingRule{Match: "A", Group: "ga"})
	press(t, m, "d", "y")
	got := rec.last(t)
	if got == nil || len(got) != 0 {
		t.Errorf("pushed %#v, want a non-nil empty list (explicit clear, not absent)", got)
	}
}

func TestRulesEditorFlagsAGroupWithNoAccounts(t *testing.T) {
	m, _ := openRules(t, view.RoutingRule{Match: "A", Group: "ghost"})
	m.accounts[0].Group = "gt"
	out := m.viewSettings(28)
	if !strings.Contains(out, "no accounts in this group") {
		t.Errorf("a rule pointing at an empty group is not flagged:\n%s", out)
	}
	if !strings.Contains(out, "gt") {
		t.Errorf("the editor should name the groups that exist:\n%s", out)
	}
}

func TestRulesEditorEscCloses(t *testing.T) {
	m, _ := openRules(t)
	m = press(t, m, "esc")
	if m.settings.rules != nil {
		t.Error("esc did not close the editor")
	}
	if m.screen != screenSettings {
		t.Error("closing the editor must stay on settings")
	}
}

func TestRulesEditorOwnsTheKeyboard(t *testing.T) {
	m, _ := openRules(t)
	m = press(t, m, "2")
	if m.screen != screenSettings || m.settings.rules == nil {
		t.Error("a global key escaped the open rules editor")
	}
}
