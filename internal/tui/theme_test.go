package tui

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// visibleWidth counts cells after stripping SGR sequences.
func visibleWidth(s string) int { return len([]rune(ansiRE.ReplaceAllString(s, ""))) }

func plainTheme() theme { return theme{mode: modeNone} }

func testTheme(t *testing.T, mode string) theme {
	t.Helper()
	switch mode {
	case "truecolor":
		return theme{mode: modeTrue}
	case "256":
		return theme{mode: mode256}
	case "16":
		return theme{mode: mode16}
	default:
		return plainTheme()
	}
}

func TestDetectColorMode(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want colorMode
	}{
		{"NO_COLOR wins over everything", map[string]string{"NO_COLOR": "1", "COLORTERM": "truecolor", "TERM": "xterm-256color"}, modeNone},
		{"dumb terminal", map[string]string{"TERM": "dumb"}, modeNone},
		{"truecolor via COLORTERM", map[string]string{"COLORTERM": "truecolor", "TERM": "xterm-256color"}, modeTrue},
		{"24bit via COLORTERM", map[string]string{"COLORTERM": "24bit", "TERM": "xterm"}, modeTrue},
		{"256 via TERM", map[string]string{"TERM": "xterm-256color"}, mode256},
		{"plain xterm gets 16", map[string]string{"TERM": "xterm"}, mode16},
		{"empty TERM still gets 16", map[string]string{}, mode16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"NO_COLOR", "COLORTERM", "TERM"} {
				t.Setenv(k, "")
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if c.env["NO_COLOR"] == "" {
				// t.Setenv("NO_COLOR", "") sets it to empty, which per the
				// NO_COLOR spec means *unset* semantics are what we want;
				// detectColorMode must treat empty as absent.
				t.Setenv("NO_COLOR", "")
			}
			if got := detectColorMode(); got != c.want {
				t.Errorf("detectColorMode() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPaintDegradesThroughEveryMode(t *testing.T) {
	cases := []struct {
		mode theme
		want string
	}{
		{testTheme(t, "truecolor"), "\x1b[38;2;88;166;111mx\x1b[0m"},
		{testTheme(t, "256"), "\x1b[38;5;71mx\x1b[0m"},
		{testTheme(t, "16"), "\x1b[32mx\x1b[0m"},
		{plainTheme(), "x"},
	}
	for _, c := range cases {
		if got := c.mode.ok("x"); got != c.want {
			t.Errorf("mode %v: ok(x) = %q, want %q", c.mode.mode, got, c.want)
		}
	}
}

func TestPaintBright16ColorsUseAixtermRange(t *testing.T) {
	th := testTheme(t, "16")
	if got := th.dim("x"); got != "\x1b[90mx\x1b[0m" {
		t.Errorf("dim in 16-mode = %q, want bright black 90", got)
	}
}

func TestFillReversesVideo(t *testing.T) {
	th := testTheme(t, "16")
	got := th.fill(sevOK, "ab")
	if !strings.Contains(got, "\x1b[7;32m") {
		t.Errorf("fill = %q, want reverse+green", got)
	}
	if plainTheme().fill(sevOK, "ab") != "ab" {
		t.Errorf("plain fill must be a no-op")
	}
}

func TestIdentityColorIsStablePerKey(t *testing.T) {
	th := testTheme(t, "256")
	a := th.identity("acc-1", "x")
	b := th.identity("acc-1", "x")
	if a != b {
		t.Errorf("identity not stable: %q vs %q", a, b)
	}
	// Different keys usually differ; at minimum the call must not panic on
	// the empty key.
	_ = th.identity("", "x")
}

// Two accounts whose keys hash to the same slot were rendered in the same
// colour, which in a two-account install makes the usage chart unreadable:
// every series looks like one series. Hashing alone gives stability but says
// nothing about distinctness, and with a six-colour cycle a given pair
// collides one time in six.
func TestIdentityGivesCollidingKeysDifferentColours(t *testing.T) {
	// These two collide under the bare hash — that is why they are the fixture.
	const a, b = "acct-2", "acct-4"
	if identitySlot(a) != identitySlot(b) {
		t.Fatalf("fixture no longer collides (%d vs %d); pick another pair",
			identitySlot(a), identitySlot(b))
	}

	th := testTheme(t, "256")
	th.ident = assignIdentities([]string{a, b})
	if th.identity(a, "x") == th.identity(b, "x") {
		t.Error("colliding keys still render identically")
	}
}

// An account must not change colour because an unrelated account appeared or
// went away — the colour is how you track one account across screens.
func TestIdentityKeepsItsColourWhenOtherKeysChange(t *testing.T) {
	th := testTheme(t, "256")
	th.ident = assignIdentities([]string{"acct-2", "acct-4"})
	before := th.identity("acct-2", "x")

	th.ident = assignIdentities([]string{"acct-2", "acct-4", "acct-9"})
	if got := th.identity("acct-2", "x"); got != before {
		t.Errorf("acct-2 changed colour when acct-9 arrived: %q -> %q", before, got)
	}
}

// More keys than colours must still render rather than fail, falling back to
// the hash for whatever cannot be given a slot of its own.
func TestIdentityDegradesWhenKeysOutnumberColours(t *testing.T) {
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7"}
	got := assignIdentities(keys)
	if len(got) != len(keys) {
		t.Fatalf("assigned %d slots for %d keys", len(got), len(keys))
	}
	for _, k := range keys {
		if got[k] < 0 || got[k] >= len(identityCycle) {
			t.Errorf("key %q got slot %d, outside the cycle", k, got[k])
		}
	}
	distinct := map[int]bool{}
	for _, v := range got {
		distinct[v] = true
	}
	if len(distinct) != len(identityCycle) {
		t.Errorf("used %d of %d colours; every colour should be spent before any repeats",
			len(distinct), len(identityCycle))
	}
}
