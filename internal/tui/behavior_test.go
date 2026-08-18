package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicko170/aiproxy/internal/view"
)

// --- narrow-terminal drop order ---

func colTitles(cols []actCol) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.title
	}
	return out
}

func TestActivityColumnsDropInDeclaredOrder(t *testing.T) {
	// The identity columns a glance needs survive every width.
	for _, w := range []int{40, 60, 80, 120, 500} {
		titles := colTitles(activityColumns(w))
		for _, must := range []string{"time", "model", "account", "st"} {
			found := false
			for _, ti := range titles {
				if ti == must {
					found = true
				}
			}
			if !found {
				t.Errorf("width %d: column %q missing (have %v)", w, must, titles)
			}
		}
	}
	// Monotone: everything shown narrow is also shown wide.
	prev := map[string]bool{}
	for _, w := range []int{40, 60, 80, 100, 120, 500} {
		cur := map[string]bool{}
		for _, ti := range colTitles(activityColumns(w)) {
			cur[ti] = true
		}
		for ti := range prev {
			if !cur[ti] {
				t.Errorf("column %q shown at a narrower width but dropped at %d", ti, w)
			}
		}
		prev = cur
	}
	// Sparklines are the first sacrifice, before any column.
	if showSparklines(60) {
		t.Error("60 columns should drop sparklines")
	}
	if !showSparklines(80) || !showSparklines(120) {
		t.Error("80 and 120 columns keep sparklines")
	}
	if len(activityColumns(120)) != 10 {
		t.Errorf("120 columns should fit all 10 columns, got %v", colTitles(activityColumns(120)))
	}
}

// --- activity ring and filters ---

func TestActivityRingIsBounded(t *testing.T) {
	var a activityState
	for i := 0; i < activityRing+150; i++ {
		a.append(view.Event{Time: int64(i)})
	}
	if len(a.events) != activityRing {
		t.Errorf("ring holds %d, want %d", len(a.events), activityRing)
	}
	if a.total != activityRing+150 {
		t.Errorf("total = %d, want %d", a.total, activityRing+150)
	}
	if a.events[len(a.events)-1].Time != int64(activityRing+149) {
		t.Error("newest event lost from the ring")
	}
}

func TestActivityFilterCyclesThroughSeenValuesAndBackToAll(t *testing.T) {
	events := []view.Event{{Account: "a1"}, {Account: "a2"}, {Account: "a1"}}
	pick := func(e view.Event) string { return e.Account }
	if v := cycleValue(events, "", pick); v != "a1" {
		t.Errorf("first cycle = %q, want a1", v)
	}
	if v := cycleValue(events, "a1", pick); v != "a2" {
		t.Errorf("second cycle = %q, want a2", v)
	}
	if v := cycleValue(events, "a2", pick); v != "" {
		t.Errorf("third cycle = %q, want all", v)
	}
	if v := cycleValue(nil, "x", pick); v != "" {
		t.Errorf("no events = %q, want all", v)
	}
}

func TestActivityScrollbackPausesTheTail(t *testing.T) {
	m := fixtureModel(120, 28)
	m.screen = screenActivity
	res, _ := m.activityKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = res.(Model)
	if !m.activity.paused {
		t.Error("scrolling back must pause the tail")
	}
	res, _ = m.activityKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	m = res.(Model)
	if m.activity.paused || m.activity.offset != 0 {
		t.Error("G must return to the live tail")
	}
}

// --- lamp ---

func TestLampSeverity(t *testing.T) {
	m := fixtureModel(120, 28)
	m.status.Probe = view.ProbeStatus{}
	m.settings.needsRestart = map[string]bool{}
	m.accounts[1].RateLimitedUntil = 0 // the fixture keeps one account held out
	if got := m.lamp(); got != sevOK {
		t.Errorf("healthy fixture lamp = %v, want ok", got)
	}

	rateLtd := m
	rateLtd.accounts = []view.Account{{ID: "x", Status: "active", RateLimitedUntil: m.now.UnixMilli() + 60_000}}
	if got := rateLtd.lamp(); got != sevBad {
		t.Errorf("all accounts held out = %v, want bad (nothing can serve)", got)
	}

	drops := m
	drops.status.EventsDropped = 3
	if got := drops.lamp(); got != sevWarn {
		t.Errorf("drops lamp = %v, want warn", got)
	}

	none := m
	none.accounts = nil
	if got := none.lamp(); got != sevBad {
		t.Errorf("no accounts lamp = %v, want bad", got)
	}

	errored := m
	errored.accounts = append([]view.Account{}, m.accounts...)
	errored.accounts[0] = view.Account{ID: "e", Status: "errored"}
	if got := errored.lamp(); got != sevBad {
		t.Errorf("errored account lamp = %v, want bad", got)
	}

	pending := m
	pending.settings.needsRestart = map[string]bool{"metricsRetentionDays": true}
	if got := pending.lamp(); got != sevWarn {
		t.Errorf("needs-restart lamp = %v, want warn", got)
	}
}

// --- settings honesty ---

