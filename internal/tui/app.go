package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nicko170/aiproxy/internal/view"
)

const claudeCodeFirstPartyEnv = "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1"

// screenID names the five screens, in tab order.
type screenID int

const (
	screenOverview screenID = iota
	screenActivity
	screenUsage
	screenAccounts
	screenSettings
	screenCount
)

func (s screenID) String() string {
	return [...]string{"overview", "activity", "usage", "accounts", "settings"}[s]
}

// refreshEvery is the cadence of the status/accounts snapshot. It is a
// glanceable instrument, not a profiler; two seconds keeps the countdowns
// honest without hammering the seam.
const refreshEvery = 2 * time.Second

// overviewEvery is the cadence of the heavier overview queries (per-account
// quota history and the throughput sparkline series).
const overviewEvery = 15 * time.Second

// fetchTimeout bounds every Source call made from a command. The render path
// never touches the Source at all; a query slower than this surfaces as an
// error line, never as a frozen frame.
const fetchTimeout = 10 * time.Second

// flash is the transient one-line acknowledgement above the footer: what
// just happened, in the severity it happened with.
type flash struct {
	text  string
	sev   severity
	until time.Time
}

// Model is the whole TUI. Everything View renders lives here as plain data;
// every Source call happens in a tea.Cmd on its own goroutine and lands back
// as a message. now is a field, not time.Now(): the render path takes no
// clock and no lock, so a frame is a pure function of this struct.
type Model struct {
	src     view.Source
	ctx     context.Context
	th      theme
	loc     *time.Location
	version string
	logs    *LogRing

	width, height int
	now           time.Time
	screen        screenID
	help          bool
	flash         flash

	status    view.Status
	statusErr string
	// updateInstalled is the version a successful in-app update wrote to disk
	// this session, or "". It is TUI-local rather than a Status field because
	// it is a fact about this UI's own action, not about the running proxy:
	// the flash announcing it expires in five seconds and the pending restart
	// does not, so the header keeps saying so.
	updateInstalled string
	accounts        []view.Account
	accountsAt      time.Time

	// resets holds the latest observed reset instant per account per bucket
	// (unix ms), read from quota history; sparks holds each account's
	// last-hour throughput per minute.
	resets map[string]map[string]int64
	sparks map[string][]float64

	activity activityState
	usage    usageState
	accts    acctsState
	settings settingsState
	login    loginState

	fetchingStatus   bool
	fetchingOverview bool
	lastOverviewAt   time.Time
}

// New builds the TUI over src. ctx cancels the event subscription and every
// in-flight fetch when the program exits. logs may be nil.
func New(ctx context.Context, src view.Source, version string, logs *LogRing) Model {
	return Model{
		src:     src,
		ctx:     ctx,
		th:      newTheme(),
		loc:     time.Local,
		version: version,
		logs:    logs,
		now:     time.Now(),
		resets:  map[string]map[string]int64{},
		sparks:  map[string][]float64{},
		activity: activityState{
			events: make([]view.Event, 0, activityRing),
		},
		usage:    newUsageState(),
		settings: newSettingsState(),
	}
}

// Run runs the program until quit or ctx cancellation.
func Run(ctx context.Context, src view.Source, version string, logs *LogRing) error {
	p := tea.NewProgram(New(ctx, src, version, logs), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	if err != nil && ctx.Err() != nil {
		// A cancelled context (SIGINT while the server winds down) is a
		// normal exit, not a TUI failure.
		return nil
	}
	return err
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.fetchStatus(),
		m.fetchOverview(),
		m.enterSettings(), // the overview gauge's redline needs switchThreshold
		m.subscribe(),
		tickCmd(),
	)
}

// --- messages ---

type tickMsg time.Time

type statusMsg struct {
	status   view.Status
	accounts []view.Account
	err      error
}

type overviewMsg struct {
	resets map[string]map[string]int64
	sparks map[string][]float64
	err    error
}

type subscribedMsg struct {
	ch  <-chan view.Event
	err error
}

type eventMsg view.Event

type eventsClosedMsg struct{}

