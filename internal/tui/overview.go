package tui

import (
	"context"

	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/nicko170/aiproxy/internal/view"
)

// sparkCells is the sparkline's width: one hour at one cell per minute.
const sparkCells = 60

// showSparklines is the first thing a narrowing terminal gives up (spec §8's
// drop order: sparklines, then columns).
func showSparklines(width int) bool { return width >= 80 }

// gaugeWidth is how wide the quota channel is at a given terminal width:
// everything after the bucket label column, capped so a very wide terminal
// gets margins rather than a fifty-centimetre gauge.
func gaugeWidth(width int) int {
	w := width - overviewLabelCol - 4
	if w > 64 {
		w = 64
	}
	if w < 12 {
		w = 12
	}
	return w
}

const overviewLabelCol = 12

func (m Model) viewOverview(h int) string {
	if len(m.accounts) == 0 {
		return m.viewNoAccounts(h)
	}
	th := m.th
	nowMS := m.now.UnixMilli()
	var b strings.Builder

	for i, a := range m.accounts {
		if i > 0 {
			b.WriteString("\n")
		}
		// Account line: identity-coloured label, dim particulars, state word.
		// Narrowing drops the particulars first, then shortens the label —
		// the state word is the thing a glance is for and always survives.
		state, stateSev := accountState(a, nowMS)
		right := th.sev(stateSev, state)
		if a.InFlight > 0 {
			right += th.dim(" · ") + fmt.Sprintf("%d in flight", a.InFlight)
		}
		partic := fmt.Sprintf("   p%d · %s", a.Priority, a.Provider)
		if m.width < 90 {
			partic = ""
		}
		// The cursor and the forced marker are separate things and must look
		// separate: one is where you are pointing, the other is what is
		// actually overriding the router right now.
		cursor := "  "
		if i == m.overview.sel {
			cursor = th.accent("▸ ")
		}
		forced := ""
		if m.status.PinnedAccountID == a.ID {
			forced = th.warn(" ⇥ forced")
		}
		labelMax := m.width - lipgloss.Width(right) - len(partic) - lipgloss.Width(forced) - 4
		if labelMax < 12 {
			labelMax = 12
		}
		left := cursor + th.identity(a.ID, th.bold(truncate(a.Label, labelMax))) + th.dim(partic) + forced
		b.WriteString(spread(left, right, m.width) + "\n")

		if a.LastError != "" && a.Status != "active" {
			b.WriteString("  " + th.bad(truncate(a.LastError, m.width-4)) + "\n")
		}

		gw := gaugeWidth(m.width)
		for _, name := range sortedBucketNames(a.Buckets) {
			u := a.Buckets[name]
			text := fmt.Sprintf(" %d%%", int(u*100+0.5))
			if r := m.resets[a.ID][name]; r > 0 {
				text += fmt.Sprintf(" · resets %s in %s",
					formatClock(r, m.loc), formatCountdown(r-nowMS))
			}
			b.WriteString("  " + th.dim(padRight(bucketLabel(name), overviewLabelCol-2)) +
				m.th.gauge(gw, u, m.settings.current.SwitchThreshold, text) + "\n")
		}
		if len(a.Buckets) == 0 {
			// Distinguish "never read" from "read, and upstream named no
			// window". Telling someone to press p when a probe has already
			// succeeded sends them to do the one thing that will not help.
			msg := "no quota reading yet — press p to probe now"
			if ps, ok := m.status.Probe.Accounts[a.ID]; ok && ps.LastSuccessAt > 0 {
				msg = "probed, but no limit window reported yet"
			}
			b.WriteString("  " + th.dim(msg) + "\n")
		}

		if showSparklines(m.width) {
			cells := m.sparks[a.ID]
			if cells == nil {
				cells = make([]float64, sparkCells)
			}
			var total float64
			for _, v := range cells {
				total += v
			}
			b.WriteString("  " + th.dim(padRight("", overviewLabelCol-2)) +
				th.identity(a.ID, sparkline(cells)) +
				th.dim(fmt.Sprintf("  %d req · last hour", int(total))) + "\n")
		}
	}

	if ps := m.probeSummary(); ps != "" {
		b.WriteString("\n" + "  " + ps + "\n")
	}
	return b.String()
}

