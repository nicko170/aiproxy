package tui

import "testing"

func TestSparkBucketsSumsIntoCells(t *testing.T) {
	// Window [0, 60000): 6 cells of 10s each.
	samples := []sparkSample{
		{At: 0, V: 2},
		{At: 5_000, V: 1},  // same cell as above
		{At: 10_000, V: 4}, // second cell
		{At: 59_999, V: 7}, // last cell
		{At: 60_000, V: 9}, // outside the window: ignored
		{At: -1, V: 9},     // before the window: ignored
	}
	got := sparkBuckets(samples, 0, 60_000, 6)
	want := []float64{3, 4, 0, 0, 0, 7}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cell %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestSparkBucketsEmptyAndDegenerate(t *testing.T) {
	if got := sparkBuckets(nil, 0, 60_000, 6); len(got) != 6 {
		t.Errorf("nil samples: len = %d, want 6", len(got))
	}
	if got := sparkBuckets(nil, 0, 60_000, 0); got != nil {
		t.Errorf("zero cells: got %v, want nil", got)
	}
	if got := sparkBuckets(nil, 60_000, 60_000, 6); len(got) != 6 {
		t.Errorf("empty window: len = %d, want 6", len(got))
	}
}

func TestSparklineScalesToItsOwnMax(t *testing.T) {
	got := sparkline([]float64{0, 1, 4, 8})
	want := "·▁▄█"
	if got != want {
		t.Errorf("sparkline = %q, want %q", got, want)
	}
}

func TestSparklineAllZeroIsAFlatBaseline(t *testing.T) {
	got := sparkline([]float64{0, 0, 0})
	if got != "···" {
		t.Errorf("sparkline = %q, want %q", got, "···")
	}
}

func TestSparklineSmallestNonZeroIsVisible(t *testing.T) {
	// 1 against a max of 1000 must still render a mark, not a baseline dot:
	// a request happened, and the sparkline saying "nothing" would be a lie.
	got := []rune(sparkline([]float64{1, 1000}))
	if got[0] != '▁' {
		t.Errorf("smallest mark = %q, want ▁", string(got[0]))
	}
	if got[1] != '█' {
		t.Errorf("max mark = %q, want █", string(got[1]))
	}
}
