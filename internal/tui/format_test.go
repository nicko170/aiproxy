package tui

import (
	"testing"
	"time"
)

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1.0k"},
		{12437, "12.4k"},
		{999_949, "999.9k"},
		{1_000_000, "1.0M"},
		{1_240_000, "1.2M"},
		{2_500_000_000, "2.5B"},
		{-42, "-42"}, // never invents a unit for a negative
	}
	for _, c := range cases {
		if got := formatTokens(c.in); got != c.want {
			t.Errorf("formatTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatCost(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "$0.00"},
		{4_130_000, "$4.13"},
		{12_000, "$0.012"},
		{900, "$0.0009"},
		{125_000_000, "$125.00"},
	}
	for _, c := range cases {
		if got := formatCost(c.in); got != c.want {
			t.Errorf("formatCost(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatMS(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0ms"},
		{312, "312ms"},
		{1_240, "1.24s"},
		{9_950, "9.95s"},
		{59_400, "59.4s"},
		{61_000, "1m01s"},
		{60_000 * 12, "12m00s"},
		{3_600_000 * 2, "2h00m"},
	}
	for _, c := range cases {
		if got := formatMS(c.in); got != c.want {
			t.Errorf("formatMS(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatUptime(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0s"},
		{58, "58s"},
		{62, "1m"},
		{3600*4 + 60*12, "4h12m"},
		{86400*3 + 3600*2, "3d2h"},
	}
	for _, c := range cases {
		if got := formatUptime(c.in); got != c.want {
			t.Errorf("formatUptime(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatCountdown(t *testing.T) {
	cases := []struct {
		deltaMS int64
		want    string
	}{
		{0, "now"},
		{-5_000, "now"}, // already past: never shows a negative
		{42_000, "42s"},
		{60_000 * 48, "48m"},
		{3_600_000*2 + 60_000*9, "2h09m"},
		{86_400_000*6 + 3_600_000*3, "6d3h"},
	}
	for _, c := range cases {
		if got := formatCountdown(c.deltaMS); got != c.want {
			t.Errorf("formatCountdown(%d) = %q, want %q", c.deltaMS, got, c.want)
		}
	}
}

func TestFormatClock(t *testing.T) {
	at := time.Date(2026, 8, 17, 14, 32, 10, 0, time.UTC)
	if got := formatClock(at.UnixMilli(), time.UTC); got != "14:32" {
		t.Errorf("formatClock = %q, want 14:32", got)
	}
	if got := formatClockSec(at.UnixMilli(), time.UTC); got != "14:32:10" {
		t.Errorf("formatClockSec = %q, want 14:32:10", got)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"héllo!", 5, "héll…"}, // rune-aware, not byte-aware
	}
	for _, c := range cases {
		if got := truncate(c.in, c.width); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

func TestPadRightIsWidthExact(t *testing.T) {
	if got := padRight("ab", 5); got != "ab   " {
		t.Errorf("padRight = %q", got)
	}
	if got := padRight("abcdef", 4); got != "abc…" {
		t.Errorf("padRight over-width = %q", got)
	}
}
