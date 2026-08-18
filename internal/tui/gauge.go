package tui

import "math"

// The gauge is this UI's signature element: a channel of exactly width
// cells whose text — utilization, and the reset countdown spec §8 wants
// inside the bar — flows through it, with the used portion painted over the
// text in reverse video and the rotation threshold marked as a fixed tick.
// The tick is the point of the whole instrument: the proxy rotates accounts
// at switchThreshold, so every quota bar shows the redline it will rotate
// at, and the fill visibly approaches it.
//
// Degradation is structural, not cosmetic: with no styling available the
// fill becomes ░ shading in the cells the text leaves blank and the tick a
// plain bar, so the geometry survives NO_COLOR and dumb terminals intact.

// gaugeGeom is the pure geometry of one gauge: the padded cell runes, how
// many leading cells are filled, and the tick's cell index (-1 for none).
type gaugeGeom struct {
	cells []rune
	fill  int
	tick  int
}

// gaugeGeometry computes gauge cells for a channel of width cells at
// utilization u with the redline at threshold. Utilization clamps to [0, 1]
// — a >100% reading fills the channel, it does not overflow it — and any
// non-zero utilization fills at least one cell, because "in use" must never
// render as "untouched".
func gaugeGeometry(width int, u, threshold float64, text string) gaugeGeom {
	if width <= 0 {
		return gaugeGeom{cells: []rune{}, tick: -1}
	}
	cells := []rune(padRight(text, width))

	cu := math.Max(0, math.Min(1, u))
	fill := int(math.Round(cu * float64(width)))
	if u > 0 && fill == 0 {
		fill = 1
	}

	tick := -1
	if threshold > 0 && threshold < 1 {
		tick = int(threshold * float64(width))
		if tick >= width {
			tick = width - 1
		}
	}
	return gaugeGeom{cells: cells, fill: fill, tick: tick}
}

// gaugeSeverity grades utilization against the redline: bad at or past it,
// warn within a tenth of it, ok below that.
func gaugeSeverity(u, threshold float64) severity {
	switch {
	case threshold > 0 && u >= threshold:
		return sevBad
	case threshold > 0 && u >= threshold-0.1:
		return sevWarn
	default:
		return sevOK
	}
}

// gauge renders one channel. Styled mode paints the filled cells in reverse
// video (severity-coloured) and dims the unfilled remainder; plain mode
// shades blank filled cells with ░ and draws the tick as |. The tick never
// overwrites a text glyph and disappears once the fill has passed it — the
// fill's colour has already said "past the redline" louder than a tick can.
func (t theme) gauge(width int, u, threshold float64, text string) string {
	g := gaugeGeometry(width, u, threshold, text)
	if len(g.cells) == 0 {
		return ""
	}
	sev := gaugeSeverity(u, threshold)

	if t.mode == modeNone {
		for i := 0; i < g.fill; i++ {
			if g.cells[i] == ' ' {
				g.cells[i] = '░'
			}
		}
		if g.tick >= g.fill && g.cells[g.tick] == ' ' {
			g.cells[g.tick] = '|'
		}
		return string(g.cells)
	}

	var b []byte
	b = append(b, t.fill(sev, string(g.cells[:g.fill]))...)
	if g.tick >= g.fill {
		b = append(b, t.dim(string(g.cells[g.fill:g.tick]))...)
		if g.cells[g.tick] == ' ' {
			b = append(b, t.bad("▏")...)
		} else {
			b = append(b, t.dim(string(g.cells[g.tick]))...)
		}
		b = append(b, t.dim(string(g.cells[g.tick+1:]))...)
	} else {
		b = append(b, t.dim(string(g.cells[g.fill:]))...)
	}
	return string(b)
}
