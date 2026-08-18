package metrics

import (
	"context"
	"io"
	"log/slog"
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

// The roller used to do nothing until its first tick, so a process that lived
// less than one interval aggregated nothing at all.
func TestRollerAggregatesBeforeItsFirstTick(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	insertRaw(t, s, time.Now().Add(-time.Minute).UnixMilli(), "a", "opus", 42, 7, 0, 0, nil)

	// An interval long enough that no tick can fire during this test: only the
	// catch-up at Start can produce a bucket.
	r := NewRoller(s, time.Hour, quietLogger())
	r.Start()
	r.Stop()

	var reqs, in int64
	err := s.DB().QueryRow(`
SELECT sum(requests), sum(input_tokens) FROM usage_buckets WHERE granularity='minute'`).Scan(&reqs, &in)
	if err != nil {
		t.Fatalf("Start produced no rollup: %v", err)
	}
	if reqs != 1 || in != 42 {
		t.Errorf("= %d reqs / %d in, want 1/42", reqs, in)
	}
}

// A row older than the ticker's 2h lookback — written by a previous run — is
// exactly the case the periodic rollup can never reach.
func TestRollupCatchUpReachesRowsOlderThanTheTickerLookback(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour).UnixMilli()
	insertRaw(t, s, old, "a", "opus", 5, 1, 0, 0, nil)

	// The periodic path cannot see it.
	if err := RollupOnce(context.Background(), s, now, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&n)
	if n != 0 {
		t.Fatalf("precondition: the 2h lookback should not reach a 30-day-old row, got %d buckets", n)
	}

	if err := RollupCatchUp(context.Background(), s, now); err != nil {
		t.Fatalf("RollupCatchUp: %v", err)
	}
	var reqs int64
	if err := s.DB().QueryRow(`
SELECT sum(requests) FROM usage_buckets WHERE granularity='minute'`).Scan(&reqs); err != nil {
		t.Fatal(err)
	}
	if reqs != 1 {
		t.Errorf("requests = %d, want 1", reqs)
	}
}

// Catch-up over an empty store must not error or invent buckets.
func TestRollupCatchUpOnAnEmptyStoreIsANoOp(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	if err := RollupCatchUp(context.Background(), s, time.Now()); err != nil {
		t.Fatalf("RollupCatchUp on an empty store: %v", err)
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&n)
	if n != 0 {
		t.Errorf("buckets = %d, want 0", n)
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
