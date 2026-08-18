package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// activityRing bounds how many live events the feed keeps. The feed is a
// window, not an archive — Usage owns history — and a bounded ring means a
// paused or scrolled-back view costs a fixed amount of memory and can never
// push back on the proxy (the hub would drop for us anyway; consuming
// promptly keeps its drop counter at zero).
const activityRing = 1000

// logRingShown caps how many log lines the log view renders.

type activityState struct {
	channel <-chan view.Event
	events  []view.Event // append-only ring, newest last
	total   int          // events ever seen, for the "n new" note while paused

	paused   bool
	pausedAt int // total when pausing, to count what arrived since
	offset   int // scrollback distance from the tail, 0 = live tail

	showLog bool

	// Filters hold the selected value per dimension; "" means all. They
	// cycle through the values seen in the ring, so a filter can only name
	// something that exists.
	facct, fmodel, foutcome string
}

func (a *activityState) ch() <-chan view.Event { return a.channel }

func (a *activityState) append(ev view.Event) {
	a.total++
	if len(a.events) >= activityRing {
		copy(a.events, a.events[1:])
		a.events[len(a.events)-1] = ev
		if a.offset > 0 && a.offset < len(a.events) {
			a.offset++ // keep the scrolled-to spot stable as the ring slides
		}
		return
	}
	a.events = append(a.events, ev)
}

// filtered returns the events the current filters keep, oldest first.
func (a *activityState) filtered() []view.Event {
	if a.facct == "" && a.fmodel == "" && a.foutcome == "" {
		return a.events
	}
	out := make([]view.Event, 0, len(a.events))
	for _, e := range a.events {
		if (a.facct == "" || e.Account == a.facct) &&
			(a.fmodel == "" || e.Model == a.fmodel) &&
			(a.foutcome == "" || e.Outcome == a.foutcome) {
			out = append(out, e)
		}
	}
	return out
}

// cycleValue advances cur through the distinct values of pick over events,
// returning "" (all) after the last.
func cycleValue(events []view.Event, cur string, pick func(view.Event) string) string {
	seen := map[string]bool{}
	var vals []string
	for _, e := range events {
		v := pick(e)
		if v != "" && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return ""
	}
	if cur == "" {
		return vals[0]
	}
	for i, v := range vals {
		if v == cur && i+1 < len(vals) {
			return vals[i+1]
		}
	}
	return ""
}

func (m Model) activityKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := &m.activity
	switch msg.String() {
	case " ", "space":
		a.paused = !a.paused
		if a.paused {
			a.pausedAt = a.total
		} else {
			a.offset = 0
		}
	case "j", "down":
		if a.offset > 0 {
			a.offset--
		}
	case "k", "up":
		if a.offset < len(a.filtered())-1 {
			a.offset++
		}
		if !a.paused {
			a.paused = true // scrolling back implies holding the tail still
			a.pausedAt = a.total
		}
	case "G", "end":
		a.offset = 0
		a.paused = false
	case "a":
		a.facct = cycleValue(a.events, a.facct, func(e view.Event) string { return e.Account })
	case "m":
		a.fmodel = cycleValue(a.events, a.fmodel, func(e view.Event) string { return e.Model })
	case "c":
		a.foutcome = cycleValue(a.events, a.foutcome, func(e view.Event) string { return e.Outcome })
	case "esc":
		a.facct, a.fmodel, a.foutcome = "", "", ""
	case "v":
		a.showLog = !a.showLog
	}
	return m, nil
}

func (m Model) activityFooter() []string {
	if m.activity.showLog {
		return []string{"v requests", "j/k scroll"}
	}
	pause := "space pause"
	if m.activity.paused {
		pause = "space resume"
	}
	return []string{pause, "j/k scroll", "a/m/c filter", "esc clear", "v log"}
}

// activityColumns picks which columns fit, dropping from the right-hand
// detail first (cache tokens, then TTFB, then tokens, then duration) and
// never the identity columns a glance needs: time, model, account, status.
type actCol struct {
	title string
	width int
	right bool
	cell  func(m Model, e view.Event) string
}

func activityColumns(width int) []actCol {
	all := []actCol{
		{"time", 8, false, func(m Model, e view.Event) string { return formatClockSec(e.Time, m.loc) }},
		{"model", 21, false, func(m Model, e view.Event) string { return e.Model }},
		{"account", 18, false, func(m Model, e view.Event) string { return m.accountLabel(e.Account) }},
		{"st", 3, false, func(m Model, e view.Event) string { return fmt.Sprintf("%d", e.Status) }},
		{"dur", 7, true, func(m Model, e view.Event) string { return formatMS(e.DurationMS) }},
		{"ttfb", 7, true, func(m Model, e view.Event) string { return formatMS(e.TTFBMS) }},
		{"in", 7, true, func(m Model, e view.Event) string { return formatTokens(e.InputTokens) }},
		{"out", 7, true, func(m Model, e view.Event) string { return formatTokens(e.OutputTokens) }},
		{"cache r", 8, true, func(m Model, e view.Event) string { return formatTokens(e.CacheReadTokens) }},
		{"cache w", 8, true, func(m Model, e view.Event) string { return formatTokens(e.CacheWriteTokens) }},
	}
	// Drop order, rightmost detail first. Indexes into all.
	dropOrder := []int{9, 8, 6, 7, 5, 4}
	kept := make([]bool, len(all))
	for i := range kept {
		kept[i] = true
	}
	need := func() int {
		n := 2 // left margin
		for i, c := range all {
			if kept[i] {
				n += c.width + 2
			}
		}
		return n
	}
	for _, d := range dropOrder {
		if need() <= width {
			break
		}
		kept[d] = false
	}
	out := make([]actCol, 0, len(all))
	for i, c := range all {
		if kept[i] {
			out = append(out, c)
		}
	}
	return out
}

