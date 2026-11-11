package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// Window is a closed-open time range in unix ms.
type Window struct {
	From int64
	To   int64
}

// GroupBy names the dimension a series is split along.
type GroupBy string

const (
	GroupByAccount GroupBy = "account"
	GroupByModel   GroupBy = "model"
	GroupByOutcome GroupBy = "outcome"
)

type SeriesQuery struct {
	Window      Window
	Granularity Granularity
	GroupBy     GroupBy
}

// Point is one bucket of one series.
type Point struct {
	BucketStart      int64
	Key              string
	Requests         int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicros       int64
}

type Series struct {
	Granularity Granularity
	GroupBy     GroupBy
	Points      []Point
}

type Totals struct {
	Requests         int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostMicros       int64
	// UnpricedRequests is how many rows had no known price, so a cost total is
	// never presented as complete when it is not.
	UnpricedRequests int64
}

type Latency struct {
	TTFBP50     int64
	TTFBP95     int64
	DurationP50 int64
	DurationP95 int64
}

type QuotaPoint struct {
	At          int64
	Bucket      string
	Utilization float64
	ResetsAt    int64
}

// UsageSeries reads rollups, not raw rows — that is what keeps a 90-day query
// cheap. Grouping by outcome falls back to raw rows, since outcome is not a
// rollup dimension (adding it would multiply bucket cardinality for a
// breakdown that is only ever read over short windows).
func (s *Store) UsageSeries(ctx context.Context, q SeriesQuery) (Series, error) {
	out := Series{Granularity: q.Granularity, GroupBy: q.GroupBy}
	if q.Granularity == "" {
		q.Granularity = GranularityHour
		out.Granularity = q.Granularity
	}

	var query string
	switch q.GroupBy {
	case GroupByAccount:
		query = `
SELECT bucket_start, account_id, sum(requests), sum(input_tokens), sum(output_tokens),
       sum(cache_read_tokens), sum(cache_write_tokens), coalesce(sum(cost_micros), 0)
FROM usage_buckets
WHERE granularity = ? AND bucket_start >= ? AND bucket_start < ?
GROUP BY bucket_start, account_id
ORDER BY bucket_start, account_id`
	case GroupByModel:
		query = `
SELECT bucket_start, model, sum(requests), sum(input_tokens), sum(output_tokens),
       sum(cache_read_tokens), sum(cache_write_tokens), coalesce(sum(cost_micros), 0)
FROM usage_buckets
WHERE granularity = ? AND bucket_start >= ? AND bucket_start < ?
GROUP BY bucket_start, model
ORDER BY bucket_start, model`
	case GroupByOutcome:
		span := q.Granularity.millis()
		query = fmt.Sprintf(`
SELECT (started_at / %d) * %d, outcome, count(*), coalesce(sum(input_tokens),0),
       coalesce(sum(output_tokens),0), coalesce(sum(cache_read_tokens),0),
       coalesce(sum(cache_write_tokens),0), coalesce(sum(cost_micros),0)
FROM requests
WHERE started_at >= ? AND started_at < ?
GROUP BY 1, outcome
ORDER BY 1, outcome`, span, span)
	default:
		return out, fmt.Errorf("unknown group-by %q", q.GroupBy)
	}

	var rows *sql.Rows
	var err error
	if q.GroupBy == GroupByOutcome {
		rows, err = s.db.QueryContext(ctx, query, q.Window.From, q.Window.To)
	} else {
		rows, err = s.db.QueryContext(ctx, query, string(q.Granularity), q.Window.From, q.Window.To)
	}
	if err != nil {
		return out, fmt.Errorf("usage series: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.BucketStart, &p.Key, &p.Requests, &p.InputTokens,
			&p.OutputTokens, &p.CacheReadTokens, &p.CacheWriteTokens, &p.CostMicros); err != nil {
			return out, fmt.Errorf("scan usage point: %w", err)
		}
		out.Points = append(out.Points, p)
	}
	return out, rows.Err()
}

