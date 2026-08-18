package metrics

import (
	"context"
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
	from := now.Add(-lookback).UnixMilli()
	to := now.UnixMilli()

	for _, g := range []Granularity{GranularityMinute, GranularityHour} {
		if err := rollupGrain(ctx, s, g, from, to); err != nil {
			return fmt.Errorf("rollup %s: %w", g, err)
		}
	}
	return nil
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

func (r *Roller) Start() {
	go func() {
		defer close(r.stopped)
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

func (r *Roller) Stop() {
	close(r.stop)
	<-r.stopped
}