// actionMsg reports a fire-and-forget mutation: did is the past-tense
// success note, fail the verb for the error line — the same word the key
// hint used, kept through the flow.
type actionMsg struct {
	did  string
	fail string
	err  error
}

type openedMsg struct{ err error }

// updateAppliedMsg reports an in-app update attempt. It is not an actionMsg:
// the success wording comes from view.UpdateResult.Message (one place words
// "restart to apply", shared with the CLI) rather than from a past-tense verb
// chosen here.
type updateAppliedMsg struct {
	res view.UpdateResult
	err error
}

// --- commands ---

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) fetchStatus() tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		st, err := src.ServerStatus(ctx)
		if err != nil {
			return statusMsg{err: err}
		}
		accts, err := src.Accounts(ctx)
		if err != nil {
			return statusMsg{status: st, err: err}
		}
		return statusMsg{status: st, accounts: accts}
	}
}

// fetchOverview reads the heavier per-account data: latest reset instants
// from quota history, and one hour of per-minute throughput for the
// sparklines. It re-reads the account list itself rather than trusting the
// model's copy, so an account added or removed mid-flight cannot desync it.
func (m Model) fetchOverview() tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		now := time.Now()
		accts, err := src.Accounts(ctx)
		if err != nil {
			return overviewMsg{err: err}
		}

		resets := make(map[string]map[string]int64, len(accts))
		w := view.Window{From: now.Add(-24 * time.Hour).UnixMilli(), To: now.UnixMilli()}
		for _, a := range accts {
			hist, err := src.AccountQuotaHistory(ctx, a.ID, w)
			if err != nil {
				continue // an account may vanish between the list and this read
			}
			per := map[string]int64{}
			latest := map[string]int64{}
			for _, p := range hist {
				if p.At >= latest[p.Bucket] && p.ResetsAt > 0 {
					latest[p.Bucket] = p.At
					per[p.Bucket] = p.ResetsAt
				}
			}
			resets[a.ID] = per
		}

		series, err := src.UsageSeries(ctx, view.SeriesQuery{
			Window:      view.Window{From: now.Add(-time.Hour).UnixMilli(), To: now.UnixMilli()},
			Granularity: view.GranularityMinute,
			GroupBy:     view.GroupByAccount,
		})
		if err != nil {
			return overviewMsg{resets: resets, err: err}
		}
		samples := map[string][]sparkSample{}
		for _, p := range series.Points {
			samples[p.Key] = append(samples[p.Key], sparkSample{At: p.BucketStart, V: float64(p.Requests)})
		}
		sparks := map[string][]float64{}
		for id, ss := range samples {
			sparks[id] = sparkBuckets(ss, now.Add(-time.Hour).UnixMilli(), now.UnixMilli(), sparkCells)
		}
		return overviewMsg{resets: resets, sparks: sparks}
	}
}

func (m Model) subscribe() tea.Cmd {
	src := m.src
	ctx := m.ctx
	return func() tea.Msg {
		ch, err := src.Subscribe(ctx)
		return subscribedMsg{ch: ch, err: err}
	}
}

// waitEvent blocks (in its own goroutine, never the render path) for the
// next live event. The channel is closed when ctx is cancelled.
func waitEvent(ch <-chan view.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg(ev)
	}
}

func (m Model) probeNow() tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, fetchTimeout)
		defer cancel()
		return actionMsg{did: "quota probe started", fail: "probe", err: src.ProbeNow(ctx)}
	}
}

// applyUpdate installs the latest release. The download runs in a command, on
// its own goroutine, with a timeout of its own: fetching several megabytes has
// nothing to do with fetchTimeout, which bounds a status read.
func (m Model) applyUpdate() tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
		defer cancel()
		res, err := src.ApplyUpdate(ctx)
		return updateAppliedMsg{res: res, err: err}
	}
}

// openDashboard opens the control UI in the default browser, from a command
// so a slow launcher never stalls a frame.
func (m Model) openDashboard() tea.Cmd {
	url := "http://" + displayAddr(m.status.ListenAddr) + "/_aiproxy/"
	return func() tea.Msg {
		var c *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			c = exec.Command("open", url)
		case "windows":
			c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		default:
			c = exec.Command("xdg-open", url)
		}
		return openedMsg{err: c.Start()}
	}
}

