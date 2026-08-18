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
