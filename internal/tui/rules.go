package tui

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// rulesEditor is the routing-rules modal on the Settings screen. Every change
// is pushed through UpdateSettings at once, like every other setting here:
// rules are live, so there is no separate save step to forget.
type rulesEditor struct {
	rules []view.RoutingRule
	sel   int

	// prompt is 0 when browsing, 1 while editing the match text, 2 while
	// editing the group. editing is the rule being edited, or -1 for a new one.
	prompt  int
	editing int
	draft   view.RoutingRule
	input   textinput.Model

	confirmDelete bool
}

func newRulesEditor(rules []view.RoutingRule) *rulesEditor {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 500
	return &rulesEditor{rules: slices.Clone(rules), input: ti, editing: -1}
}

func openRulesEditor(m *Model) tea.Cmd {
	m.settings.rules = newRulesEditor(m.settings.current.RoutingRules)
	m.settings.updateErr = ""
	return nil
}

// pushRules sends the editor's list as the whole rule set. Never nil: an
// empty list is how "delete the last rule" is said, and an absent field would
// mean "keep what is there".
func (m Model) pushRules() tea.Cmd {
	next := m.settings.current
	next.RoutingRules = append([]view.RoutingRule{}, m.settings.rules.rules...)
	return m.pushSettings(next)
}

func (r *rulesEditor) startPrompt(stage int, value string) {
	r.prompt = stage
	r.input.SetValue(value)
	r.input.CursorEnd()
	r.input.Focus()
}

func (m Model) rulesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	r := m.settings.rules
	key := msg.String()

	if r.prompt != 0 {
		switch key {
		case "esc":
			r.prompt = 0
			return m, nil
		case "enter":
			if r.prompt == 1 {
				r.draft.Match = r.input.Value()
				r.startPrompt(2, r.draft.Group)
				return m, nil
			}
			r.draft.Group = strings.TrimSpace(r.input.Value())
			r.prompt = 0
			if r.editing < 0 {
				r.rules = append(r.rules, r.draft)
				r.sel = len(r.rules) - 1
			} else {
				r.rules[r.editing] = r.draft
			}
			return m, m.pushRules()
		}
		var cmd tea.Cmd
		r.input, cmd = r.input.Update(msg)
		return m, cmd
	}

	if r.confirmDelete {
		r.confirmDelete = false
		if key != "y" || len(r.rules) == 0 {
			return m, nil
		}
		r.rules = slices.Delete(r.rules, r.sel, r.sel+1)
		if r.sel >= len(r.rules) && r.sel > 0 {
			r.sel--
		}
		return m, m.pushRules()
	}

	switch key {
	case "esc":
		m.settings.rules = nil
		return m, nil
	case "j", "down":
		if r.sel < len(r.rules)-1 {
			r.sel++
		}
	case "k", "up":
		if r.sel > 0 {
			r.sel--
		}
	case "a":
		r.editing, r.draft = -1, view.RoutingRule{}
		r.startPrompt(1, "")
	case "e", "enter":
		if len(r.rules) > 0 {
			r.editing, r.draft = r.sel, r.rules[r.sel]
			r.startPrompt(1, r.draft.Match)
		}
	case "s":
		if len(r.rules) > 0 {
			r.rules[r.sel].Strict = !r.rules[r.sel].Strict
			return m, m.pushRules()
		}
	case "J":
		if r.sel < len(r.rules)-1 {
			r.rules[r.sel], r.rules[r.sel+1] = r.rules[r.sel+1], r.rules[r.sel]
			r.sel++
			return m, m.pushRules()
		}
	case "K":
		if r.sel > 0 && r.sel < len(r.rules) {
			r.rules[r.sel], r.rules[r.sel-1] = r.rules[r.sel-1], r.rules[r.sel]
			r.sel--
			return m, m.pushRules()
		}
	case "d":
		if len(r.rules) > 0 {
			r.confirmDelete = true
		}
	}
	return m, nil
}

func (m Model) rulesFooter() []string {
	r := m.settings.rules
	switch {
	case r.prompt == 1:
		return []string{"enter next: group", "esc cancel"}
	case r.prompt == 2:
		return []string{"enter save", "esc cancel"}
	case r.confirmDelete:
		return []string{"y delete", "any other key keeps it"}
	}
	return []string{"a add", "e edit", "s strict", "J/K move", "d delete", "esc close"}
}

// accountGroups maps each group that has an account to how many it has.
func (m Model) accountGroups() map[string]int {
	out := map[string]int{}
	for _, a := range m.accounts {
		out[a.Group]++
	}
	return out
}

func (m Model) viewRules() string {
	th := m.th
	r := m.settings.rules
	groups := m.accountGroups()

	var b strings.Builder
	b.WriteString("  " + th.bold("routing rules") + th.dim("  ·  text in a request → preferred group, first match wins") + "\n\n")
	if len(r.rules) == 0 && r.prompt == 0 {
		b.WriteString("    " + th.dim("no rules — every request prefers the default group. a adds one.") + "\n")
	}
	for i, rule := range r.rules {
		cursor := "  "
		if i == r.sel {
			cursor = th.accent("▸ ")
		}
		group := rule.Group
		if group == "" {
			group = "(ungrouped)"
		}
		// Group first: a long match is what gets truncated on a narrow
		// terminal, never the group it routes to.
		if rule.Strict {
			group += " [strict]"
		}
		line := fmt.Sprintf("%d. %s  ← %q", i+1, group, rule.Match)
		row := "  " + cursor + truncate(line, m.width-8)
		if i == r.sel {
			row = "  " + cursor + th.bold(truncate(line, m.width-8))
		}
		b.WriteString(row + "\n")
		if groups[rule.Group] == 0 {
			b.WriteString("       " + th.warn("no accounts in this group") + "\n")
		}
	}

	switch r.prompt {
	case 1:
		b.WriteString("\n  " + th.dim("text to match:  ") + th.accent(r.input.View()) + "\n")
	case 2:
		b.WriteString("\n  " + th.dim("preferred group:  ") + th.accent(r.input.View()) + "\n")
	}
	if r.confirmDelete && r.sel < len(r.rules) {
		b.WriteString("\n  " + th.bad(fmt.Sprintf("delete rule %d?", r.sel+1)) + th.dim("  ") + th.accent("y") + th.dim(" delete · any other key keeps it") + "\n")
	}
	if m.settings.updateErr != "" {
		b.WriteString("\n  " + th.bad(truncate(m.settings.updateErr, m.width-6)) + "\n")
	}

	names := make([]string, 0, len(groups))
	for g, n := range groups {
		if g == "" {
			g = "(ungrouped)"
		}
		names = append(names, fmt.Sprintf("%s ×%d", g, n))
	}
	sort.Strings(names)
	b.WriteString("\n  " + th.dim("groups with accounts: "+strings.Join(names, ", ")) + "\n")
	return b.String()
}