// --- update ---

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		var cmds []tea.Cmd
		cmds = append(cmds, tickCmd())
		if !m.fetchingStatus && m.now.Sub(m.accountsAt) >= refreshEvery {
			m.fetchingStatus = true
			cmds = append(cmds, m.fetchStatus())
		}
		if !m.fetchingOverview && m.now.Sub(m.lastOverviewAt) >= overviewEvery {
			m.fetchingOverview = true
			cmds = append(cmds, m.fetchOverview())
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.fetchingStatus = false
		m.accountsAt = m.now
		if msg.err != nil {
			m.statusErr = msg.err.Error()
		} else {
			m.statusErr = ""
		}
		if msg.accounts != nil || msg.err == nil {
			m.accounts = msg.accounts
		}
		m.status = msg.status
		return m, nil

	case overviewMsg:
		m.fetchingOverview = false
		m.lastOverviewAt = m.now
		if msg.resets != nil {
			m.resets = msg.resets
		}
		if msg.sparks != nil {
			m.sparks = msg.sparks
		}
		return m, nil

	case subscribedMsg:
		if msg.err != nil {
			m.flash = m.newFlash(sevBad, "live feed unavailable: "+msg.err.Error())
			return m, nil
		}
		m.activity.channel = msg.ch
		return m, waitEvent(msg.ch)

	case eventMsg:
		m.activity.append(view.Event(msg))
		return m, waitEvent(m.activity.ch())

	case eventsClosedMsg:
		return m, nil

	case actionMsg:
		if msg.err != nil {
			m.flash = m.newFlash(sevBad, msg.fail+" failed: "+msg.err.Error())
			return m, nil
		}
		m.flash = m.newFlash(sevOK, msg.did)
		// A mutation changed the world; re-read it now rather than at the
		// next cadence tick.
		m.fetchingStatus = true
		return m, m.fetchStatus()

	case openedMsg:
		if msg.err != nil {
			m.flash = m.newFlash(sevBad, "open dashboard failed: "+msg.err.Error())
		}
		return m, nil

	case updateAppliedMsg:
		if msg.err != nil {
			m.flash = m.newFlash(sevBad, "update failed: "+msg.err.Error())
			return m, nil
		}
		if msg.res.Updated {
			m.updateInstalled = msg.res.Version
		}
		m.flash = m.newFlash(sevOK, msg.res.Message)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Screen-specific messages.
	var cmd tea.Cmd
	m, cmd = m.updateUsage(msg)
	if cmd != nil {
		return m, cmd
	}
	m, cmd = m.updateAccounts(msg)
	if cmd != nil {
		return m, cmd
	}
	m, cmd = m.updateSettings(msg)
	if cmd != nil {
		return m, cmd
	}
	m, cmd = m.updateLogin(msg)
	return m, cmd
}

func (m Model) newFlash(sev severity, text string) flash {
	return flash{text: text, sev: sev, until: m.now.Add(5 * time.Second)}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A focused input owns the keyboard; only its handler sees keys.
	if m.login.active {
		return m.loginKey(msg)
	}
	if m.screen == screenSettings && m.settings.editing {
		return m.settingsKey(msg)
	}
	if m.screen == screenAccounts && (m.accts.confirming || m.accts.importing) {
		return m.accountsKey(msg)
	}

	if m.help {
		switch msg.String() {
		case "?", "esc", "q":
			m.help = false
		}
		return m, nil
	}

	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "1":
		m.screen = screenOverview
	case "2":
		m.screen = screenActivity
	case "3":
		m.screen = screenUsage
		return m, m.enterUsage()
	case "4":
		m.screen = screenAccounts
	case "5":
		m.screen = screenSettings
		return m, m.enterSettings()
	case "tab":
		return m.switchScreen((m.screen + 1) % screenCount)
	case "shift+tab":
		return m.switchScreen((m.screen + screenCount - 1) % screenCount)
	case "?":
		m.help = true
	case "l":
		return m.startLogin()
	case "p":
		return m, m.probeNow()
	case "o":
		return m, m.openDashboard()
	case "u":
		return m.startUpdate()
	default:
		switch m.screen {
		case screenActivity:
			return m.activityKey(msg)
		case screenUsage:
			return m.usageKey(msg)
		case screenAccounts:
			return m.accountsKey(msg)
		case screenSettings:
			return m.settingsKey(msg)
		}
	}
	return m, nil
}

