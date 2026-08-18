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

// Store lets a caller that only holds an *Ingester (cmd/aiproxy's OnResult
// wiring) build a query-only consumer, such as view.Local, over the same
// database without threading a second *Store parameter through separately.
func TestIngesterStoreReturnsTheUnderlyingStore(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	defer ing.Close()

	if ing.Store() != s {
		t.Error("Store() did not return the same *Store the ingester was built with")
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
//
// The writer must be genuinely prevented from draining the channel, not merely
// slowed. The writer goroutine's for-select loop drains i.ch continuously
// whether or not a batch is ever flushed to the store, so a large FlushInterval
// alone does not stop it from keeping the buffer empty — whether the buffer
// ever truly filled during a 500-Record burst came down to a scheduler race,
// which made `Dropped() == 0` a flaky gate on the very invariant this test
// exists to enforce. Holding the writer goroutine back from starting at all,
// until after every Record has returned, makes the fill deterministic.
func TestRecordNeverBlocksWhenTheBufferIsFull(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := newIngester(s, IngestOptions{BufferSize: 4, FlushInterval: time.Hour, BatchSize: 1000})
	hold := make(chan struct{})
	go func() {
		<-hold
		ing.run()
	}()
	defer func() {
		close(hold)
		if err := ing.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

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

	// The writer has not even started yet at this point (still parked on hold),
	// so every Record beyond the 4-entry buffer MUST have dropped.
	if want := int64(500 - 4); ing.Dropped() != want {
		t.Errorf("Dropped() = %d, want %d — the 4-entry buffer should have overflowed "+
			"deterministically with the writer held back", ing.Dropped(), want)
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

// The strongest possible violation of invariant 3: a panic in the batch write
// path — a SQLite driver bug, a nil dereference — must not take the writer
// goroutine, and with it the whole process and every in-flight proxied
// request, down. It must be recovered, the faulting batch dropped and
// counted, and the writer must keep running for the next one.
//
// writeFunc is substituted BEFORE the writer goroutine starts (via newIngester
// rather than NewIngester) so the override is visible without a data race:
// nothing else touches the field once run() begins reading it.
func TestWriterSurvivesAPanicInTheBatchWriteAndKeepsRunning(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := newIngester(s, IngestOptions{})
	real := ing.writeFunc
	var calls int
	ing.writeFunc = func(batch []entry) error {
		calls++
		if calls == 1 {
			panic("simulated SQLite driver panic")
		}
		return real(batch)
	}
	go ing.run()
	defer ing.Close()

	// The first batch faults. It must not crash the writer, and it must be
	// counted rather than silently lost.
	ing.Record(sample(1, "a", "m", 1, 1))
	if err := ing.Flush(); err == nil {
		t.Error("Flush should surface the recovered panic as an error")
	}
	if ing.Dropped() == 0 {
		t.Error("the panicking batch must be counted as dropped, not lost invisibly")
	}

	// The writer must still be alive to serve a later, healthy sample.
	ing.Record(sample(2, "a", "m", 2, 2))
	if err := ing.Flush(); err != nil {
		t.Fatalf("writer did not survive the panic: %v", err)
	}

	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1 — only the sample recorded after the panic should have landed", n)
	}
}

// Spec §7.3: degradation must be visible, not invisible. Once the writer has
// stopped, a Record must not sit forever in the channel's own buffer,
// unwritten and uncounted — it must be visibly dropped, exactly like a Record
// against a full buffer.
func TestRecordAfterCloseCountsAsDropped(t *testing.T) {
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ing := NewIngester(s, IngestOptions{})
	if err := ing.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := ing.Dropped()
	ing.Record(sample(1, "a", "m", 1, 1))
	ing.RecordQuota(QuotaSample{At: 1, AccountID: "a", Bucket: "5h", Utilization: 0.1})

	if got, want := ing.Dropped(), before+2; got != want {
		t.Errorf("Dropped() = %d, want %d — Record and RecordQuota after Close must count as drops",
			got, want)
	}

	var n int
	s.DB().QueryRow(`SELECT count(*) FROM requests`).Scan(&n)
	if n != 0 {
		t.Errorf("rows = %d, want 0 — nothing recorded after Close should ever be written", n)
	}
}
