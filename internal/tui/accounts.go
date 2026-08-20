package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

type acctsState struct {
	sel        int
	detail     bool
	confirming bool // remove asks first
	importing  bool // import source menu open

	history    []view.QuotaPoint
	historyFor string
	historyErr string
}

type historyMsg struct {
	accountID string
	points    []view.QuotaPoint
	err       error
}

func (m Model) updateAccounts(msg tea.Msg) (Model, tea.Cmd) {
	h, ok := msg.(historyMsg)
	if !ok {
		return m, nil
	}
	if h.accountID != m.accts.historyFor {
		return m, nil // selection moved on; a vanished account lands here too
	}
	if h.err != nil {
		m.accts.historyErr = h.err.Error()
		return m, nil
	}
	m.accts.historyErr = ""
	m.accts.history = h.points
	return m, nil
}

func (m Model) selectedAccount() (view.Account, bool) {
	if m.accts.sel >= 0 && m.accts.sel < len(m.accounts) {
		return m.accounts[m.accts.sel], true
	}
	return view.Account{}, false
}

func (m Model) fetchHistory(accountID string) tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		now := time.Now()
		pts, err := src.AccountQuotaHistory(ctx, accountID, view.Window{
			From: now.Add(-24 * time.Hour).UnixMilli(), To: now.UnixMilli(),
		})
		return historyMsg{accountID: accountID, points: pts, err: err}
	}
}

// action wraps one Source mutation: did is the past-tense success note, fail
// the verb for the error line — the same word the key hint used.
func (m Model) action(did, fail string, f func(context.Context) error) tea.Cmd {
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		return actionMsg{did: did, fail: fail, err: f(ctx)}
	}
}

func (m Model) accountsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := &m.accts
	key := msg.String()

	if s.confirming {
		a, ok := m.selectedAccount()
		switch key {
		case "y":
			s.confirming = false
			if !ok {
				return m, nil
			}
			return m, m.action("removed "+a.Label, "remove", func(ctx context.Context) error {
				return m.src.RemoveAccount(ctx, a.ID)
			})
		default: // any other key keeps the account
			s.confirming = false
		}
		return m, nil
	}

	if s.importing {
		var source view.ImportSource
		switch key {
		case "c":
			source = view.ImportSourceClaudeCode
		case "x":
			source = view.ImportSourceCodex
		default:
			s.importing = false
			return m, nil
		}
		s.importing = false
		src := m.src
		parent := m.ctx
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(parent, fetchTimeout)
			defer cancel()
			added, err := src.ImportCredentials(ctx, source)
			if err != nil {
				return actionMsg{fail: "import", err: err}
			}
			if added == 0 {
				return actionMsg{did: "nothing new to import"}
			}
			return actionMsg{did: fmt.Sprintf("imported %d account(s)", added)}
		}
	}

	switch key {
	case "j", "down":
		if s.sel < len(m.accounts)-1 {
			s.sel++
			return m.reloadDetail()
		}
	case "k", "up":
		if s.sel > 0 {
			s.sel--
			return m.reloadDetail()
		}
	case "enter":
		s.detail = !s.detail
		if s.detail {
			return m.reloadDetail()
		}
	case "e":
		if a, ok := m.selectedAccount(); ok {
			did, fail := "enabled "+a.Label, "enable"
			if !a.Disabled {
				did, fail = "disabled "+a.Label, "disable"
			}
			enable := a.Disabled
			return m, m.action(did, fail, func(ctx context.Context) error {
				return m.src.SetAccountEnabled(ctx, a.ID, enable)
			})
		}
	case "+", "=":
		if a, ok := m.selectedAccount(); ok && a.Priority > 0 {
			p := a.Priority - 1
			return m, m.action(fmt.Sprintf("%s now p%d", a.Label, p), "reprioritise", func(ctx context.Context) error {
				return m.src.SetPriority(ctx, a.ID, p)
			})
		}
	case "-", "_":
		if a, ok := m.selectedAccount(); ok {
			p := a.Priority + 1
			return m, m.action(fmt.Sprintf("%s now p%d", a.Label, p), "reprioritise", func(ctx context.Context) error {
				return m.src.SetPriority(ctx, a.ID, p)
			})
		}
	case "x":
		if _, ok := m.selectedAccount(); ok {
			s.confirming = true
		}
	case "i":
		s.importing = true
	}
	return m, nil
}

func (m Model) reloadDetail() (tea.Model, tea.Cmd) {
	if !m.accts.detail {
		return m, nil
	}
	a, ok := m.selectedAccount()
	if !ok {
		return m, nil
	}
	m.accts.historyFor = a.ID
	m.accts.history = nil
	m.accts.historyErr = ""
	return m, m.fetchHistory(a.ID)
}

func (m Model) accountsFooter() []string {
	if m.accts.confirming {
		return []string{"y remove", "any other key keeps it"}
	}
	if m.accts.importing {
		return []string{"c from Claude Code", "x from Codex", "esc cancel"}
	}
	// Ordered so the footer sheds from the right when narrow: navigation
	// hints go before the mutations do.
	return []string{"l login", "i import", "e enable/disable", "+/- priority", "x remove", "enter detail", "j/k select"}
}