// startUpdate applies an available update, or explains why it will not. The
// three no-op cases are distinguished on purpose: "nothing available", "your
// check is switched off", and "this is a dev build" are different situations,
// and one shared "nothing to do" would leave two of them looking like a bug.
func (m Model) startUpdate() (tea.Model, tea.Cmd) {
	switch {
	case m.updateInstalled != "":
		m.flash = m.newFlash(sevOK, "already updated to "+m.updateInstalled+" — restart to apply")
	case m.status.Update.DevBuild:
		m.flash = m.newFlash(sevWarn, "dev build — install a release to update in place")
	case m.status.Update.Disabled:
		m.flash = m.newFlash(sevWarn, "update checking is off — enable it in settings")
	case !m.status.Update.Available:
		m.flash = m.newFlash(sevOK, "no newer release available")
	default:
		m.flash = m.newFlash(sevOK, "updating to "+m.status.Update.LatestVersion+"…")
		return m, m.applyUpdate()
	}
	return m, nil
}

func (m Model) switchScreen(s screenID) (tea.Model, tea.Cmd) {
	m.screen = s
	switch s {
	case screenUsage:
		return m, m.enterUsage()
	case screenSettings:
		return m, m.enterSettings()
	}
	return m, nil
}

// --- view ---

func (m Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	header := m.viewHeader()
	tabs := m.viewTabs()
	rule := m.th.dim(strings.Repeat("─", m.width))
	footer := m.viewFooter()

	contentH := m.height - 5 // header, tabs, rule, flash line, footer
	if contentH < 1 {
		contentH = 1
	}

	var content string
	switch {
	case m.login.active:
		content = m.viewLogin(contentH)
	case m.help:
		content = m.viewHelp(contentH)
	default:
		switch m.screen {
		case screenOverview:
			content = m.viewOverview(contentH)
		case screenActivity:
			content = m.viewActivity(contentH)
		case screenUsage:
			content = m.viewUsage(contentH)
		case screenAccounts:
			content = m.viewAccounts(contentH)
		case screenSettings:
			content = m.viewSettings(contentH)
		}
	}
	content = fitHeight(content, contentH)

	return clipAnsi(strings.Join([]string{header, tabs, rule, content, m.viewFlash(), footer}, "\n"), m.width)
}

// lamp is the one-glyph answer to "is anything wrong?": the worst severity
// visible anywhere. It sits in the frame's top-left corner — the first cell
// a glance lands on.
func (m Model) lamp() severity {
	worst := sevOK
	raise := func(s severity) {
		if s > worst {
			worst = s
		}
	}
	if m.statusErr != "" {
		raise(sevBad)
	}
	if m.status.MetricsDropped > 0 || m.status.EventsDropped > 0 {
		raise(sevWarn)
	}
	if len(m.settings.needsRestart) > 0 {
		raise(sevWarn)
	}
	serving := 0
	for _, a := range m.accounts {
		if a.Disabled {
			continue
		}
		if a.Status != "active" {
			raise(sevBad)
			continue
		}
		if a.RateLimitedUntil > m.now.UnixMilli() || a.PausedUntil > m.now.UnixMilli() {
			raise(sevWarn)
			continue
		}
		serving++
	}
	if serving == 0 {
		raise(sevBad) // nothing can serve: no accounts, or all held out
	}
	for _, ps := range m.status.Probe.Accounts {
		if ps.LastError != "" {
			raise(sevWarn)
		}
	}
	return worst
}

