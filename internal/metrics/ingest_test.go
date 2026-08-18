package metrics

import (
	"sync"
	"testing"
	"time"
)

func sample(at int64, acct, model string, in, out int64) Sample {
	return Sample{
		StartedAt: at, DurationMS: 10, TTFBMS: 5,
		AccountID: acct, Provider: "anthropic", Model: model,
		Endpoint: "/v1/messages", Status: 200, Outcome: "ok",
		Stream: true, Attempts: 1,
		InputTokens: in, OutputTokens: out,
	}
}

func drainInto(t *testing.T, s *Store, ing *Ingester) {
	t.Helper()
	if err := ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func TestRecordPersistsRows(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	defer ing.Close()

	ing.Record(sample(1000, "acct-1", "claude-opus-5", 100, 20))
	ing.Record(sample(2000, "acct-1", "claude-opus-5", 50, 10))
	drainInto(t, s, ing)

	var n int
	var totalIn, totalOut int64
	if err := s.DB().QueryRow(
		`SELECT count(*), sum(input_tokens), sum(output_tokens) FROM requests`).
		Scan(&n, &totalIn, &totalOut); err != nil {
		t.Fatal(err)
	}
	if n != 2 || totalIn != 150 || totalOut != 30 {
		t.Errorf("rows=%d in=%d out=%d, want 2/150/30", n, totalIn, totalOut)
	}
}

// Invariant 3: Record must never block, even when the writer cannot keep up.
// A full buffer drops and counts, it does not wait.
func TestRecordNeverBlocksWhenTheBufferIsFull(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A tiny buffer and a writer that is not running: every Record after the
	// buffer fills must drop rather than block.
	ing := NewIngester(s, IngestOptions{BufferSize: 4, FlushInterval: time.Hour, BatchSize: 1000})
	defer ing.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			ing.Record(sample(int64(i), "a", "m", 1, 1))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked — accounting must never delay a proxied request")
	}

	if ing.Dropped() == 0 {
		t.Error("expected drops to be counted when the buffer overflows")
	}
}

func TestRecordQuotaPersistsSamples(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	defer ing.Close()

	ing.RecordQuota(QuotaSample{At: 1000, AccountID: "a", Bucket: "5h", Utilization: 0.42, ResetsAt: 9999})
	drainInto(t, s, ing)

	var util float64
	if err := s.DB().QueryRow(
		`SELECT utilization FROM quota_samples WHERE account_id='a' AND bucket='5h'`).Scan(&util); err != nil {
		t.Fatal(err)
	}
	if util != 0.42 {
		t.Errorf("utilization = %v, want 0.42", util)
	}
}

// The same (at, account, bucket) arriving twice must not fail the whole batch.
func TestRecordQuotaToleratesDuplicates(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	defer ing.Close()

	q := QuotaSample{At: 1000, AccountID: "a", Bucket: "5h", Utilization: 0.5}
	ing.RecordQuota(q)
	ing.RecordQuota(q)
	if err := ing.Flush(); err != nil {
		t.Fatalf("a duplicate quota sample must not fail the batch: %v", err)
	}

	var n int
	s.DB().QueryRow(`SELECT count(*) FROM quota_samples`).Scan(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

func TestConcurrentRecordersAllLand(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{BufferSize: 4096})
	defer ing.Close()

	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				ing.Record(sample(int64(w*1000+i), "a", "m", 1, 1))
			}
		}(w)
	}
	wg.Wait()
	drainInto(t, s, ing)

	var n int64
	s.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&n)
	if n+ing.Dropped() != writers*each {
		t.Errorf("persisted %d + dropped %d != %d recorded", n, ing.Dropped(), writers*each)
	}
}