// accountLabel resolves an account id to its label; a vanished account keeps
// its id so history stays attributable.
func (m Model) accountLabel(id string) string {
	for _, a := range m.accounts {
		if a.ID == id {
			return a.Label
		}
	}
	return id
}

func (m Model) viewActivity(h int) string {
	if m.activity.showLog {
		return m.viewLog(h)
	}
	th := m.th
	a := m.activity
	events := m.activity.filtered()

	// Caption: filters and pause state — the feed's own status line.
	var facets []string
	if a.facct != "" {
		facets = append(facets, "account "+th.identity(a.facct, m.accountLabel(a.facct)))
	}
	if a.fmodel != "" {
		facets = append(facets, "model "+a.fmodel)
	}
	if a.foutcome != "" {
		facets = append(facets, "outcome "+a.foutcome)
	}
	left := th.dim("live feed")
	if len(facets) > 0 {
		left = th.dim("filtered: ") + strings.Join(facets, th.dim(" · "))
	}
	right := ""
	if a.paused {
		right = th.warn("paused")
		if n := a.total - a.pausedAt; n > 0 {
			right += th.dim(fmt.Sprintf(" · %d new", n))
		}
	}
	caption := "  " + spread(left, right, m.width-2)

	if len(events) == 0 {
		hint := strings.Join([]string{
			"waiting for the first request — export both for Claude Code:",
			"ANTHROPIC_BASE_URL=http://" + displayAddr(m.status.ListenAddr),
			claudeCodeFirstPartyEnv,
		}, "\n")
		if len(a.events) > 0 {
			hint = "nothing matches these filters — esc clears them"
		}
		return caption + "\n\n" + overlay(th.dim(hint), m.width, h-2)
	}

	cols := activityColumns(m.width)
	head := make([]string, 0, len(cols))
	for _, c := range cols {
		if c.right {
			head = append(head, padLeft(c.title, c.width))
		} else {
			head = append(head, padRight(c.title, c.width))
		}
	}
	header := "  " + th.dim(strings.Join(head, "  "))

	rows := h - 3 // caption, blank, header
	if rows < 1 {
		rows = 1
	}
	end := len(events) - a.offset
	if end > len(events) {
		end = len(events)
	}
	if end < 0 {
		end = 0
	}
	start := end - rows
	if start < 0 {
		start = 0
	}

	var b strings.Builder
	b.WriteString(caption + "\n\n" + header + "\n")
	for _, e := range events[start:end] {
		cells := make([]string, 0, len(cols))
		for _, c := range cols {
			v := c.cell(m, e)
			if c.right {
				v = padLeft(v, c.width)
			} else {
				v = padRight(v, c.width)
			}
			switch c.title {
			case "account":
				v = th.identity(e.Account, v)
			case "st":
				v = th.sev(statusSeverity(e.Status, e.Outcome), v)
			case "time":
				v = th.dim(v)
			}
			cells = append(cells, v)
		}
		b.WriteString("  " + strings.Join(cells, "  ") + "\n")
	}
	return b.String()
}

// statusSeverity grades one request's outcome: server errors and exhausted
// retries are bad, client errors and rate limits warn, success is quiet.
func statusSeverity(status int, outcome string) severity {
	switch {
	case status >= 500 || outcome == "error":
		return sevBad
	case status == 429 || (status >= 400 && status < 500):
		return sevWarn
	default:
		return sevOK
	}
}

func (m Model) viewLog(h int) string {
	th := m.th
	if m.logs == nil {
		return "\n" + overlay(th.dim("no log buffer attached"), m.width, h-1)
	}
	lines := m.logs.Snapshot()
	rows := h - 2
	if rows < 1 {
		rows = 1
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	var b strings.Builder
	b.WriteString("  " + th.dim("log — newest last") + "\n\n")
	for _, l := range lines {
		lvl := th.dim(l.Level)
		switch l.Level {
		case "WARN":
			lvl = th.warn(l.Level)
		case "ERROR":
			lvl = th.bad(l.Level)
		}
		b.WriteString("  " + th.dim(formatClockSec(l.At, m.loc)) + " " + lvl + " " +
			truncate(l.Text, m.width-16) + "\n")
	}
	return b.String()
}