func (m Model) viewHeader() string {
	th := m.th
	lamp := th.sev(m.lamp(), "●")
	if th.mode == modeNone {
		lamp = [...]string{"=", "!", "x"}[m.lamp()]
	}

	segs := []string{lamp + " " + th.bold("aiproxy")}
	segs = append(segs, th.dim("on ")+displayAddr(m.status.ListenAddr))
	if m.width >= 70 {
		segs = append(segs, th.dim("up ")+formatUptime(m.status.UptimeSeconds))
	}
	segs = append(segs, fmt.Sprintf("%d %s", m.status.InFlight, th.dim("in flight")))
	if m.width >= 90 {
		segs = append(segs, th.dim("p95 ")+formatMS(m.status.TTFBP95MS))
	}
	if d := m.status.MetricsDropped + m.status.EventsDropped; d > 0 {
		segs = append(segs, th.warn(fmt.Sprintf("%d dropped", d)))
	}
	if m.statusErr != "" {
		segs = append(segs, th.bad("status query failed"))
	}

	// The update segment degrades in three steps rather than being width-gated
	// like "up" and "p95" above. A gate would hide it entirely on an
	// 80-column terminal, and being seen is the whole point of it; appending it
	// unconditionally would let clipAnsi cut it mid-word ("^ 0.2.0 availab"),
	// which reads as a rendering fault. So: try the full wording, fall back to
	// just the version, and only drop it when even that will not fit.
	join := func(extra ...string) string {
		if len(extra) == 0 {
			return strings.Join(segs, th.dim("  ·  "))
		}
		return strings.Join(append(append([]string{}, segs...), extra...), th.dim("  ·  "))
	}

	// privacyOptions and updateOptions each end with "" — "omit this segment
	// entirely" — so the ladder below can shed one independently of the
	// other. Privacy is tried outer and update inner: for a given privacy
	// candidate every update candidate (down to omitted) is tried before the
	// privacy candidate itself shrinks, so an update notice is always shed
	// before the privacy segment is.
	privacyOptions := append(append([]string{}, m.privacySegments()...), "")
	updateOptions := append(append([]string{}, m.updateSegments()...), "")

	line := join()
outer:
	for _, p := range privacyOptions {
		for _, u := range updateOptions {
			var extra []string
			if p != "" {
				extra = append(extra, p)
			}
			if u != "" {
				extra = append(extra, u)
			}
			candidate := join(extra...)
			if lipgloss.Width(candidate) <= m.width {
				line = candidate
				break outer
			}
		}
	}
	return padAnsi(line, m.width)
}

// privacySegments returns the header's privacy wordings, longest first.
//
// Four states, most urgent first, because only one segment fits:
//
//  1. A scan error. The filter is not doing its job at all, and under
//     onScanFailure:open that is completely silent everywhere else.
//  2. Requests sent unfiltered. Same condition seen from the other side, and it
//     persists after the error that caused it has stopped recurring.
//  3. An unresolved placeholder — the agent received something wrong.
//  4. The redaction count, which merely means the filter is working.
//
// The first two outrank the third because "protecting nothing" is a failure of
// the feature's purpose, whereas an unresolved placeholder is a visible,
// self-correcting wrongness in one value (see the restore table's per-request
// note). All three outrank the count for the reason the count exists: a count
// is reassurance, and reassurance must never be shown in place of a fault.
func (m Model) privacySegments() []string {
	th := m.th
	p := m.status.Privacy
	if !p.Enabled {
		return nil
	}
	glyph := "⊘ "
	if th.mode == modeNone {
		glyph = "[!] "
	}
	if p.LastError != "" {
		return []string{
			th.bad(glyph + "filter error"),
			th.bad("filter error"),
		}
	}
	if p.SentUnfiltered > 0 {
		return []string{
			th.bad(fmt.Sprintf("%s%d unfiltered", glyph, p.SentUnfiltered)),
			th.bad(fmt.Sprintf("%d unfiltered", p.SentUnfiltered)),
		}
	}
	if p.Unresolved > 0 {
		return []string{
			th.bad(fmt.Sprintf("%s%d unresolved", glyph, p.Unresolved)),
			th.bad(fmt.Sprintf("%d unresolved", p.Unresolved)),
		}
	}
	var total int64
	for _, n := range p.Redactions {
		total += n
	}
	if total == 0 {
		return nil
	}
	return []string{
		th.dim(fmt.Sprintf("%s%d redacted", glyph, total)),
		th.dim(fmt.Sprintf("%d redacted", total)),
	}
}

