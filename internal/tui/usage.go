package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// usageRanges are the four windows the range selector cycles through.
var usageRanges = []struct {
	name string
	span time.Duration
	gran view.Granularity
}{
	{"1h", time.Hour, view.GranularityMinute},
	{"24h", 24 * time.Hour, view.GranularityHour},
	{"7d", 7 * 24 * time.Hour, view.GranularityHour},
	{"30d", 30 * 24 * time.Hour, view.GranularityHour},
}

var usageGroups = []view.GroupBy{view.GroupByAccount, view.GroupByModel, view.GroupByOutcome}

// aggRow is one line of a ledger table: a key and its window totals.
type aggRow struct {
	key      string
	requests int64
	tokens   int64
	cost     int64
}

type usageState struct {
	rangeIdx int
	groupIdx int

	series   view.Series
	totals   view.Totals
	latency  view.Latency
	topModel []aggRow
	topAcct  []aggRow

	fetching bool
	loaded   bool
	err      string
}

func newUsageState() usageState {
	return usageState{rangeIdx: 1} // 24h: the "what has today cost me" default
}

type usageMsg struct {
	rangeIdx, groupIdx int
	series             view.Series
	totals             view.Totals
	latency            view.Latency
	topModel, topAcct  []aggRow
	err                error
}

// enterUsage fetches everything the screen shows. Stale-response guard: the
// message carries the range/group it was fetched for and Update discards a
// mismatch, so mashing r never interleaves windows.
func (m Model) enterUsage() tea.Cmd {
	if m.usage.fetching {
		return nil
	}
	m.usage.fetching = true // note: value copy; Update sets it too
	src := m.src
	parent := m.ctx
	ri, gi := m.usage.rangeIdx, m.usage.groupIdx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		r := usageRanges[ri]
		now := time.Now()
		w := view.Window{From: now.Add(-r.span).UnixMilli(), To: now.UnixMilli()}

		out := usageMsg{rangeIdx: ri, groupIdx: gi}
		var err error
		out.series, err = src.UsageSeries(ctx, view.SeriesQuery{Window: w, Granularity: r.gran, GroupBy: usageGroups[gi]})
		if err != nil {
			out.err = err
			return out
		}
		if out.totals, err = src.Totals(ctx, w); err != nil {
			out.err = err
			return out
		}
		if out.latency, err = src.LatencyPercentiles(ctx, w); err != nil {
			out.err = err
			return out
		}
		byModel, err := src.UsageSeries(ctx, view.SeriesQuery{Window: w, Granularity: r.gran, GroupBy: view.GroupByModel})
		if err != nil {
			out.err = err
			return out
		}
		out.topModel = topRows(byModel.Points, 5)
		byAcct, err := src.UsageSeries(ctx, view.SeriesQuery{Window: w, Granularity: r.gran, GroupBy: view.GroupByAccount})
		if err != nil {
			out.err = err
			return out
		}
		out.topAcct = topRows(byAcct.Points, 5)
		return out
	}
}

// topRows folds series points into per-key totals, ordered by cost then
// requests, keeping the top n.
func topRows(points []view.Point, n int) []aggRow {
	agg := map[string]*aggRow{}
	for _, p := range points {
		r := agg[p.Key]
		if r == nil {
			r = &aggRow{key: p.Key}
			agg[p.Key] = r
		}
		r.requests += p.Requests
		r.tokens += p.InputTokens + p.OutputTokens
		r.cost += p.CostMicros
	}
	rows := make([]aggRow, 0, len(agg))
	for _, r := range agg {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].cost != rows[j].cost {
			return rows[i].cost > rows[j].cost
		}
		if rows[i].requests != rows[j].requests {
			return rows[i].requests > rows[j].requests
		}
		return rows[i].key < rows[j].key
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

func (m Model) updateUsage(msg tea.Msg) (Model, tea.Cmd) {
	u, ok := msg.(usageMsg)
	if !ok {
		return m, nil
	}
	m.usage.fetching = false
	if u.rangeIdx != m.usage.rangeIdx || u.groupIdx != m.usage.groupIdx {
		return m, nil // stale: the selector moved while this was in flight
	}
	if u.err != nil {
		m.usage.err = u.err.Error()
		return m, nil
	}
	m.usage.err = ""
	m.usage.loaded = true
	m.usage.series = u.series
	m.usage.totals = u.totals
	m.usage.latency = u.latency
	m.usage.topModel = u.topModel
	m.usage.topAcct = u.topAcct
	return m, nil
}

func (m Model) usageKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "r":
		m.usage.rangeIdx = (m.usage.rangeIdx + 1) % len(usageRanges)
		m.usage.fetching = false
		return m, m.enterUsage()
	case "g":
		m.usage.groupIdx = (m.usage.groupIdx + 1) % len(usageGroups)
		m.usage.fetching = false
		return m, m.enterUsage()
	}
	return m, nil
}

