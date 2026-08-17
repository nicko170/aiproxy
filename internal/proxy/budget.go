package proxy

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBudgetExhausted reports that a request has no pre-first-byte time left.
var ErrBudgetExhausted = errors.New("pre-first-byte budget exhausted")

// Budget bounds the total time a request may spend NOT transferring response
// bytes: retry backoff, waiting on a paused account, absorbing a rate limit,
// refreshing a credential.
//
// This is the mechanism behind the design's first invariant. Every constant in
// the retry engine can be badly chosen without producing an unbounded silent
// hang, because each of them draws down this one allowance. Once response
// headers are written the request can no longer be retried, so the budget is
// never consulted again — it governs dead air and nothing else.
type Budget struct {
	// Sleep is swapped out in tests. It must return ctx.Err() on cancellation.
	Sleep func(ctx context.Context, d time.Duration) error

	mu        sync.Mutex
	remaining time.Duration
}

func NewBudget(total time.Duration) *Budget {
	if total < 0 {
		total = 0
	}
	return &Budget{remaining: total, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Budget) Remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}

func (b *Budget) Exhausted() bool { return b.Remaining() <= 0 }

// Spend draws down d, reporting false and changing nothing if d exceeds what is
// left. Use it for time already consumed, such as an elapsed refresh.
func (b *Budget) Spend(d time.Duration) bool {
	if d < 0 {
		d = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d > b.remaining {
		return false
	}
	b.remaining -= d
	return true
}

// Wait blocks for d and draws it down. When d exceeds the remaining budget it
// sleeps not at all, exhausts the budget, and returns ErrBudgetExhausted — a
// partial sleep would still burn the wall-clock this type exists to bound.
func (b *Budget) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d < 0 {
		d = 0
	}
	if !b.Spend(d) {
		b.mu.Lock()
		b.remaining = 0
		b.mu.Unlock()
		return ErrBudgetExhausted
	}
	if err := b.Sleep(ctx, d); err != nil {
		return err
	}
	return nil
}
