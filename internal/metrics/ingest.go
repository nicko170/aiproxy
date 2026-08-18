package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// IngestOptions tunes the writer. Zero values take defaults.
type IngestOptions struct {
	BufferSize    int
	FlushInterval time.Duration
	BatchSize     int
	// Log receives the writer's own diagnostics: a batch that failed or
	// panicked, dropped rather than fatal. Nil takes slog.Default().
	Log *slog.Logger
}

type entry struct {
	sample *Sample
	quota  *QuotaSample
}

// Ingester accepts samples without ever blocking the caller and writes them in
// batches on a single goroutine.
//
// Spec invariant 3: accounting must never block, delay, or fail a proxied
// request. That is why Record does a non-blocking send and increments a drop
// counter when the buffer is full. Dropping accounting data is a real cost, but
// it is a smaller one than adding latency to the request path, and a visible
// drop count is honest in a way that silent backpressure is not.
type Ingester struct {
	store   *Store
	ch      chan entry
	opts    IngestOptions
	log     *slog.Logger
	dropped atomic.Int64

	// writeFunc performs one batch write. It defaults to (*Ingester).write and is
	// a field — rather than a direct call to that method — solely so a test can
	// substitute a fault and prove the writer survives a panic in the batch path
	// without depending on a real SQLite failure to produce one.
	writeFunc func([]entry) error

	// flushed signals the writer has drained everything queued before it.
	flushReq chan chan error

	closeOnce sync.Once
	done      chan struct{}
	stopped   chan struct{}
}

// NewIngester builds the ingester and starts its writer goroutine.
func NewIngester(s *Store, opts IngestOptions) *Ingester {
	ing := newIngester(s, opts)
	go ing.run()
	return ing
}

// newIngester builds the ingester WITHOUT starting its writer goroutine, so a
// test can arrange for the writer to start later — or substitute writeFunc
// first — without racing the goroutine NewIngester would otherwise launch
// immediately.
func newIngester(s *Store, opts IngestOptions) *Ingester {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 4096
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 200 * time.Millisecond
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 100
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	ing := &Ingester{
		store:    s,
		ch:       make(chan entry, opts.BufferSize),
		opts:     opts,
		log:      log,
		flushReq: make(chan chan error, 1),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	ing.writeFunc = ing.write
	return ing
}

// Record queues a completed request. It never blocks: a full buffer drops the
// sample and increments the drop counter — and so does a send after the writer
// has already stopped, which would otherwise land in the channel's own buffer
// and sit there forever, uncounted and never written. The stopped check is
// deliberately tried FIRST: still non-blocking (both arms have a default), but
// it is what keeps a post-Close sample from being silently swallowed instead of
// visibly dropped (spec §7.3).
func (i *Ingester) Record(s Sample) {
	select {
	case <-i.stopped:
		i.dropped.Add(1)
		return
	default:
	}
	select {
	case i.ch <- entry{sample: &s}:
	default:
		i.dropped.Add(1)
	}
}

// RecordQuota queues a quota observation under the same no-blocking,
// stopped-checked-first contract as Record.
func (i *Ingester) RecordQuota(q QuotaSample) {
	select {
	case <-i.stopped:
		i.dropped.Add(1)
		return
	default:
	}
	select {
	case i.ch <- entry{quota: &q}:
	default:
		i.dropped.Add(1)
	}
}

// Dropped is the number of samples discarded because the buffer was full.
func (i *Ingester) Dropped() int64 { return i.dropped.Load() }

// Store returns the accounting database the ingester writes into, so a
// caller that already holds an Ingester (cmd/aiproxy does, for wiring
// OnResult) does not also need to thread the *Store through separately just
// to build a query-only consumer like view.Local.
func (i *Ingester) Store() *Store { return i.store }

// Flush blocks until everything queued at call time has been written.
func (i *Ingester) Flush() error {
	reply := make(chan error, 1)
	select {
	case i.flushReq <- reply:
	case <-i.stopped:
		return nil
	}
	select {
	case err := <-reply:
		return err
	case <-i.stopped:
		return nil
	}
}

// Close flushes and stops the writer.
func (i *Ingester) Close() error {
	var err error
	i.closeOnce.Do(func() {
		err = i.Flush()
		close(i.done)
		<-i.stopped
	})
	return err
}

func (i *Ingester) run() {
	defer close(i.stopped)
	ticker := time.NewTicker(i.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]entry, 0, i.opts.BatchSize)
	writeBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		n := len(batch)
		err := i.writeRecovered(batch)
		batch = batch[:0]
		// Every caller of writeBatch — the shutdown path, the periodic ticker,
		// and Flush — used to discard this error. A whole batch silently missing
		// is exactly the kind of gap that only shows up much later as unexplained
		// holes in the accounting; logging it here means it doesn't have to.
		if err != nil {
			i.log.Error("metrics batch write failed; batch dropped", "err", err, "batchSize", n)
		}
		return err
	}

	for {
		select {
		case <-i.done:
			i.drain(&batch)
			writeBatch()
			return

		case reply := <-i.flushReq:
			i.drain(&batch)
			reply <- writeBatch()

		case <-ticker.C:
			writeBatch()

		case e := <-i.ch:
			batch = append(batch, e)
			if len(batch) >= i.opts.BatchSize {
				writeBatch()
			}
		}
	}
}