// updateSegments returns the header's update wordings from longest to
// shortest, already styled, or nothing when there is no update to report.
//
// One wording, never both: updater.Checker.Apply clears Available the moment
// an update is installed, so "available" and "installed" cannot both be true —
// and an installed update outranks an available one regardless, because the
// action it asks for (restart) is the only one left.
func (m Model) updateSegments() []string {
	th := m.th
	arrow := "↑ "
	if th.mode == modeNone {
		arrow = "^ "
	}
	switch {
	case m.updateInstalled != "":
		sep := " · "
		if th.mode == modeNone {
			sep = " - "
		}
		// The shortest form keeps "restart" and drops the version, not the
		// other way round: under modeNone the two states have no colour to
		// tell them apart, and "^ 0.2.0" alone would read identically to an
		// update that is merely available — pointing at the wrong key.
		return []string{
			th.warn(arrow + m.updateInstalled + " installed" + sep + "restart"),
			th.warn(arrow + m.updateInstalled + sep + "restart"),
			th.warn(arrow + "restart"),
			th.warn("restart"),
		}
	case m.status.Update.Available:
		v := m.status.Update.LatestVersion
		return []string{
			th.accent(arrow + v + " available"),
			th.accent(arrow + v),
		}
	}
	return nil
}

func (m Model) viewTabs() string {
	th := m.th
	var parts []string
	for s := screenOverview; s < screenCount; s++ {
		name := s.String()
		key := fmt.Sprintf("%d", int(s)+1)
		if s == m.screen {
			parts = append(parts, th.accent(key)+" "+th.bold(name))
		} else {
			parts = append(parts, th.dim(key+" "+name))
		}
	}
	return padAnsi("  "+strings.Join(parts, "   "), m.width)
}

func (m Model) viewFlash() string {
	if m.flash.text == "" || m.now.After(m.flash.until) {
		return ""
	}
	return padAnsi("  "+m.th.sev(m.flash.sev, truncate(m.flash.text, m.width-4)), m.width)
}

func (m Model) viewFooter() string {
	th := m.th
	var keys []string
	switch {
	case m.login.active:
		keys = []string{"enter submit code", "ctrl+o open url", "esc cancel"}
	case m.help:
		keys = []string{"esc close"}
	default:
		switch m.screen {
		case screenOverview:
			keys = []string{"l login", "p probe", "o dashboard", "u update"}
		case screenActivity:
			keys = m.activityFooter()
		case screenUsage:
			keys = []string{"r range", "g group by"}
		case screenAccounts:
			keys = m.accountsFooter()
		case screenSettings:
			keys = m.settingsFooter()
		}
	}
	global := []string{"? help", "q quit"}

	render := func(ks []string) string {
		var parts []string
		for _, k := range ks {
			key, rest, found := strings.Cut(k, " ")
			if !found {
				parts = append(parts, th.dim(k))
				continue
			}
			parts = append(parts, th.accent(key)+" "+th.dim(rest))
		}
		return "  " + strings.Join(parts, th.dim("  ·  "))
	}
	line := render(append(append([]string{}, keys...), global...))
	// Too narrow: shed screen keys from the right, least essential last-
	// listed first; the global pair always survives.
	for lipgloss.Width(line) > m.width && len(keys) > 0 {
		keys = keys[:len(keys)-1]
		line = render(append(append([]string{}, keys...), global...))
	}
	return padAnsi(line, m.width)
}

