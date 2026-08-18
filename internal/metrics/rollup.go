package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Granularity names a rollup grain.
type Granularity string

const (
	GranularityMinute Granularity = "minute"
	GranularityHour   Granularity = "hour"
)

func (g Granularity) millis() int64 {
	if g == GranularityHour {
		return 3600_000
	}
	return 60_000
}

// RollupOnce recomputes every bucket touched in the lookback window.
//
// Recompute, not increment: each bucket is derived in full from the raw rows it
// covers and written with INSERT OR REPLACE. That is what makes it safe to run
// on a timer, twice by accident, or again after a crash mid-write — an
// incrementing aggregator has none of those properties and drifts silently.
func RollupOnce(ctx context.Context, s *Store, now time.Time, lookback time.Duration) error {
	if lookback <= 0 {
		lookback = time.Hour
	}
	return rollupRange(ctx, s, now.Add(-lookback).UnixMilli(), now.UnixMilli())
}

// rollupRange recomputes every grain over an explicit [from, to) range. Split
// out of RollupOnce so the catch-up and pre-prune paths can name a range
// directly instead of expressing it as a lookback from "now".
func rollupRange(ctx context.Context, s *Store, from, to int64) error {
	for _, g := range []Granularity{GranularityMinute, GranularityHour} {
		if err := rollupGrain(ctx, s, g, from, to); err != nil {
			return fmt.Errorf("rollup %s: %w", g, err)
		}
	}
	return nil
}

// RollupCatchUp aggregates every raw row the periodic rollup can no longer
// reach, and is run once at Start before the first tick.
//
// The ticker only ever looks back 2h from now, so rows written by a previous
// run — or by a run shorter than one tick — fall outside every subsequent
// lookback and are never aggregated. UsageSeries (rollups) and Totals (raw)
// then disagree permanently and silently, which is spec invariant 4 ("one
// number, one source") not holding.
//
// The range is bounded by work actually outstanding rather than by a fixed
// window: it starts at the newest minute bucket already rolled up (recomputed
// in full, since a bucket may have been partial when it was written) or, if
// nothing has ever been rolled up, at the oldest raw row. An empty requests
// table is a no-op.
func RollupCatchUp(ctx context.Context, s *Store, now time.Time) error {
	var oldest sql.NullInt64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT min(started_at) FROM requests`).Scan(&oldest); err != nil {
		return fmt.Errorf("rollup catch-up: oldest raw row: %w", err)
	}
	if !oldest.Valid {
		return nil
	}

	var latest sql.NullInt64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT max(bucket_start) FROM usage_buckets WHERE granularity = ?`,
		string(GranularityMinute)).Scan(&latest); err != nil {
		return fmt.Errorf("rollup catch-up: newest bucket: %w", err)
	}

	from := oldest.Int64
	if latest.Valid && latest.Int64 > from {
		from = latest.Int64
	}
	return rollupRange(ctx, s, from, now.UnixMilli())
}

func rollupGrain(ctx context.Context, s *Store, g Granularity, from, to int64) error {
	span := g.millis()
	// Widen to whole buckets so a partially covered bucket is recomputed in
	// full rather than truncated to the window.
	start := (from / span) * span
	end := (to/span)*span + span

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// coalesce(model,'') because model is part of the primary key and SQLite
	// treats NULLs in a key as distinct — a NULL model would create a fresh row
	// per request instead of aggregating.
	_, err = tx.ExecContext(ctx, `
INSERT OR REPLACE INTO usage_buckets
  (bucket_start, granularity, account_id, model, requests,
   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_micros)
SELECT
  (started_at / ?) * ?            AS bucket_start,
  ?                               AS granularity,
  account_id,
  coalesce(model, '')             AS model,
  count(*)                        AS requests,
  coalesce(sum(input_tokens), 0),
  coalesce(sum(output_tokens), 0),
  coalesce(sum(cache_read_tokens), 0),
  coalesce(sum(cache_write_tokens), 0),
  sum(cost_micros)
FROM requests
WHERE started_at >= ? AND started_at < ?
GROUP BY bucket_start, account_id, coalesce(model, '')`,
		span, span, string(g), start, end)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Roller runs RollupOnce on a timer.
type Roller struct {
	store    *Store
	interval time.Duration
	log      *slog.Logger
	stop     chan struct{}
	stopped  chan struct{}
}

func NewRoller(s *Store, interval time.Duration, log *slog.Logger) *Roller {
	if interval <= 0 {
		interval = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Roller{
		store: s, interval: interval, log: log,
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
}

// Start begins the background rollup loop. Must be called exactly once,
// followed by exactly one Stop.
//
// Unlike prober.Prober's Start/Stop (which guard themselves with sync.Once,
// making a duplicate call a silent no-op), Roller relies on its caller
// honouring that contract rather than enforcing it: a second Start would
// spawn a second loop racing the first one's ticks against the same store,
// and a second Stop would panic closing an already-closed channel. This
// package documents the contract instead of guarding it because Roller has
// exactly one caller (main), constructed and started once at startup — the
// failure mode a sync.Once guards against (some other, unrelated caller
// double-starting or double-stopping it) does not arise here.
func (r *Roller) Start() {
	go func() {
		defer close(r.stopped)
		// Catch up BEFORE the first tick. A process that lives less than one
		// interval would otherwise aggregate nothing at all, and rows left behind
		// by an earlier run sit outside every future 2h lookback forever.
		if err := RollupCatchUp(context.Background(), r.store, time.Now()); err != nil {
			r.log.Warn("initial rollup failed", "err", err)
		}
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case now := <-t.C:
				// Look back further than the interval so a bucket straddling a
				// tick, or one missed while the process was down, is still
				// recomputed.
				if err := RollupOnce(context.Background(), r.store, now, 2*time.Hour); err != nil {
					r.log.Warn("rollup failed", "err", err)
				}
			}
		}
	}()
}

// Stop ends the background loop and waits for it to exit. Must be paired
// with exactly one prior Start; see Start's doc comment on why a duplicate
// Stop is not guarded against here the way prober.Prober's is.
func (r *Roller) Stop() {
	close(r.stop)
	<-r.stopped
}
