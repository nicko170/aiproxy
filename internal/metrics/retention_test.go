package metrics

import (
	"context"
	"testing"
	"time"
)

func TestPruneRemovesOldRawRowsButKeepsRollups(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-100 * 24 * time.Hour).UnixMilli()
	recent := now.Add(-1 * time.Hour).UnixMilli()

	insertRaw(t, s, old, "a", "opus", 10, 1, 0, 0, nil)
	insertRaw(t, s, recent, "a", "opus", 20, 2, 0, 0, nil)
	if err := RollupOnce(context.Background(), s, now, 200*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	var bucketsBefore int
	s.DB().QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&bucketsBefore)

	deleted, err := PruneOnce(context.Background(), s, now, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	var raw int
	s.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&raw)
	if raw != 1 {
		t.Errorf("raw rows = %d, want 1 (the recent one)", raw)
	}

	var bucketsAfter int
	s.DB().QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&bucketsAfter)
	if bucketsAfter != bucketsBefore {
		t.Errorf("rollups = %d, want %d — rollups are never pruned; they are what makes a long window cheap",
			bucketsAfter, bucketsBefore)
	}
}

func TestPruneAlsoTrimsQuotaSamples(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization) VALUES (?,?,?,?)`,
		now.Add(-100*24*time.Hour).UnixMilli(), "a", "5h", 0.5)
	s.DB().Exec(`INSERT INTO quota_samples (at, account_id, bucket, utilization) VALUES (?,?,?,?)`,
		now.Add(-time.Hour).UnixMilli(), "a", "5h", 0.6)

	if _, err := PruneOnce(context.Background(), s, now, 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	var n int
	s.DB().QueryRow(`SELECT count(*) FROM quota_samples`).Scan(&n)
	if n != 1 {
		t.Errorf("quota samples = %d, want 1", n)
	}
}

// Retention of zero or less means keep everything — an operator who clears the
// setting must not silently lose their history.
func TestPruneWithZeroRetentionKeepsEverything(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	insertRaw(t, s, now.Add(-1000*24*time.Hour).UnixMilli(), "a", "opus", 1, 1, 0, 0, nil)

	deleted, err := PruneOnce(context.Background(), s, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 when retention is disabled", deleted)
	}
}

// Spec invariant 4: one number, one source. A row that is written and then
// pruned before any rollup tick reaches it must still be counted in the
// rollups, or UsageSeries and Totals disagree permanently and silently — the
// raw row is gone and nothing will ever aggregate it again.
func TestPrunedRowsAreRolledUpBeforeTheyAreDeleted(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	at := now.Add(-48 * time.Hour).UnixMilli()
	insertRaw(t, s, at, "a", "opus", 111, 22, 3, 4, nil)

	// No rollup has ever run: this is the state after a process that died
	// before its first tick, or a window a later run's 2h lookback never covered.
	var buckets int
	s.DB().QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&buckets)
	if buckets != 0 {
		t.Fatalf("precondition: %d buckets, want 0", buckets)
	}

	deleted, err := PruneOnce(context.Background(), s, now, 24*time.Hour)
	if err != nil {
		t.Fatalf("PruneOnce: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}

	var raw int
	s.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&raw)
	if raw != 0 {
		t.Fatalf("raw rows = %d, want 0 — the row should have been pruned", raw)
	}

	var reqs, in, out int64
	err = s.DB().QueryRow(`
SELECT sum(requests), sum(input_tokens), sum(output_tokens) FROM usage_buckets
WHERE granularity='hour' AND account_id='a' AND model='opus'`).Scan(&reqs, &in, &out)
	if err != nil {
		t.Fatalf("the pruned row left no rollup behind: %v", err)
	}
	if reqs != 1 || in != 111 || out != 22 {
		t.Errorf("rollup = %d reqs / %d in / %d out, want 1/111/22", reqs, in, out)
	}
}

// The bucket straddling the cutoff must be recomputed while BOTH sides still
// exist, or it silently records only the rows that happened to survive.
func TestTheBucketStraddlingTheCutoffKeepsItsPrunedRows(t *testing.T) {
	s, _ := OpenMemory()
	defer s.Close()

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-24 * time.Hour)
	insertRaw(t, s, cutoff.Add(-time.Minute).UnixMilli(), "a", "opus", 10, 1, 0, 0, nil) // pruned
	insertRaw(t, s, cutoff.Add(time.Minute).UnixMilli(), "a", "opus", 20, 2, 0, 0, nil)  // kept

	if _, err := PruneOnce(context.Background(), s, now, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	var reqs, in int64
	err := s.DB().QueryRow(`
SELECT sum(requests), sum(input_tokens) FROM usage_buckets
WHERE granularity='hour' AND account_id='a' AND model='opus'`).Scan(&reqs, &in)
	if err != nil {
		t.Fatal(err)
	}
	if reqs != 2 || in != 30 {
		t.Errorf("hour rollups = %d reqs / %d in, want 2/30 — the pruned half of the boundary bucket was lost", reqs, in)
	}
}
