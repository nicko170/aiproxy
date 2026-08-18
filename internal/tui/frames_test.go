package tui

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/view"
)

var update = flag.Bool("update", false, "rewrite golden frames")

// fixtureLoginURL is a realistically long OAuth authorize URL (the real
// thing carries a client_id, redirect_uri, scope list, PKCE challenge, and
// state, and easily runs past any terminal's width) — long enough that the
// login frame at every golden width actually exercises wrapping rather than
// happening to fit on one line.
const fixtureLoginURL = "https://claude.ai/oauth/authorize?client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256&redirect_uri=http%3A%2F%2F127.0.0.1%2F54231%2Fcallback&response_type=code&scope=org%3Acreate_api_key%20user%3Aprofile%20user%3Ainference&state=abcdefghijklmnopqrstuvwxyzABCDEF12345678"

// fixtureNow is the fixed instant every frame renders at.
var fixtureNow = time.Date(2026, 8, 17, 14, 30, 0, 0, time.UTC)

// fixtureModel builds a fully-populated model without ever touching a
// Source: View is a pure function of this state, which is exactly what
// makes these frames reproducible.
func fixtureModel(w, h int) Model {
	m := New(context.Background(), nil, "test", nil)
	m.th = plainTheme()
	m.loc = time.UTC
	m.width, m.height = w, h
	m.now = fixtureNow
	nowMS := fixtureNow.UnixMilli()

	m.status = view.Status{
		ListenAddr: "127.0.0.1:8118", UptimeSeconds: 4*3600 + 12*60,
		InFlight: 2, TTFBP95MS: 1240,
		Probe: view.ProbeStatus{
			LastCompletedAt: nowMS - 3*60_000,
			Accounts: map[string]view.AccountProbeStatus{
				"a1": {LastSuccessAt: nowMS - 3*60_000},
				"a2": {LastSuccessAt: nowMS - 8*60_000, LastError: "quota read throttled (429)", NextAttemptAt: nowMS + 6*60_000},
			},
		},
	}
	m.accounts = []view.Account{
		{
			ID: "a1", Label: "nick@example.com (Personal)", Provider: "anthropic",
			Priority: 1, Status: "active", InFlight: 2,
			Buckets: map[string]float64{"five_hour": 0.63, "seven_day": 0.18, "7d_fable": 0.09},
		},
		{
			ID: "a2", Label: "ops@example.com (Ops)", Provider: "anthropic",
			Priority: 2, Status: "active",
			RateLimitedUntil: nowMS + 4*60_000,
			Buckets:          map[string]float64{"five_hour": 0.91, "seven_day": 0.44},
		},
	}
	m.resets = map[string]map[string]int64{
		"a1": {"five_hour": nowMS + (2*60+9)*60_000, "seven_day": nowMS + (3*24+2)*3_600_000},
		"a2": {"five_hour": nowMS + 11*60_000, "seven_day": nowMS + 26*3_600_000},
	}
	spark := make([]float64, sparkCells)
	for i := range spark {
		spark[i] = float64((i * 7 % 13))
	}
	burst := make([]float64, sparkCells)
	for i := 40; i < 52; i++ {
		burst[i] = float64(i - 39)
	}
	m.sparks = map[string][]float64{"a1": spark, "a2": burst}

	m.settings.loaded = true
	m.settings.current = view.Settings{
		SwitchThreshold: 0.8, RetryBudgetMS: 60_000, InlineAbsorbMaxMS: 15_000,
		HeaderTimeoutMS: 30_000, BodyIdleMS: 120_000, SessionAffinity: true,
		BlockedModels: []string{"claude-3-*"}, QuotaProbeIntervalSeconds: 300,
		MetricsRetentionDays: 90,
	}
	m.settings.needsRestart = map[string]bool{"metricsRetentionDays": true}
	m.settings.justApplied = map[string]bool{"switchThreshold": true}

	models := []string{"claude-fable-5", "claude-haiku-4", "claude-fable-5"}
	outcomes := []string{"ok", "ok", "rate_limited"}
	statuses := []int{200, 200, 429}
	for i := 0; i < 9; i++ {
		m.activity.append(view.Event{
			Time:    nowMS - int64(9-i)*47_000,
			Model:   models[i%3],
			Account: []string{"a1", "a1", "a2"}[i%3],
			Status:  statuses[i%3], Outcome: outcomes[i%3],
			DurationMS: int64(1200 + i*310), TTFBMS: int64(240 + i*35),
			InputTokens: int64(1200 + i*900), OutputTokens: int64(150 + i*60),
			CacheReadTokens: int64(i * 4000), CacheWriteTokens: int64(i%2) * 800,
		})
	}

	m.usage.loaded = true
	from := fixtureNow.Add(-24 * time.Hour).UnixMilli()
	for i := 0; i < 24; i++ {
		at := from + int64(i)*3_600_000
		m.usage.series.Points = append(m.usage.series.Points,
			view.Point{BucketStart: at, Key: "claude-fable-5", Requests: int64(3 + i%7)},
			view.Point{BucketStart: at, Key: "claude-haiku-4", Requests: int64(i % 4)},
		)
	}
	m.usage.series.GroupBy = view.GroupByModel
	m.usage.groupIdx = 1
	m.usage.totals = view.Totals{
		Requests: 412, InputTokens: 5_200_000, OutputTokens: 480_000,
		CacheReadTokens: 21_000_000, CacheWriteTokens: 300_000,
		CostMicros: 12_400_000, UnpricedRequests: 3,
	}
	m.usage.latency = view.Latency{TTFBP50MS: 480, TTFBP95MS: 1240, DurationP50MS: 3200, DurationP95MS: 9100}
	m.usage.topModel = []aggRow{
		{key: "claude-fable-5", requests: 310, tokens: 4_900_000, cost: 11_000_000},
		{key: "claude-haiku-4", requests: 102, tokens: 780_000, cost: 1_400_000},
	}
	m.usage.topAcct = []aggRow{
		{key: "a1", requests: 290, tokens: 4_100_000, cost: 9_300_000},
		{key: "a2", requests: 122, tokens: 1_580_000, cost: 3_100_000},
	}
	return m
}