// drain moves everything currently queued into the batch without waiting.
func (i *Ingester) drain(batch *[]entry) {
	for {
		select {
		case e := <-i.ch:
			*batch = append(*batch, e)
		default:
			return
		}
	}
}

// writeRecovered runs writeFunc (ordinarily (*Ingester).write) with a panic
// recovered.
//
// Spec invariant 3 makes the ingester the one component whose entire job is
// protecting the request path; an unrecovered panic here — a SQLite driver
// bug, a nil dereference somewhere in the batch path — would otherwise take
// the whole process down, and with it every in-flight proxied request. A
// batch that faults this way cannot be salvaged, so it is dropped and counted
// rather than fatal: run's for-select loop, and the writer goroutine, keep
// going for the next one.
func (i *Ingester) writeRecovered(batch []entry) (err error) {
	defer func() {
		if r := recover(); r != nil {
			i.dropped.Add(int64(len(batch)))
			err = fmt.Errorf("metrics writer recovered from panic: %v", r)
		}
	}()
	return i.writeFunc(batch)
}

func (i *Ingester) write(batch []entry) error {
	ctx := context.Background()
	tx, err := i.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metrics tx: %w", err)
	}
	defer tx.Rollback()

	reqStmt, err := tx.PrepareContext(ctx, `
INSERT INTO requests (started_at, duration_ms, ttfb_ms, wait_ms, account_id, provider,
  model, upstream_model, session_id, endpoint, status, outcome, stream, attempts, rotated,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare request insert: %w", err)
	}
	defer reqStmt.Close()

	// A repeated (at, account, bucket) is an ordinary consequence of polling on
	// a fixed interval; it must not fail the batch and take unrelated request
	// rows down with it.
	quotaStmt, err := tx.PrepareContext(ctx, `
INSERT OR REPLACE INTO quota_samples (at, account_id, bucket, utilization, resets_at)
VALUES (?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare quota insert: %w", err)
	}
	defer quotaStmt.Close()

	for _, e := range batch {
		switch {
		case e.sample != nil:
			s := e.sample
			var cost any
			if s.CostMicros != nil {
				cost = *s.CostMicros
			}
			if _, err := reqStmt.ExecContext(ctx,
				s.StartedAt, s.DurationMS, s.TTFBMS, s.WaitMS, s.AccountID, s.Provider,
				nullIfEmpty(s.Model), nullIfEmpty(s.UpstreamModel), nullIfEmpty(s.SessionID),
				nullIfEmpty(s.Endpoint), s.Status, s.Outcome, s.Stream, s.Attempts, s.Rotated,
				s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens, cost,
			); err != nil {
				return fmt.Errorf("insert request row: %w", err)
			}
		case e.quota != nil:
			q := e.quota
			if _, err := quotaStmt.ExecContext(ctx,
				q.At, q.AccountID, q.Bucket, q.Utilization, nullIfZero(q.ResetsAt),
			); err != nil {
				return fmt.Errorf("insert quota row: %w", err)
			}
		}
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