func TestSettingsRestartBadgeFollowsAppliedExactly(t *testing.T) {
	m := fixtureModel(120, 28)
	m.screen = screenSettings
	m.settings.needsRestart = map[string]bool{}
	m.settings.justApplied = map[string]bool{}

	// The seam says one field is live and one needs a restart.
	m, _ = m.updateSettings(appliedMsg{
		s: m.settings.current,
		applied: view.Applied{
			Live:         []string{"switchThreshold"},
			NeedsRestart: []string{"metricsRetentionDays"},
		},
	})
	out := m.viewSettings(28)
	if !strings.Contains(out, "saved · restart to apply") {
		t.Error("a restart-gated field must say so")
	}
	if !strings.Contains(out, "applied") {
		t.Error("a live field may say it applied")
	}

	// The badge survives later updates that do not mention the field: only a
	// Live report (i.e. after restart the classification narrows) clears it.
	m, _ = m.updateSettings(appliedMsg{s: m.settings.current, applied: view.Applied{Live: []string{"switchThreshold"}}})
	if !strings.Contains(m.viewSettings(28), "saved · restart to apply") {
		t.Error("restart badge must persist until the field is actually live")
	}
	m, _ = m.updateSettings(appliedMsg{s: m.settings.current, applied: view.Applied{Live: []string{"metricsRetentionDays"}}})
	if strings.Contains(m.viewSettings(28), "saved · restart to apply") {
		t.Error("a field reported live must stop claiming it needs a restart")
	}
}

func TestSettingsValidationErrorShowsUnderTheRow(t *testing.T) {
	m := fixtureModel(120, 28)
	m.screen = screenSettings
	m, _ = m.updateSettings(appliedMsg{err: errFake("switchThreshold must be in (0, 1], got 1.4")})
	if !strings.Contains(m.viewSettings(28), "switchThreshold must be in (0, 1], got 1.4") {
		t.Error("the seam's refusal must be shown verbatim")
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// --- vanishing data ---

func TestAccountVanishingMidRenderDoesNotPanic(t *testing.T) {
	m := fixtureModel(120, 28)
	m.screen = screenAccounts
	m.accts.sel = 1
	m.accts.detail = true
	m.accts.historyFor = "gone"
	m.accounts = m.accounts[:1] // the selected account vanished
	_ = m.View()

	m.accounts = nil
	_ = m.View()

	// An event naming an unknown account keeps its id.
	if got := m.accountLabel("ghost"); got != "ghost" {
		t.Errorf("unknown account label = %q, want the id", got)
	}

	// A history reply for an account no longer selected is discarded.
	m2 := fixtureModel(120, 28)
	m2.accts.historyFor = "a1"
	m2, _ = m2.updateAccounts(historyMsg{accountID: "a2", points: []view.QuotaPoint{{At: 1}}})
	if len(m2.accts.history) != 0 {
		t.Error("stale history reply must be discarded")
	}
}

func TestStaleUsageReplyIsDiscarded(t *testing.T) {
	m := fixtureModel(120, 28)
	m.usage.rangeIdx = 2
	before := m.usage.totals
	m, _ = m.updateUsage(usageMsg{rangeIdx: 0, groupIdx: 0, totals: view.Totals{Requests: 999}})
	if m.usage.totals != before {
		t.Error("a reply for a range the user has left must not land")
	}
}

// --- chart geometry ---

func TestChartColumnsFoldPointsIntoBuckets(t *testing.T) {
	pts := []view.Point{
		{BucketStart: 0, Key: "a", Requests: 2},
		{BucketStart: 500, Key: "a", Requests: 1},
		{BucketStart: 1500, Key: "b", Requests: 4},
		{BucketStart: 2000, Key: "a", Requests: 9}, // outside
	}
	cols := chartColumns(pts, 0, 2000, 2)
	if cols[0]["a"] != 3 || cols[1]["b"] != 4 || cols[1]["a"] != 0 {
		t.Errorf("cols = %v", cols)
	}
	if got := chartColumns(nil, 0, 0, 3); len(got) != 3 {
		t.Errorf("degenerate window: %v", got)
	}
	if got := chartColumns(pts, 0, 2000, 0); got != nil {
		t.Errorf("zero columns: %v", got)
	}
}

func TestRenderChartKeepsWidthAndShowsSmallColumns(t *testing.T) {
	cols := []map[string]float64{{"a": 100}, {"a": 1}, {}}
	lines := renderChart(cols, []string{"a"}, 4, 2, plainTheme())
	if len(lines) != 4 {
		t.Fatalf("chart height %d, want 4", len(lines))
	}
	for _, l := range lines {
		if visibleWidth(l) != 6 {
			t.Errorf("chart line width %d, want 6: %q", visibleWidth(l), l)
		}
	}
	bottom := []rune(lines[3])
	if bottom[0] == ' ' || bottom[2] == ' ' {
		t.Errorf("non-zero columns must render at least one cell: %q", string(bottom))
	}
	if bottom[4] != ' ' {
		t.Errorf("zero column must stay empty: %q", string(bottom))
	}
}

// --- login never renders credential material ---

func TestLoginViewShowsURLNeverACredential(t *testing.T) {
	m := fixtureModel(100, 30)
	res, _ := m.startLogin()
	m = res.(Model)
	m, _ = m.updateLogin(loginStartedMsg{sess: view.LoginSession{URL: "https://example.test/authorize?x=1"}})
	out := m.View()
	if !strings.Contains(out, "https://example.test/authorize?x=1") {
		t.Error("login must show the authorize URL")
	}

	// A closed Done without a value is "no result", never success.
	m, _ = m.updateLogin(loginDoneMsg{ok: false})
	if !m.login.active {
		t.Error("a resultless close must keep the flow open with an error, not report success")
	}
	if m.login.err == "" {
		t.Error("a resultless close must say something happened")
	}
}