func (m Model) viewAccounts(h int) string {
	th := m.th
	if len(m.accounts) == 0 {
		body := strings.Join([]string{
			th.bold("no accounts yet"),
			"",
			"  " + th.accent("l") + "  log in — Anthropic or ChatGPT",
			"  " + th.accent("i") + "  import from Claude Code or Codex",
		}, "\n")
		if m.accts.importing {
			body += "\n\n" + th.dim("import from:  ") + th.accent("c") + " Claude Code  " + th.accent("x") + " Codex  " + th.dim("esc cancel")
		}
		return overlay(body, m.width, h)
	}

	if m.accts.sel >= len(m.accounts) {
		m.accts.sel = len(m.accounts) - 1
	}
	nowMS := m.now.UnixMilli()

	var b strings.Builder
	head := "   " + th.dim(padRight("pri", 5)+padRight("account", 30)+padRight("state", 24)+padRight("5h", 6)+padRight("7d", 6)+"in flight")
	b.WriteString(head + "\n")
	for i, a := range m.accounts {
		cursor := "  "
		line := fmt.Sprintf("%s%s%s",
			padRight(fmt.Sprintf("p%d", a.Priority), 5),
			padRight(truncate(a.Label, 28), 30),
			"")
		state, sev := accountState(a, nowMS)
		util := func(name string) string {
			u, ok := a.Buckets[name]
			if !ok {
				return padRight("—", 6)
			}
			return padRight(fmt.Sprintf("%d%%", int(u*100+0.5)), 6)
		}
		row := " " + cursor + th.identity(a.ID, line) +
			th.sev(sev, padRight(truncate(state, 22), 24)) +
			util("five_hour") + util("seven_day") +
			fmt.Sprintf("%d", a.InFlight)
		if i == m.accts.sel {
			row = " " + th.accent("▸ ") + th.bold(th.identity(a.ID, line)) +
				th.sev(sev, padRight(truncate(state, 22), 24)) +
				util("five_hour") + util("seven_day") +
				fmt.Sprintf("%d", a.InFlight)
		}
		b.WriteString(row + "\n")
	}

	if m.accts.confirming {
		if a, ok := m.selectedAccount(); ok {
			b.WriteString("\n  " + th.bad("remove "+a.Label+"?") + th.dim("  its history stays in the ledger; its credential is deleted. ") +
				th.accent("y") + th.dim(" remove · any other key keeps it") + "\n")
		}
	} else if m.accts.importing {
		b.WriteString("\n  " + th.dim("import from:  ") + th.accent("c") + " Claude Code  " + th.accent("x") + " Codex  " + th.dim("esc cancel") + "\n")
	}

	if m.accts.detail {
		if a, ok := m.selectedAccount(); ok {
			b.WriteString("\n" + m.viewAccountDetail(a))
		}
	}
	return b.String()
}

func (m Model) viewAccountDetail(a view.Account) string {
	th := m.th
	var b strings.Builder
	b.WriteString("  " + th.dim(strings.Repeat("─", min(m.width-4, 60))) + "\n")
	b.WriteString("  " + th.identity(a.ID, th.bold(a.Label)) + th.dim("  ·  "+a.Provider+"  ·  id "+a.ID) + "\n")
	if a.LastError != "" {
		b.WriteString("  " + th.bad("last error: "+truncate(a.LastError, m.width-16)) + "\n")
	}

	if ps, ok := m.status.Probe.Accounts[a.ID]; ok {
		var parts []string
		if ps.LastSuccessAt > 0 {
			parts = append(parts, "probed "+formatCountdown(m.now.UnixMilli()-ps.LastSuccessAt)+" ago")
		} else {
			parts = append(parts, "never probed")
		}
		if ps.NextAttemptAt > m.now.UnixMilli() {
			parts = append(parts, "backing off · next try in "+formatCountdown(ps.NextAttemptAt-m.now.UnixMilli()))
		}
		line := th.dim(strings.Join(parts, " · "))
		if ps.LastError != "" {
			line += th.dim(" · ") + th.warn(truncate(ps.LastError, m.width/2))
		}
		b.WriteString("  " + line + "\n")
	}

	if m.accts.historyErr != "" {
		b.WriteString("  " + th.bad("quota history failed: "+truncate(m.accts.historyErr, m.width-24)) + "\n")
		return b.String()
	}
	if len(m.accts.history) == 0 {
		b.WriteString("  " + th.dim("no quota history yet — press p to probe now") + "\n")
		return b.String()
	}

	// One utilization sparkline per bucket over the last 24h: the tide table
	// of how this account's quota moved.
	byBucket := map[string][]sparkSample{}
	lastU := map[string]float64{}
	for _, p := range m.accts.history {
		byBucket[p.Bucket] = append(byBucket[p.Bucket], sparkSample{At: p.At, V: p.Utilization})
		lastU[p.Bucket] = p.Utilization
	}
	from, to := m.now.Add(-24*time.Hour).UnixMilli(), m.now.UnixMilli()
	names := make(map[string]float64, len(byBucket))
	for k := range byBucket {
		names[k] = 0
	}
	for _, name := range sortedBucketNames(names) {
		cells := sparkMax(byBucket[name], from, to, 48)
		b.WriteString("  " + th.dim(padRight(bucketLabel(name), 10)) +
			sparkline(cells) +
			th.dim(fmt.Sprintf("  now %d%% · last 24h", int(lastU[name]*100+0.5))) + "\n")
	}
	return b.String()
}

// sparkMax buckets samples like sparkBuckets but keeps each cell's maximum —
// right for a level (utilization) rather than a count.
func sparkMax(samples []sparkSample, from, to int64, cells int) []float64 {
	if cells <= 0 {
		return nil
	}
	out := make([]float64, cells)
	span := to - from
	if span <= 0 {
		return out
	}
	for _, s := range samples {
		if s.At < from || s.At >= to {
			continue
		}
		i := int((s.At - from) * int64(cells) / span)
		if i >= cells {
			i = cells - 1
		}
		if s.V > out[i] {
			out[i] = s.V
		}
	}
	return out
}