// probeSummary is one dim line about the quota prober — when it last
// completed, or what is holding it back.
func (m Model) probeSummary() string {
	th := m.th
	p := m.status.Probe
	var errs int
	for _, a := range p.Accounts {
		if a.LastError != "" {
			errs++
		}
	}
	var s string
	switch {
	case p.LastCompletedAt > 0:
		s = th.dim("quota probed " + formatCountdown(m.now.UnixMilli()-p.LastCompletedAt) + " ago")
	case p.Running:
		s = th.dim("quota probe running…")
	default:
		return ""
	}
	if errs > 0 {
		s += th.dim(" · ") + th.warn(plural(errs, "account")+" failing to probe")
	}
	return s
}

// accountState is the one word (plus countdown) that says whether this
// account can serve right now, and its severity.
func accountState(a view.Account, nowMS int64) (string, severity) {
	switch {
	case a.Disabled:
		return "disabled", sevWarn
	case a.Status != "active":
		return "errored", sevBad
	case a.RateLimitedUntil > nowMS:
		return "rate-limited · back in " + formatCountdown(a.RateLimitedUntil-nowMS), sevBad
	case a.PausedUntil > nowMS:
		return "paused " + formatCountdown(a.PausedUntil-nowMS), sevWarn
	default:
		return "ready", sevOK
	}
}

// viewNoAccounts is the first thing a new user sees: not an empty frame but
// the two actions that make the instrument read something.
func (m Model) viewNoAccounts(h int) string {
	th := m.th
	addr := displayAddr(m.status.ListenAddr)
	body := strings.Join([]string{
		th.bold("no accounts yet"),
		"",
		"aiproxy rotates requests across accounts as each",
		"one's quota burns down. Add the first account:",
		"",
		"  " + th.accent("l") + "  log in — Anthropic or ChatGPT",
		"  " + th.accent("4") + "  open accounts to import from Claude Code",
		"",
		th.dim("then export both before starting Claude Code:"),
		"  ANTHROPIC_BASE_URL=http://" + addr,
		"  " + claudeCodeFirstPartyEnv,
	}, "\n")
	return overlay(body, m.width, h)
}

// spread lays left and right at the edges of width, ANSI-aware. When the
// two collide the gap stays at one cell: a squeezed line overflows to the
// terminal's own truncation rather than reflowing into rubble.
func spread(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// overviewState is the forced-override cursor. It exists only on this screen:
// the Accounts screen has its own selection for a different purpose, and
// sharing one would make j/k mean two things depending on where you came from.
type overviewState struct {
	sel int // index into m.accounts
}

// overviewKey drives the forced-account override.
//
// Arrow keys move a cursor and f pins the account under it, so the whole
// interaction is "point at the one you want, say use that one". f on the
// account already pinned clears the override, which makes the same key the way
// out as well as the way in — there is no separate unpin to remember.
func (m Model) overviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := &m.overview
	switch msg.String() {
	case "j", "down":
		if s.sel < len(m.accounts)-1 {
			s.sel++
		}
	case "k", "up":
		if s.sel > 0 {
			s.sel--
		}
	case "f":
		if s.sel >= len(m.accounts) {
			return m, nil
		}
		a := m.accounts[s.sel]
		target := a.ID
		verb := "forced all traffic to " + a.Label
		if m.status.PinnedAccountID == a.ID {
			target = "" // f on the pinned account is how you release it
			verb = "released the forced account"
		}
		src := m.src
		parent := m.ctx
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(parent, fetchTimeout)
			defer cancel()
			if err := src.PinAccount(ctx, target); err != nil {
				return actionMsg{fail: "force", err: err}
			}
			return actionMsg{did: verb}
		}
	}
	return m, nil
}
