package tui

import (
	"strings"
	"testing"
)

func TestGaugeGeometryFillIsProportional(t *testing.T) {
	cases := []struct {
		u        float64
		width    int
		wantFill int
	}{
		{0, 40, 0},
		{0.5, 40, 20},
		{1, 40, 40},
		{1.7, 40, 40},  // clamps: utilization above 100% cannot overflow the channel
		{-0.2, 40, 0},  // clamps low
		{0.024, 40, 1}, // any non-zero utilization shows at least one cell
		{0.5, 0, 0},    // zero width renders nothing rather than panicking
	}
	for _, c := range cases {
		g := gaugeGeometry(c.width, c.u, 0.8, "x")
		if g.fill != c.wantFill {
			t.Errorf("fill(u=%v,w=%d) = %d, want %d", c.u, c.width, g.fill, c.wantFill)
		}
		if len(g.cells) != c.width {
			t.Errorf("cells(u=%v,w=%d) len = %d, want %d", c.u, c.width, len(g.cells), c.width)
		}
	}
}

func TestGaugeGeometryTickMarksTheThreshold(t *testing.T) {
	g := gaugeGeometry(40, 0.1, 0.8, "")
	if g.tick != 32 {
		t.Errorf("tick = %d, want 32", g.tick)
	}
	// A threshold at or above 1 has no cell to mark: the redline is the end
	// of the channel itself.
	if g := gaugeGeometry(40, 0.1, 1.0, ""); g.tick != -1 {
		t.Errorf("tick at threshold 1.0 = %d, want -1", g.tick)
	}
	if g := gaugeGeometry(40, 0.1, 0, ""); g.tick != -1 {
		t.Errorf("tick at threshold 0 = %d, want -1", g.tick)
	}
}

func TestGaugeGeometryTextFlowsInsideTheChannel(t *testing.T) {
	g := gaugeGeometry(20, 0, 0.8, "63% · resets 2h09m")
	got := string(g.cells)
	if !strings.HasPrefix(got, "63% · resets 2h09m") {
		t.Errorf("cells = %q, want text at the start", got)
	}
	// Longer than the channel: truncated with an ellipsis, never overflowing.
	g = gaugeGeometry(10, 0, 0.8, "63% · resets 2h09m")
	if len(g.cells) != 10 {
		t.Fatalf("overflow: %d cells", len(g.cells))
	}
	if string(g.cells[8:]) != "…s" && string(g.cells[9]) != "…" {
		// text is cut to width with ellipsis; last visible rune is the ellipsis
		if g.cells[9] != '…' {
			t.Errorf("cells = %q, want ellipsis at end", string(g.cells))
		}
	}
}

func TestGaugeSeverity(t *testing.T) {
	cases := []struct {
		u, th float64
		want  severity
	}{
		{0.10, 0.80, sevOK},
		{0.69, 0.80, sevOK},
		{0.72, 0.80, sevWarn}, // within a tenth of the redline
		{0.80, 0.80, sevBad},
		{0.95, 0.80, sevBad},
	}
	for _, c := range cases {
		if got := gaugeSeverity(c.u, c.th); got != c.want {
			t.Errorf("gaugeSeverity(%v,%v) = %v, want %v", c.u, c.th, got, c.want)
		}
	}
}

// Plain rendering (no styling available): fill must still be visible, as
// shading in the cells the text leaves blank, and the tick as a bar.
func TestGaugeRendersStructurallyWhenPlain(t *testing.T) {
	th := plainTheme()
	out := th.gauge(20, 0.5, 0.8, "50%")
	r := []rune(out)
	if len(r) != 20 {
		t.Fatalf("width = %d, want 20: %q", len(r), out)
	}
	if !strings.HasPrefix(out, "50%") {
		t.Errorf("text lost: %q", out)
	}
	if r[5] != '░' || r[9] != '░' {
		t.Errorf("fill shading missing in %q", out)
	}
	if r[10] == '░' {
		t.Errorf("fill overshoots in %q", out)
	}
	if r[16] != '|' {
		t.Errorf("tick missing at redline in %q", out)
	}
}

func TestGaugeRendersExactWidthWhenStyled(t *testing.T) {
	th := testTheme(t, "truecolor")
	out := th.gauge(20, 0.5, 0.8, "50%")
	if w := visibleWidth(out); w != 20 {
		t.Errorf("visible width = %d, want 20: %q", w, out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("styled gauge carries no escape sequences: %q", out)
	}
}
