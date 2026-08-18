package metrics

import (
	"context"
	"testing"
	"time"
)

func seedForQueries(t *testing.T, s *Store, base int64) {
	t.Helper()
	insertRaw(t, s, base+1000, "acct-a", "claude-opus-5", 100, 10, 500, 5, nil)
	insertRaw(t, s, base+2000, "acct-a", "claude-sonnet-5", 200, 20, 0, 0, nil)
	insertRaw(t, s, base+3000, "acct-b", "claude-opus-5", 300, 30, 0, 0, nil)
	if err := RollupOnce(context.Background(), s, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestUsageSeriesGroupsByModel(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.UsageSeries(context.Background(), SeriesQuery{
		Window:      Window{From: base, To: base + 60000},
		Granularity: GranularityMinute,
		GroupBy:     GroupByModel,
	})
	if err != nil {
		t.Fatalf("UsageSeries: %v", err)
	}

	byKey := map[string]int64{}
	for _, p := range got.Points {
		byKey[p.Key] += p.InputTokens
	}
	if byKey["claude-opus-5"] != 400 {
		t.Errorf("opus input = %d, want 400", byKey["claude-opus-5"])
	}
	if byKey["claude-sonnet-5"] != 200 {
		t.Errorf("sonnet input = %d, want 200", byKey["claude-sonnet-5"])
	}
}

func TestUsageSeriesGroupsByAccount(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.UsageSeries(context.Background(), SeriesQuery{
		Window:      Window{From: base, To: base + 60000},
		Granularity: GranularityMinute,
		GroupBy:     GroupByAccount,
	})
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]int64{}
	for _, p := range got.Points {
		byKey[p.Key] += p.Requests
	}
	if byKey["acct-a"] != 2 || byKey["acct-b"] != 1 {
		t.Errorf("requests by account = %v, want acct-a 2 / acct-b 1", byKey)
	}
}

// The window must actually bound the result, or every chart silently shows all
// of history.
func TestUsageSeriesRespectsTheWindow(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.UsageSeries(context.Background(), SeriesQuery{
		Window:      Window{From: base + 3_600_000, To: base + 7_200_000},
		Granularity: GranularityMinute,
		GroupBy:     GroupByModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) != 0 {
		t.Errorf("got %d points outside the window, want 0", len(got.Points))
	}
}

func TestTotalsSumsTheWindow(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedForQueries(t, s, base)

	got, err := s.Totals(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 3 || got.InputTokens != 600 || got.OutputTokens != 60 {
		t.Errorf("totals = %+v, want 3 requests / 600 in / 60 out", got)
	}
	if got.CacheReadTokens != 500 {
		t.Errorf("cache read = %d, want 500", got.CacheReadTokens)
	}
}

func TestLatencyPercentiles(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()

	// ttfb 10..1000 so p50 and p95 are clearly distinguishable.
	for i := 1; i <= 100; i++ {
		if _, err := s.DB().Exec(`
INSERT INTO requests (started_at, account_id, provider, status, outcome, ttfb_ms, duration_ms)
VALUES (?,?,?,?,?,?,?)`, base+int64(i), "a", "anthropic", 200, "ok", i*10, i*20); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.LatencyPercentiles(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if got.TTFBP50 < 400 || got.TTFBP50 > 600 {
		t.Errorf("TTFB p50 = %d, want ~500", got.TTFBP50)
	}
	if got.TTFBP95 < 900 || got.TTFBP95 > 1000 {
		t.Errorf("TTFB p95 = %d, want ~950", got.TTFBP95)
	}
	if got.TTFBP95 <= got.TTFBP50 {
		t.Error("p95 must exceed p50")
	}
}

// A request that never produced a first byte records ttfb_ms = -1; including it
// would drag the percentile toward zero and hide a real latency problem.
func TestLatencyPercentilesIgnoreRequestsWithNoFirstByte(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()

	s.DB().Exec(`INSERT INTO requests (started_at, account_id, provider, status, outcome, ttfb_ms)
	             VALUES (?,?,?,?,?,?)`, base+1, "a", "anthropic", 200, "ok", 400)
	s.DB().Exec(`INSERT INTO requests (started_at, account_id, provider, status, outcome, ttfb_ms)
	             VALUES (?,?,?,?,?,?)`, base+2, "a", "anthropic", 429, "throttled_no_hint", -1)

	got, err := s.LatencyPercentiles(context.Background(), Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if got.TTFBP50 != 400 {
		t.Errorf("p50 = %d, want 400 — a request with no first byte must not be counted", got.TTFBP50)
	}
}

func TestAccountQuotaHistory(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()

	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization, resets_at)
	             VALUES (?,?,?,?,?)`, base+1000, "acct-a", "5h", 0.10, base+9999)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization, resets_at)
	             VALUES (?,?,?,?,?)`, base+2000, "acct-a", "5h", 0.35, base+9999)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization, resets_at)
	             VALUES (?,?,?,?,?)`, base+3000, "acct-b", "5h", 0.90, base+9999)

	got, err := s.AccountQuotaHistory(context.Background(), "acct-a", Window{From: base, To: base + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d points, want 2 (only acct-a)", len(got))
	}
	if got[0].At > got[1].At {
		t.Error("history must be ordered oldest first")
	}
	if got[1].Utilization != 0.35 {
		t.Errorf("latest utilization = %v, want 0.35", got[1].Utilization)
	}
}
