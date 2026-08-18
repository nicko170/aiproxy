// Package tui is the terminal front-end. It reads everything through
// view.Source and imports nothing below that seam (enforced by
// TestTUIImportsOnlyTheViewSeam): swap the Source for a remote one and this
// entire package drives a detached daemon unchanged.
package tui

import (
	"fmt"
	"strings"
	"time"
)

// formatTokens renders a token count the way a glance wants it: exact under a
// thousand, one decimal of a named unit above. Negative counts cannot occur
// but must not invent a unit if they somehow do.
func formatTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
}

// formatCost renders micro-dollars. Cents precision once the amount is at
// least a cent; below that, enough decimals that a small number is never
// shown as $0.00 when it is not zero.
func formatCost(micros int64) string {
	d := float64(micros) / 1e6
	switch {
	case micros == 0:
		return "$0.00"
	case d >= 0.1:
		return fmt.Sprintf("$%.2f", d)
	case d >= 0.01:
		return fmt.Sprintf("$%.3f", d)
	default:
		return fmt.Sprintf("$%.4f", d)
	}
}

// formatMS renders a latency or duration at the precision that matters for
// its magnitude: milliseconds under a second, hundredths of a second under a
// minute, then minutes and hours.
func formatMS(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 10_000:
		return fmt.Sprintf("%.2fs", float64(ms)/1000)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms%60_000)/1000)
	default:
		return fmt.Sprintf("%dh%02dm", ms/3_600_000, (ms%3_600_000)/60_000)
	}
}

// formatUptime renders elapsed seconds with two units at most.
func formatUptime(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%dd%dh", sec/86400, (sec%86400)/3600)
	}
}

// formatCountdown renders time-until as a countdown. A reset that has already
// passed reads "now": the next probe will confirm it, and a negative
// countdown is a gauge lying about the future.
func formatCountdown(deltaMS int64) string {
	if deltaMS <= 0 {
		return "now"
	}
	sec := deltaMS / 1000
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%dd%dh", sec/86400, (sec%86400)/3600)
	}
}

// formatClock renders a wall-clock instant as HH:MM in loc.
func formatClock(unixMS int64, loc *time.Location) string {
	return time.UnixMilli(unixMS).In(loc).Format("15:04")
}

// formatClockSec renders a wall-clock instant as HH:MM:SS in loc.
func formatClockSec(unixMS int64, loc *time.Location) string {
	return time.UnixMilli(unixMS).In(loc).Format("15:04:05")
}

// truncate cuts s to width cells, rune-aware, ending in an ellipsis when
// anything was cut. Width 0 is empty; width 1 is just the ellipsis.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}

// wrapURL hard-wraps s into lines of at most width runes each. Unlike
// truncate, nothing is ever cut: concatenating the returned lines
// reconstructs s exactly. This is for content like a URL that has no spaces
// to word-wrap on and must stay fully present and selectable on screen
// rather than end in an ellipsis. width below 1 is treated as 1; an empty s
// yields a single empty line, never zero lines.
func wrapURL(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	r := []rune(s)
	if len(r) == 0 {
		return []string{""}
	}
	lines := make([]string, 0, (len(r)+width-1)/width)
	for len(r) > 0 {
		n := width
		if n > len(r) {
			n = len(r)
		}
		lines = append(lines, string(r[:n]))
		r = r[n:]
	}
	return lines
}

// padRight pads (or truncates) s to exactly width cells.
func padRight(s string, width int) string {
	s = truncate(s, width)
	if n := width - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// padLeft right-aligns s in width cells.
func padLeft(s string, width int) string {
	s = truncate(s, width)
	if n := width - len([]rune(s)); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}
