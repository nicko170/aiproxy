package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// settingField declares one editable row. name is the field's wire name —
// the same string UpdateSettings returns in Applied.Live/NeedsRestart, so
// the restart badge can never drift from what the seam reported.
type settingField struct {
	name    string
	desc    string
	unit    string
	boolean bool
	get     func(view.Settings) string
	set     func(*view.Settings, string) error
}

func setInt(dst *int) func(*view.Settings, string) error {
	return func(_ *view.Settings, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("not a whole number: %q", v)
		}
		*dst = n
		return nil
	}
}

// settingFields is the screen's row order: the routing knobs a person
// actually reaches for first, the plumbing timeouts after.
func settingFields() []settingField {
	return []settingField{
		{
			name: "switchThreshold", unit: "",
			desc: "rotate to the next account when a quota bucket reaches this fraction",
			get:  func(s view.Settings) string { return strconv.FormatFloat(s.SwitchThreshold, 'f', 2, 64) },
			set: func(s *view.Settings, v string) error {
				f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if err != nil {
					return fmt.Errorf("not a number: %q", v)
				}
				s.SwitchThreshold = f
				return nil
			},
		},
		{
			name: "sessionAffinity", boolean: true,
			desc: "keep a session on the account that served it, while that account can serve",
			get: func(s view.Settings) string {
				if s.SessionAffinity {
					return "on"
				}
				return "off"
			},
			set: func(s *view.Settings, v string) error {
				s.SessionAffinity = v == "on"
				return nil
			},
		},
		{
			name: "blockedModels",
			desc: "glob patterns of models this proxy refuses, comma-separated; empty allows all",
			get:  func(s view.Settings) string { return strings.Join(s.BlockedModels, ", ") },
			set: func(s *view.Settings, v string) error {
				var out []string
				for _, p := range strings.Split(v, ",") {
					if p = strings.TrimSpace(p); p != "" {
						out = append(out, p)
					}
				}
				s.BlockedModels = out
				return nil
			},
		},
		{
			name: "retryBudgetMs", unit: "ms",
			desc: "total time the proxy may spend retrying one request across accounts",
			get:  func(s view.Settings) string { return strconv.Itoa(s.RetryBudgetMS) },
			set:  func(s *view.Settings, v string) error { return setInt(&s.RetryBudgetMS)(s, v) },
		},
		{
			name: "inlineAbsorbMaxMs", unit: "ms",
			desc: "longest upstream-suggested wait absorbed silently instead of rotating",
			get:  func(s view.Settings) string { return strconv.Itoa(s.InlineAbsorbMaxMS) },
			set:  func(s *view.Settings, v string) error { return setInt(&s.InlineAbsorbMaxMS)(s, v) },
		},
		{
			name: "headerTimeoutMs", unit: "ms",
			desc: "how long to wait for upstream response headers before treating the attempt as failed",
			get:  func(s view.Settings) string { return strconv.Itoa(s.HeaderTimeoutMS) },
			set:  func(s *view.Settings, v string) error { return setInt(&s.HeaderTimeoutMS)(s, v) },
		},
		{
			name: "bodyIdleMs", unit: "ms",
			desc: "how long a streaming body may stall before the relay gives up on it",
			get:  func(s view.Settings) string { return strconv.Itoa(s.BodyIdleMS) },
			set:  func(s *view.Settings, v string) error { return setInt(&s.BodyIdleMS)(s, v) },
		},
		{
			name: "quotaProbeIntervalSeconds", unit: "s",
			desc: "how often the background probe re-reads each account's quota",
			get:  func(s view.Settings) string { return strconv.Itoa(s.QuotaProbeIntervalSeconds) },
			set:  func(s *view.Settings, v string) error { return setInt(&s.QuotaProbeIntervalSeconds)(s, v) },
		},
		{
			name: "metricsRetentionDays", unit: "d",
			desc: "how long per-request accounting rows are kept before pruning",
			get:  func(s view.Settings) string { return strconv.Itoa(s.MetricsRetentionDays) },
			set:  func(s *view.Settings, v string) error { return setInt(&s.MetricsRetentionDays)(s, v) },
		},
	}
}

type settingsState struct {
	current view.Settings
	loaded  bool
	loadErr string

	sel     int
	editing bool
	input   textinput.Model

	// updateErr is the seam's own refusal (validation), shown under the row
	// it refused.
	updateErr string

	// needsRestart marks fields UpdateSettings persisted but could not apply
	// to the running proxy. The badge stays until restart — the seam returns
	// this as data precisely so this screen cannot show a pending field as
	// live (view.Applied's doc comment).
	needsRestart map[string]bool

	// justApplied names fields confirmed live by the last update, cleared on
	// the next edit.
	justApplied map[string]bool
}