// Totals reads raw rows so it can also report how many were unpriced.
//
// EVERY aggregate is coalesced, including the unpriced count. sum() over zero
// rows is NULL in SQLite, not 0, and scanning that into an int64 is an error —
// so an uncoalesced column turns the most ordinary input there is (a fresh
// install, an idle hour, a window scrubbed back to a quiet period) into a hard
// failure of the whole call.
func (s *Store) Totals(ctx context.Context, w Window) (Totals, error) {
	var t Totals
	err := s.db.QueryRowContext(ctx, `
SELECT count(*),
       coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
       coalesce(sum(cache_read_tokens),0), coalesce(sum(cache_write_tokens),0),
       coalesce(sum(cost_micros),0),
       coalesce(sum(CASE WHEN cost_micros IS NULL THEN 1 ELSE 0 END), 0)
FROM requests
WHERE started_at >= ? AND started_at < ?`, w.From, w.To).
		Scan(&t.Requests, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens,
			&t.CacheWriteTokens, &t.CostMicros, &t.UnpricedRequests)
	if err != nil {
		return t, fmt.Errorf("totals: %w", err)
	}
	return t, nil
}

// LatencyPercentiles computes p50/p95 of TTFB and duration over a window.
// Requests that never produced a first byte record ttfb_ms = -1 and are
// excluded, as is a missing duration: counting them would drag the percentile
// toward zero and hide the very latency problem it exists to show.
//
// One indexed range scan reads both columns and the percentiles are picked in
// memory. This used to be eight queries — a count and an ORDER BY … OFFSET
// sort per percentile — and over a 30-day window on a busy proxy (411k rows)
// that took 6.5 s on its own, which put the Usage screen past its fetch
// deadline in production. The pick is unchanged: the element at
// floor((n-1)·p) of the ascending values.
func (s *Store) LatencyPercentiles(ctx context.Context, w Window) (Latency, error) {
	var l Latency
	rows, err := s.db.QueryContext(ctx, `
SELECT coalesce(ttfb_ms, -1), coalesce(duration_ms, -1) FROM requests
WHERE started_at >= ? AND started_at < ?`, w.From, w.To)
	if err != nil {
		return l, fmt.Errorf("latency: %w", err)
	}
	defer rows.Close()

	var ttfb, dur []int64
	for rows.Next() {
		var t, d int64
		if err := rows.Scan(&t, &d); err != nil {
			return l, fmt.Errorf("latency: %w", err)
		}
		if t >= 0 {
			ttfb = append(ttfb, t)
		}
		if d >= 0 {
			dur = append(dur, d)
		}
	}
	if err := rows.Err(); err != nil {
		return l, fmt.Errorf("latency: %w", err)
	}

	slices.Sort(ttfb)
	slices.Sort(dur)
	l.TTFBP50, l.TTFBP95 = pickPercentile(ttfb, 0.50), pickPercentile(ttfb, 0.95)
	l.DurationP50, l.DurationP95 = pickPercentile(dur, 0.50), pickPercentile(dur, 0.95)
	return l, nil
}

// pickPercentile returns the element at floor((n-1)·p) of ascending values,
// or 0 for none.
func pickPercentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int64(float64(len(sorted)-1)*p)]
}

// AccountQuotaHistory returns one account's observed quota, oldest first.
func (s *Store) AccountQuotaHistory(ctx context.Context, accountID string, w Window) ([]QuotaPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT at, bucket, utilization, coalesce(resets_at, 0)
FROM quota_samples
WHERE account_id = ? AND at >= ? AND at < ?
ORDER BY at`, accountID, w.From, w.To)
	if err != nil {
		return nil, fmt.Errorf("quota history: %w", err)
	}
	defer rows.Close()

	var out []QuotaPoint
	for rows.Next() {
		var p QuotaPoint
		if err := rows.Scan(&p.At, &p.Bucket, &p.Utilization, &p.ResetsAt); err != nil {
			return nil, fmt.Errorf("scan quota point: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