// chartColumns folds series points into ncols columns of per-key request
// counts across [from, to).
func chartColumns(points []view.Point, from, to int64, ncols int) []map[string]float64 {
	if ncols <= 0 {
		return nil
	}
	out := make([]map[string]float64, ncols)
	for i := range out {
		out[i] = map[string]float64{}
	}
	span := to - from
	if span <= 0 {
		return out
	}
	for _, p := range points {
		if p.BucketStart < from || p.BucketStart >= to {
			continue
		}
		i := int((p.BucketStart - from) * int64(ncols) / span)
		if i >= ncols {
			i = ncols - 1
		}
		out[i][p.Key] += float64(p.Requests)
	}
	return out
}

// chartGroups orders keys by grand total, largest first, capped at six —
// the identity cycle's size; more series than colours is noise, not signal.
func chartGroups(cols []map[string]float64) []string {
	totals := map[string]float64{}
	for _, c := range cols {
		for k, v := range c {
			totals[k] += v
		}
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 6 {
		keys = keys[:6]
	}
	return keys
}

// plainShades distinguish stacked groups without colour.
var plainShades = []rune("█▓▒░▚▞")

// renderChart draws stacked columns of height h. Each column scales to the
// global maximum; a non-zero column always shows at least one cell.
func renderChart(cols []map[string]float64, groups []string, h int, th theme) []string {
	if h <= 0 || len(cols) == 0 {
		return nil
	}
	gi := map[string]int{}
	for i, g := range groups {
		gi[g] = i
	}
	var max float64
	colTotal := make([]float64, len(cols))
	for i, c := range cols {
		for _, g := range groups {
			colTotal[i] += c[g]
		}
		if colTotal[i] > max {
			max = colTotal[i]
		}
	}
	// grid[i] is column i's stack of group indexes, bottom cell first,
	// distributed by each group's share of the column total.
	grid := make([][]int, len(cols)) // grid[i] = stack of group indexes, bottom first
	for i, c := range cols {
		if colTotal[i] <= 0 || max <= 0 {
			continue
		}
		colH := int(colTotal[i]/max*float64(h) + 0.5)
		if colH < 1 {
			colH = 1
		}
		stack := make([]int, 0, colH)
		for gIdx := len(groups) - 1; gIdx >= 0; gIdx-- { // smallest at top: draw big groups at the base
			g := groups[gIdx]
			if c[g] <= 0 {
				continue
			}
			n := int(c[g]/colTotal[i]*float64(colH) + 0.5)
			if n < 1 {
				n = 1
			}
			for j := 0; j < n && len(stack) < colH; j++ {
				stack = append(stack, gIdx)
			}
		}
		for len(stack) < colH {
			stack = append(stack, len(groups)-1)
		}
		// stack currently holds larger group indexes first (drawn bottom-up
		// later); reverse so index 0 is the bottom cell.
		for l, r := 0, len(stack)-1; l < r; l, r = l+1, r-1 {
			stack[l], stack[r] = stack[r], stack[l]
		}
		grid[i] = stack
	}

	lines := make([]string, h)
	for row := 0; row < h; row++ { // row 0 is the top line
		level := h - row // cells from the bottom this line represents
		var b strings.Builder
		for i := range cols {
			stack := grid[i]
			if len(stack) >= level {
				g := stack[level-1]
				ch := "█"
				if th.mode == modeNone {
					ch = string(plainShades[g%len(plainShades)])
				}
				b.WriteString(th.identity(groups[g], ch))
			} else {
				b.WriteString(" ")
			}
		}
		lines[row] = b.String()
	}
	return lines
}

func (m Model) viewUsage(h int) string {
	th := m.th
	u := m.usage
	r := usageRanges[u.rangeIdx]

	sel := "  " + th.dim("range ") + th.accent(r.name) +
		th.dim("   by ") + th.accent(string(usageGroups[u.groupIdx]))
	if u.err != "" {
		sel += "   " + th.bad("usage query failed: "+truncate(u.err, m.width-40))
	} else if u.fetching && !u.loaded {
		sel += "   " + th.dim("loading…")
	}

	if u.loaded && u.totals.Requests == 0 {
		hint := "no requests in the last " + r.name + " — the ledger starts with the first request served"
		return sel + "\n\n" + overlay(th.dim(hint), m.width, h-2)
	}

	chartW := m.width - 4
	if chartW > 120 {
		chartW = 120
	}
	chartH := 8
	if h < 18 {
		chartH = 5
	}
	now := m.now
	from, to := now.Add(-r.span).UnixMilli(), now.UnixMilli()
	cols := chartColumns(u.series.Points, from, to, chartW)
	groups := chartGroups(cols)
	chart := renderChart(cols, groups, chartH, th)

	var b strings.Builder
	b.WriteString(sel + "\n\n")
	var maxTotal float64
	for _, c := range cols {
		var t float64
		for _, g := range groups {
			t += c[g]
		}
		if t > maxTotal {
			maxTotal = t
		}
	}
	scale := th.dim(fmt.Sprintf("▲ %s req/%s", formatTokens(int64(maxTotal)), map[view.Granularity]string{view.GranularityMinute: "min", view.GranularityHour: "col"}[r.gran]))
	b.WriteString("  " + scale + "\n")
	for _, line := range chart {
		b.WriteString("  " + line + "\n")
	}
	// Legend under the chart, identity-coloured swatches.
	var legend []string
	for i, g := range groups {
		sw := "█"
		if th.mode == modeNone {
			sw = string(plainShades[i%len(plainShades)])
		}
		legend = append(legend, th.identity(g, sw)+" "+m.groupLabel(g))
	}
	if len(legend) > 0 {
		b.WriteString("  " + truncate(strings.Join(legend, th.dim("  ·  ")), m.width*2) + "\n")
	}
	b.WriteString("\n")

	// Totals ledger: one line, the numbers that answer "what did this cost".
	t := u.totals
	cost := formatCost(t.CostMicros)
	if t.UnpricedRequests > 0 {
		cost += th.warn(fmt.Sprintf(" +%d unpriced", t.UnpricedRequests))
	}
	tot := []string{
		fmt.Sprintf("%s %s", th.bold(formatTokens(t.Requests)), th.dim("requests")),
		fmt.Sprintf("%s %s", formatTokens(t.InputTokens), th.dim("in")),
		fmt.Sprintf("%s %s", formatTokens(t.OutputTokens), th.dim("out")),
		fmt.Sprintf("%s %s", formatTokens(t.CacheReadTokens), th.dim("cache r")),
		fmt.Sprintf("%s %s", formatTokens(t.CacheWriteTokens), th.dim("cache w")),
		fmt.Sprintf("%s %s", th.bold(cost), th.dim("cost")),
		fmt.Sprintf("%s/%s %s", formatMS(u.latency.TTFBP50MS), formatMS(u.latency.TTFBP95MS), th.dim("ttfb p50/p95")),
	}
	if m.width < 100 {
		tot = tot[:6]
	}
	b.WriteString("  " + strings.Join(tot, th.dim("   ")) + "\n\n")

	// Two ledgers: top models and top accounts, side by side when they fit.
	mt := m.ledger("top models", u.topModel, false)
	at := m.ledger("top accounts", u.topAcct, true)
	if m.width >= 104 {
		b.WriteString(sideBySide(mt, at, (m.width-6)/2))
	} else {
		b.WriteString(strings.Join(mt, "\n") + "\n\n" + strings.Join(at, "\n"))
	}
	return b.String()
}

// groupLabel maps a series key to what a person calls it: account ids
// become labels, everything else names itself.
func (m Model) groupLabel(key string) string {
	if l := m.accountLabel(key); l != key {
		return l
	}
	return key
}

// ledger renders a small totals table: key, requests, tokens, cost.
func (m Model) ledger(title string, rows []aggRow, identity bool) []string {
	th := m.th
	out := []string{"  " + th.dim(title)}
	if len(rows) == 0 {
		out = append(out, "  "+th.dim("—"))
		return out
	}
	for _, r := range rows {
		name := truncate(m.groupLabel(r.key), 24)
		cell := padRight(name, 26)
		if identity {
			cell = th.identity(r.key, cell)
		}
		out = append(out, fmt.Sprintf("  %s%s %s  %s %s  %s",
			cell,
			padLeft(formatTokens(r.requests), 7), th.dim("req"),
			padLeft(formatTokens(r.tokens), 7), th.dim("tok"),
			padLeft(formatCost(r.cost), 8)))
	}
	return out
}

// sideBySide lays two line-lists in two columns of colWidth.
func sideBySide(left, right []string, colWidth int) string {
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		b.WriteString(padAnsi(l, colWidth) + "  " + r + "\n")
	}
	return b.String()
}
