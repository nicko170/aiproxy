package rules

import "math"

// ShannonBits is the Shannon entropy of s in bits per byte, from 0 (one
// repeated byte) to 8 (uniform over all byte values). A random base64 run lands
// near 5-6; English prose near 3-4; a hand-written placeholder like "changeme"
// below 3.
//
// Computed over bytes rather than runes deliberately: the inputs this qualifies
// are credential-shaped, which is to say ASCII, and byte frequencies are what
// the thresholds in Builtin were chosen against.
func ShannonBits(s string) float64 {
	if len(s) < 2 {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var bits float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		bits -= p * math.Log2(p)
	}
	return bits
}