func newSettingsState() settingsState {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 200
	return settingsState{input: ti, needsRestart: map[string]bool{}, justApplied: map[string]bool{}}
}

type settingsLoadedMsg struct {
	s   view.Settings
	err error
}

type appliedMsg struct {
	s       view.Settings
	applied view.Applied
	err     error
}

func (m Model) enterSettings() tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		s, err := src.Settings(ctx)
		return settingsLoadedMsg{s: s, err: err}
	}
}

func (m Model) updateSettings(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case settingsLoadedMsg:
		if msg.err != nil {
			m.settings.loadErr = msg.err.Error()
			return m, nil
		}
		m.settings.loadErr = ""
		m.settings.current = msg.s
		m.settings.loaded = true
		return m, nil

	case appliedMsg:
		if msg.err != nil {
			m.settings.updateErr = msg.err.Error()
			return m, nil
		}
		m.settings.updateErr = ""
		m.settings.current = msg.s
		m.settings.justApplied = map[string]bool{}
		for _, f := range msg.applied.Live {
			m.settings.justApplied[f] = true
			delete(m.settings.needsRestart, f)
		}
		for _, f := range msg.applied.NeedsRestart {
			m.settings.needsRestart[f] = true
		}
		return m, nil
	}
	return m, nil
}

func (m Model) settingsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := &m.settings
	fields := settingFields()

	if s.editing {
		switch msg.String() {
		case "esc":
			s.editing = false
			s.updateErr = ""
			return m, nil
		case "enter":
			f := fields[s.sel]
			next := m.settings.current
			if err := f.set(&next, s.input.Value()); err != nil {
				s.updateErr = err.Error()
				return m, nil
			}
			s.editing = false
			return m, m.pushSettings(next)
		}
		var cmd tea.Cmd
		s.input, cmd = s.input.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "j", "down":
		if s.sel < len(fields)-1 {
			s.sel++
			s.updateErr = ""
		}
	case "k", "up":
		if s.sel > 0 {
			s.sel--
			s.updateErr = ""
		}
	case "enter", "e":
		f := fields[s.sel]
		if !s.loaded {
			return m, nil
		}
		if f.boolean {
			next := m.settings.current
			cur := f.get(next)
			val := "on"
			if cur == "on" {
				val = "off"
			}
			if err := f.set(&next, val); err != nil {
				s.updateErr = err.Error()
				return m, nil
			}
			return m, m.pushSettings(next)
		}
		s.editing = true
		s.justApplied = map[string]bool{}
		s.input.SetValue(f.get(s.current))
		s.input.CursorEnd()
		s.input.Focus()
	}
	return m, nil
}

// pushSettings writes the whole settings struct through the seam and reports
// what actually took effect.
func (m Model) pushSettings(next view.Settings) tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		applied, err := src.UpdateSettings(ctx, next)
		if err != nil {
			return appliedMsg{err: err}
		}
		// Re-read rather than trusting our own copy: the seam may normalise.
		s, err := src.Settings(ctx)
		if err != nil {
			return appliedMsg{err: err}
		}
		return appliedMsg{s: s, applied: applied}
	}
}

func (m Model) settingsFooter() []string {
	if m.settings.editing {
		return []string{"enter save", "esc cancel"}
	}
	return []string{"j/k select", "enter edit"}
}

func (m Model) viewSettings(h int) string {
	th := m.th
	s := m.settings
	if s.loadErr != "" {
		return "\n  " + th.bad("settings unavailable: "+truncate(s.loadErr, m.width-26))
	}
	if !s.loaded {
		return "\n  " + th.dim("loading settings…")
	}

	fields := settingFields()
	var b strings.Builder
	b.WriteString("  " + th.dim("changes are saved immediately; a value the running proxy has not adopted says so") + "\n\n")
	for i, f := range fields {
		cursor := "  "
		name := padRight(f.name, 28)
		val := f.get(s.current)
		if f.unit != "" {
			val += " " + f.unit
		}
		valCell := padRight(val, 22)

		var badge string
		switch {
		case s.needsRestart[f.name]:
			badge = th.warn("saved · restart to apply")
		case s.justApplied[f.name]:
			badge = th.ok("applied")
		}

		if i == s.sel {
			cursor = th.accent("▸ ")
			if s.editing {
				b.WriteString("  " + cursor + th.bold(name) + th.accent(padRight(s.input.View(), 22)) + badge + "\n")
			} else {
				b.WriteString("  " + cursor + th.bold(name) + valCell + badge + "\n")
			}
			if s.updateErr != "" {
				b.WriteString("      " + th.bad(truncate(s.updateErr, m.width-8)) + "\n")
			} else {
				b.WriteString("      " + th.dim(truncate(f.desc, m.width-8)) + "\n")
			}
			continue
		}
		b.WriteString("  " + cursor + name + th.dim(valCell) + badge + "\n")
	}
	return b.String()
}