// frameCases are every screen state a golden frame pins down.
func frameCases() map[string]func(w, h int) Model {
	return map[string]func(w, h int) Model{
		"overview": func(w, h int) Model { return fixtureModel(w, h) },
		"overview_empty": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.accounts = nil
			return m
		},
		"activity": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenActivity
			return m
		},
		"activity_paused_filtered": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenActivity
			m.activity.paused = true
			m.activity.pausedAt = m.activity.total - 3
			m.activity.facct = "a1"
			return m
		},
		"activity_empty": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenActivity
			m.activity.events = nil
			return m
		},
		"usage": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenUsage
			return m
		},
		"usage_empty": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenUsage
			m.usage.series = view.Series{}
			m.usage.totals = view.Totals{}
			m.usage.topModel, m.usage.topAcct = nil, nil
			return m
		},
		"accounts": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenAccounts
			m.accts.detail = true
			m.accts.historyFor = "a1"
			for i := 0; i < 24; i++ {
				at := fixtureNow.Add(-24*time.Hour + time.Duration(i)*time.Hour).UnixMilli()
				m.accts.history = append(m.accts.history,
					view.QuotaPoint{At: at, Bucket: "five_hour", Utilization: float64(i%10) / 10},
					view.QuotaPoint{At: at, Bucket: "seven_day", Utilization: float64(i) / 48},
				)
			}
			return m
		},
		"accounts_remove_confirm": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenAccounts
			m.accts.confirming = true
			return m
		},
		"settings": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.screen = screenSettings
			m.settings.sel = 1
			return m
		},
		"help": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.help = true
			return m
		},
		"login": func(w, h int) Model {
			m := fixtureModel(w, h)
			m, _ = mustModel(m.startLogin())
			m.login.sess = view.LoginSession{URL: fixtureLoginURL}
			return m
		},
	}
}

func mustModel(mo interface{ View() string }, _ any) (Model, bool) {
	m, ok := mo.(Model)
	return m, ok
}

func TestGoldenFrames(t *testing.T) {
	widths := []int{60, 80, 120}
	const height = 28
	for name, build := range frameCases() {
		for _, w := range widths {
			t.Run(fmt.Sprintf("%s_%d", name, w), func(t *testing.T) {
				m := build(w, height)
				got := m.View()

				// Invariants first: exact height, and no line wider than the
				// terminal — narrowing must degrade, never wrap.
				lines := strings.Split(got, "\n")
				if len(lines) != height {
					t.Errorf("frame is %d lines, want %d", len(lines), height)
				}
				for i, l := range lines {
					if vw := visibleWidth(l); vw > w {
						t.Errorf("line %d is %d cells wide, terminal is %d:\n%q", i, vw, w, l)
					}
				}

				path := filepath.Join("testdata", fmt.Sprintf("%s_%d.golden", name, w))
				if *update {
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					return
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("missing golden (run with -update): %v", err)
				}
				if got != string(want) {
					t.Errorf("frame differs from golden %s\n--- got ---\n%s", path, got)
				}
			})
		}
	}
}

// TestFramesSurviveExtremeWidths renders every screen at 40 and 500 columns:
// no panic, no overwide line, exact height. 40 columns is not pretty — it is
// merely required not to be rubble.
func TestFramesSurviveExtremeWidths(t *testing.T) {
	for name, build := range frameCases() {
		for _, w := range []int{40, 500} {
			for _, h := range []int{8, 28} {
				m := build(w, h)
				got := m.View()
				lines := strings.Split(got, "\n")
				if len(lines) != h {
					t.Errorf("%s at %dx%d: %d lines, want %d", name, w, h, len(lines), h)
				}
				for i, l := range lines {
					if vw := visibleWidth(l); vw > w {
						t.Errorf("%s at %dx%d: line %d is %d cells", name, w, h, i, vw)
					}
				}
			}
		}
	}
}

// TestStyledFramesKeepWidths renders with full colour and checks the ANSI
// clip still holds every line to the terminal.
func TestStyledFramesKeepWidths(t *testing.T) {
	for name, build := range frameCases() {
		for _, w := range []int{60, 120} {
			m := build(w, 28)
			m.th = testTheme(t, "truecolor")
			for i, l := range strings.Split(m.View(), "\n") {
				if vw := visibleWidth(l); vw > w {
					t.Errorf("%s at %d: line %d is %d cells", name, w, i, vw)
				}
			}
		}
	}
}