func (m Model) viewHelp(h int) string {
	th := m.th
	// Every row here must be visible at a 28-line terminal, the shortest the
	// golden frames pin. overlay does not scroll and fitHeight clips the frame
	// from the bottom, so a panel one line too tall silently hides a key
	// rather than reporting anything — which is exactly what it did before the
	// blank separators came out and "q quit" (already in the footer of every
	// screen, this one included) stopped being repeated here. The dim section
	// headers do the grouping the blank lines used to.
	rows := [][2]string{
		{"1–5, tab", "switch screens"},
		{"l", "log in with Anthropic"},
		{"p", "probe quota now"},
		{"o", "open the dashboard"},
		{"u", "install the latest release"},
		{"activity", ""},
		{"space", "pause and resume the feed"},
		{"j/k", "scroll; G returns to live tail"},
		{"a m c", "filter by account, model, outcome"},
		{"v", "switch between requests and the log"},
		{"usage", ""},
		{"r", "cycle range: 1h, 24h, 7d, 30d"},
		{"g", "group by account, model, or outcome"},
		{"accounts", ""},
		{"e", "enable or disable"},
		{"+/-", "raise or lower priority"},
		{"x", "remove (asks first)"},
		{"i", "import credentials"},
	}
	// "q quit" is deliberately absent: the footer shows it on every screen,
	// including this one, and the panel is height-clipped by fitHeight — a
	// duplicated row costs two lines that a real key would otherwise use.
	var b strings.Builder
	// One newline, not two: the panel's own top padding already separates the
	// title, and at a 28-line terminal the extra line is what pushes the
	// closing border off the bottom.
	b.WriteString(th.bold("keys") + "\n")
	for _, r := range rows {
		if r[0] == "" && r[1] == "" {
			b.WriteString("\n")
			continue
		}
		if r[1] == "" {
			b.WriteString(th.dim(r[0]) + "\n")
			continue
		}
		b.WriteString("  " + th.accent(padRight(r[0], 10)) + r[1] + "\n")
	}
	return overlay(b.String(), m.width, h)
}

// overlay centres a panel in the content area with a thin border — the one
// place the UI draws a box, because a floating layer needs an edge to read
// as floating.
func overlay(body string, w, h int) string {
	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 3).
		Render(strings.TrimRight(body, "\n"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, panel)
}

// fitHeight pads or trims content to exactly h lines so the footer never
// drifts.
func fitHeight(s string, h int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// clipAnsi hard-cuts every line of s at width cells, ANSI-aware. This is the
// frame's last line of defence: a line that would wrap turns the whole
// screen to rubble, so an over-long one is cut instead, keeping its styling
// closed with a reset.
func clipAnsi(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if lipgloss.Width(line) <= width {
			continue
		}
		var b strings.Builder
		cells, styled := 0, false
		r := []rune(line)
		for j := 0; j < len(r); j++ {
			if r[j] == '\x1b' {
				k := j + 1
				if k < len(r) && r[k] == '[' {
					for k < len(r) && r[k] != 'm' {
						k++
					}
				}
				seq := string(r[j:min(k+1, len(r))])
				b.WriteString(seq)
				styled = seq != "\x1b[0m"
				j = k
				continue
			}
			if cells >= width {
				break
			}
			b.WriteRune(r[j])
			cells++
		}
		if styled {
			b.WriteString("\x1b[0m")
		}
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}

// padAnsi pads s with spaces to width cells, ANSI-aware, truncating by
// dropping nothing (headers compose themselves to fit; this only pads).
func padAnsi(s string, width int) string {
	if n := width - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// displayAddr renders a listen address for humans: an unspecified host
// stays as-is, but the empty string (status not loaded yet) reads as a
// placeholder rather than nothing.
func displayAddr(addr string) string {
	if addr == "" {
		return "…"
	}
	return addr
}

// sortedBucketNames orders quota buckets the way the eye wants them: the
// short window first, then the long one, then per-model buckets.
func sortedBucketNames(buckets map[string]float64) []string {
	names := make([]string, 0, len(buckets))
	for n := range buckets {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		return bucketRank(names[i]) < bucketRank(names[j]) ||
			(bucketRank(names[i]) == bucketRank(names[j]) && names[i] < names[j])
	})
	return names
}

func bucketRank(name string) int {
	switch name {
	case "five_hour", "5h":
		return 0
	case "seven_day", "7d":
		return 1
	default:
		return 2
	}
}

// plural renders a count with its noun, without the "(s)" shrug.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// bucketLabel renders a provider bucket name as its instrument label.
func bucketLabel(name string) string {
	switch name {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "7d"
	}
	return strings.ReplaceAll(name, "_", " ")
}
