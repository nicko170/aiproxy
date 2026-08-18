package tui

// sparkSample is one (time, value) observation for a sparkline.
type sparkSample struct {
	At int64 // unix ms
	V  float64
}

// sparkBuckets sums samples into cells equal slices of [from, to). Samples
// outside the window are ignored. A degenerate window (to <= from) yields
// all-zero cells rather than dividing by zero.
func sparkBuckets(samples []sparkSample, from, to int64, cells int) []float64 {
	if cells <= 0 {
		return nil
	}
	out := make([]float64, cells)
	span := to - from
	if span <= 0 {
		return out
	}
	for _, s := range samples {
		if s.At < from || s.At >= to {
			continue
		}
		i := int((s.At - from) * int64(cells) / span)
		if i >= cells { // guard the exact upper edge under integer math
			i = cells - 1
		}
		out[i] += s.V
	}
	return out
}

// sparkLevels are the eight block heights. Zero renders as · — a baseline
// dot, not an empty cell, so "no traffic" is visibly a measurement of
// nothing rather than a gap in the instrument.
var sparkLevels = []rune("▁▂▃▄▅▆▇█")

// sparkline renders values scaled to their own maximum. The smallest
// non-zero value always gets the lowest mark: a request happened, and a
// sparkline rounding it to "nothing" would be a lie.
func sparkline(vals []float64) string {
	var max float64
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	out := make([]rune, len(vals))
	for i, v := range vals {
		switch {
		case v <= 0 || max <= 0:
			out[i] = '·'
		default:
			idx := int(v / max * float64(len(sparkLevels)))
			if float64(idx) != v/max*float64(len(sparkLevels)) {
				idx++ // ceil
			}
			if idx < 1 {
				idx = 1
			}
			if idx > len(sparkLevels) {
				idx = len(sparkLevels)
			}
			out[i] = sparkLevels[idx-1]
		}
	}
	return string(out)
}
