package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// PruneOnce deletes raw rows older than the retention window and returns how
// many request rows went.
//
// Rollups are never pruned. They are tiny and they are what makes a 90-day
// query cheap; deleting them would trade the only cheap path for disk space
// that was never the problem. Raw rows exist for drill-down, which has a much
// shorter useful life.
//
// Ordering matters operationally: rollups run every minute and retention runs
// daily, so by the time a row is old enough to prune it has long since been
// aggregated. A retention window shorter than the rollup interval would lose
// data, which is why retain is measured in days.
func PruneOnce(ctx context.Context, s *Store, now time.Time, retain time.Duration) (int64, error) {
	if retain <= 0 {
		return 0, nil // retention disabled: keep everything
	}
	cutoff := now.Add(-retain).UnixMilli()

	res, err := s.DB().ExecContext(ctx, `DELETE FROM requests WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune requests: %w", err)
	}
	deleted, _ := res.RowsAffected()

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM quota_samples WHERE at < ?`, cutoff); err != nil {
		return deleted, fmt.Errorf("prune quota samples: %w", err)
	}
	return deleted, nil
}

// Pruner runs PruneOnce on a timer.
type Pruner struct {
	store   *Store
	retain  time.Duration
	log     *slog.Logger
	stop    chan struct{}
	stopped chan struct{}
}

func NewPruner(s *Store, retain time.Duration, log *slog.Logger) *Pruner {
	if log == nil {
		log = slog.Default()
	}
	return &Pruner{
		store: s, retain: retain, log: log,
		stop: make(chan struct{}), stopped: make(chan struct{}),
	}
}

func (p *Pruner) Start() {
	go func() {
		defer close(p.stopped)
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case now := <-t.C:
				n, err := PruneOnce(context.Background(), p.store, now, p.retain)
				if err != nil {
					p.log.Warn("retention prune failed", "err", err)
					continue
				}
				if n > 0 {
					p.log.Info("pruned raw request rows", "deleted", n,
						"retainDays", int(p.retain.Hours()/24))
				}
			}
		}
	}()
}

func (p *Pruner) Stop() {
	close(p.stop)
	<-p.stopped
}
