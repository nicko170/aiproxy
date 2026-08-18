package updater

import "testing"

// TestCompare pins the ordering rules the header and the install path both
// depend on. 0.10.0 vs 0.9.0 is the case a string comparison gets wrong, and
// it is the reason this function exists at all.
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"0.1.0", "v0.1.0", 0},
		{"0.2.0", "0.1.0", 1},
		{"0.1.0", "0.2.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.1.10", "0.1.9", 1},
		// A prerelease sorts below its release.
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		{"1.0.0-rc1", "1.0.0-rc1", 0},
		// A prerelease of a higher version still beats a lower release.
		{"1.1.0-rc1", "1.0.0", 1},
		// Unparseable sorts below anything parseable, and equals itself.
		{"dev", "0.1.0", -1},
		{"0.1.0", "dev", 1},
		{"dev", "dev", 0},
		{"", "0.1.0", -1},
		{"1.2", "1.2.0", -1},
		{"1.2.x", "1.2.0", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
