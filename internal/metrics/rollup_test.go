package metrics

import (
	"context"
	"testing"
	"time"
)

func insertRaw(t *testing.T, s *Store, at int64, acct, model string, in, out, cr, cw int64, cost *int64) {
	t.Helper()
	var c any
	if cost != nil {
		c = *cost
	}
	_, err := s.DB().Exec(`
INSERT INTO requests (started_at, account_id, provider, model, status, outcome,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		at, acct, "anthropic", model, 200, "ok", in, out, cr, cw, c)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRollupAggregatesByMinuteAndHour(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC).UnixMilli()
	insertRaw(t, s, base+1000, "a", "opus", 100, 10, 5, 1, nil)
	insertRaw(t, s, base+2000, "a", "opus", 200, 20, 5, 1, nil)
	insertRaw(t, s, base+61000, "a", "opus", 7, 3, 0, 0, nil) // next minute, same hour

	now := time.UnixMilli(base + 120000)
	if err := RollupOnce(context.Background(), s, now, time.Hour); err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}

	var reqs, in, out int64
	err := s.DB().QueryRow(`
SELECT requests, input_tokens, output_tokens FROM usage_buckets
WHERE granularity='minute' AND bucket_start=? AND account_id='a' AND model='opus'`,
		base).Scan(&reqs, &in, &out)
	if err != nil {
		t.Fatalf("minute bucket: %v", err)
	}
	if reqs != 2 || in != 300 || out != 30 {
		t.Errorf("minute bucket = %d reqs / %d in / %d out, want 2/300/30", reqs, in, out)
	}

	err = s.DB().QueryRow(`
SELECT requests, input_tokens FROM usage_buckets
WHERE granularity='hour' AND bucket_start=? AND account_id='a' AND model='opus'`,
		base).Scan(&reqs, &in)
	if err != nil {
		t.Fatalf("hour bucket: %v", err)
	}
	if reqs != 3 || in != 307 {
		t.Errorf("hour bucket = %d reqs / %d in, want 3/307", reqs, in)
	}
}

// Running twice must not double-count. The aggregator recomputes a bucket from
// the raw rows rather than incrementing it, which is what makes it safe to run
// on a timer, after a crash, or twice by accident.
func TestRollupIsIdempotent(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 11, 0, 0, 0, time.UTC).UnixMilli()
	insertRaw(t, s, base+500, "a", "opus", 50, 5, 0, 0, nil)

	now := time.UnixMilli(base + 60000)
	for i := 0; i < 3; i++ {
		if err := RollupOnce(context.Background(), s, now, time.Hour); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	var reqs, in int64
	s.DB().QueryRow(`
SELECT requests, input_tokens FROM usage_buckets
WHERE granularity='minute' AND account_id='a' AND model='opus'`).Scan(&reqs, &in)
	if reqs != 1 || in != 50 {
		t.Errorf("after 3 runs: %d reqs / %d in, want 1/50 — rollup is not idempotent", reqs, in)
	}
}

// A request whose model was never identified must still be accounted for, not
// silently vanish into a NULL primary-key column.
func TestRollupCountsRowsWithNoModel(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC).UnixMilli()
	_, err := s.DB().Exec(`
INSERT INTO requests (started_at, account_id, provider, status, outcome,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
VALUES (?,?,?,?,?,?,?,?,?)`, base+100, "a", "anthropic", 200, "ok", 9, 4, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := RollupOnce(context.Background(), s, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatal(err)
	}

	var reqs, in int64
	if err := s.DB().QueryRow(`
SELECT requests, input_tokens FROM usage_buckets
WHERE granularity='minute' AND account_id='a' AND model=''`).Scan(&reqs, &in); err != nil {
		t.Fatalf("a row with no model should aggregate under '': %v", err)
	}
	if reqs != 1 || in != 9 {
		t.Errorf("= %d reqs / %d in, want 1/9", reqs, in)
	}
}

func TestRollupSumsCostAndToleratesUnpriced(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	base := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC).UnixMilli()
	priced := int64(1500)
	insertRaw(t, s, base+100, "a", "opus", 1, 1, 0, 0, &priced)
	insertRaw(t, s, base+200, "a", "opus", 1, 1, 0, 0, nil) // unpriced

	if err := RollupOnce(context.Background(), s, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatal(err)
	}

	var cost int64
	if err := s.DB().QueryRow(`
SELECT cost_micros FROM usage_buckets
WHERE granularity='minute' AND account_id='a' AND model='opus'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 1500 {
		t.Errorf("cost = %d, want 1500 — an unpriced row contributes nothing, it does not void the bucket", cost)
	}
}
