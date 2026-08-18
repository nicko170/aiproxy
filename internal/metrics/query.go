package metrics

import (
	"context"
	"database/sql"
	"fmt"
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
func (s *Store) Totals(ctx context.Context, w Window) (Totals, error) {
	var t Totals
	err := s.db.QueryRowContext(ctx, `
SELECT count(*),
       coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
       coalesce(sum(cache_read_tokens),0), coalesce(sum(cache_write_tokens),0),
       coalesce(sum(cost_micros),0),
       sum(CASE WHEN cost_micros IS NULL THEN 1 ELSE 0 END)
FROM requests
WHERE started_at >= ? AND started_at < ?`, w.From, w.To).
		Scan(&t.Requests, &t.InputTokens, &t.OutputTokens, &t.CacheReadTokens,
			&t.CacheWriteTokens, &t.CostMicros, &t.UnpricedRequests)
	if err != nil {
		return t, fmt.Errorf("totals: %w", err)
	}
	return t, nil
}

// LatencyPercentiles computes p50/p95 by offset. Requests that never produced a
// first byte record ttfb_ms = -1 and are excluded: counting them would drag the
// percentile toward zero and hide the very latency problem it exists to show.
func (s *Store) LatencyPercentiles(ctx context.Context, w Window) (Latency, error) {
	var l Latency
	pick := func(column string, pct float64) (int64, error) {
		var n int64
		if err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT count(*) FROM requests WHERE started_at >= ? AND started_at < ? AND %s >= 0`, column),
			w.From, w.To).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
		offset := int64(float64(n-1) * pct)
		var v int64
		err := s.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT %s FROM requests
WHERE started_at >= ? AND started_at < ? AND %s >= 0
ORDER BY %s LIMIT 1 OFFSET ?`, column, column, column),
			w.From, w.To, offset).Scan(&v)
		return v, err
	}

	var err error
	if l.TTFBP50, err = pick("ttfb_ms", 0.50); err != nil {
		return l, fmt.Errorf("ttfb p50: %w", err)
	}
	if l.TTFBP95, err = pick("ttfb_ms", 0.95); err != nil {
		return l, fmt.Errorf("ttfb p95: %w", err)
	}
	if l.DurationP50, err = pick("duration_ms", 0.50); err != nil {
		return l, fmt.Errorf("duration p50: %w", err)
	}
	if l.DurationP95, err = pick("duration_ms", 0.95); err != nil {
		return l, fmt.Errorf("duration p95: %w", err)
	}
	return l, nil
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
