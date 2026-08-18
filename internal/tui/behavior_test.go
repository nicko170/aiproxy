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

func TestClaudeCodeHintsPreserveNativeContextWindow(t *testing.T) {
	m := fixtureModel(120, 28)
	m.accounts = nil
	if got := m.viewNoAccounts(24); !strings.Contains(got, claudeCodeFirstPartyEnv) {
		t.Fatalf("new-user hint omits %q", claudeCodeFirstPartyEnv)
	}

	m = fixtureModel(120, 28)
	m.screen = screenActivity
	m.activity.events = nil
	if got := m.viewActivity(24); !strings.Contains(got, claudeCodeFirstPartyEnv) {
		t.Fatalf("empty-activity hint omits %q", claudeCodeFirstPartyEnv)
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

// The header must offer an update when one exists and say nothing at all when
// none does — a segment that is always present, reading "up to date", is
// noise on every frame for the sake of one.
func TestHeaderShowsAnAvailableUpdate(t *testing.T) {
	m := fixtureModel(120, 28)
	if strings.Contains(m.viewHeader(), "available") {
		t.Fatal("header mentions an update when none is available")
	}

	m.status.Update = view.UpdateStatus{
		CurrentVersion: "0.1.0", LatestVersion: "0.2.0", Available: true,
	}
	if !strings.Contains(m.viewHeader(), "0.2.0 available") {
		t.Errorf("header = %q, want it to offer 0.2.0", m.viewHeader())
	}
}

// Once installed, the header stops offering the update and starts asking for a
// restart: the flash that said so has five seconds, and the pending restart
// outlives it.
func TestHeaderAsksForARestartAfterInstalling(t *testing.T) {
	m := fixtureModel(120, 28)
	m.status.Update = view.UpdateStatus{CurrentVersion: "0.1.0", LatestVersion: "0.2.0"}
	m.updateInstalled = "0.2.0"
	h := m.viewHeader()
	if !strings.Contains(h, "0.2.0 installed") || !strings.Contains(h, "restart") {
		t.Errorf("header = %q, want it to ask for a restart", h)
	}
	if strings.Contains(h, "available") {
		t.Errorf("header = %q, must not offer and report the same update at once", h)
	}
}

// u with nothing available explains itself instead of failing.
func TestUWithNoUpdateAvailableFlashesAnExplanation(t *testing.T) {
	m := fixtureModel(80, 28)
	m.status.Update = view.UpdateStatus{CurrentVersion: "0.1.0", LatestVersion: "0.1.0"}
	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	got, _ := mustModel(next, cmd)
	if got.flash.text == "" {
		t.Error("u should say why it did nothing")
	}
	if cmd != nil {
		t.Error("u must not call the seam when there is nothing to install")
	}
}

// A dev build is told what it is, not that it is up to date.
func TestUOnADevBuildSaysSo(t *testing.T) {
	m := fixtureModel(80, 28)
	m.status.Update = view.UpdateStatus{CurrentVersion: "dev", DevBuild: true}
	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	got, _ := mustModel(next, cmd)
	if !strings.Contains(got.flash.text, "dev build") {
		t.Errorf("flash = %q, want it to mention a dev build", got.flash.text)
	}
}

// The result's Message is rendered verbatim: the wording of "restart to
// apply" lives in one place (view.Local.ApplyUpdate) so the TUI and the CLI
// cannot drift apart on it.
func TestUpdateAppliedMessageIsShownAndRemembered(t *testing.T) {
	m := fixtureModel(80, 28)
	next, _ := m.Update(updateAppliedMsg{res: view.UpdateResult{
		Updated: true, Version: "0.2.0", Message: "updated to 0.2.0 — restart to apply",
	}})
	got, _ := mustModel(next, nil)
	if got.flash.text != "updated to 0.2.0 — restart to apply" {
		t.Errorf("flash = %q", got.flash.text)
	}
	if got.updateInstalled != "0.2.0" {
		t.Errorf("updateInstalled = %q, want 0.2.0", got.updateInstalled)
	}
}

func TestUpdateFailureFlashesTheError(t *testing.T) {
	m := fixtureModel(80, 28)
	next, _ := m.Update(updateAppliedMsg{err: errFake("checksum mismatch")})
	got, _ := mustModel(next, nil)
	if !strings.Contains(got.flash.text, "checksum mismatch") {
		t.Errorf("flash = %q", got.flash.text)
	}
	if got.flash.sev != sevBad {
		t.Error("a failed update should read as bad")
	}
	if got.updateInstalled != "" {
		t.Error("a failed update must not claim a restart is pending")
	}
}

// The header sheds whole words rather than letting clipAnsi cut one in half,
// and whatever survives must still tell the operator which key to press. A
// bare version number means "press u"; the word restart means "you already
// did". Measured on View's first line, since that is the clipped one the
// operator sees — viewHeader alone only pads.
func TestUpdateHeaderDegradesWithoutBecomingAmbiguous(t *testing.T) {
	headerOf := func(m Model) string {
		return strings.SplitN(m.View(), "\n", 2)[0]
	}
	for _, w := range []int{40, 60, 80, 120} {
		avail := fixtureModel(w, 28)
		avail.status.Update = view.UpdateStatus{
			CurrentVersion: "0.1.0", LatestVersion: "0.2.0", Available: true,
		}
		instal := fixtureModel(w, 28)
		instal.status.Update = view.UpdateStatus{CurrentVersion: "0.1.0", LatestVersion: "0.2.0"}
		instal.updateInstalled = "0.2.0"

		availHdr, instalHdr := headerOf(avail), headerOf(instal)
		for name, hdr := range map[string]string{"available": availHdr, "installed": instalHdr} {
			if got := visibleWidth(hdr); got > w {
				t.Errorf("%s header at %d is %d cells wide", name, w, got)
			}
			// A truncated word is the failure mode a width gate would have
			// avoided and a bare append would have caused: any prefix present
			// must be present in full.
			for prefix, whole := range map[string]string{
				"availa": "available", "instal": "installed", "resta": "restart",
			} {
				if strings.Contains(hdr, prefix) && !strings.Contains(hdr, whole) {
					t.Errorf("%s header at %d was clipped mid-word: %q", name, w, hdr)
				}
			}
		}
		// At 40 columns the mandatory segments already fill the line, so
		// dropping the update segment is correct — but wherever one IS shown,
		// the two states must not read alike.
		if w >= 60 && availHdr == instalHdr {
			t.Errorf("at %d columns an available update and an installed one render identically:\n%q", w, availHdr)
		}
	}
}
