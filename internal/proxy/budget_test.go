package proxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSleep records requested sleeps and returns immediately, so budget
// accounting is tested without real time.
func fakeSleep(log *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*log = append(*log, d)
		return nil
	}
}

func TestBudgetSpendDrawsDownAndRefusesOverspend(t *testing.T) {
	b := NewBudget(time.Second)

	if !b.Spend(400 * time.Millisecond) {
		t.Fatal("first spend should be allowed")
	}
	if got := b.Remaining(); got != 600*time.Millisecond {
		t.Errorf("Remaining = %v, want 600ms", got)
	}
	if b.Spend(700 * time.Millisecond) {
		t.Error("overspend should be refused")
	}
	if got := b.Remaining(); got != 600*time.Millisecond {
		t.Errorf("refused spend must not draw down; Remaining = %v", got)
	}
	if b.Exhausted() {
		t.Error("budget with remaining time is not exhausted")
	}
	if !b.Spend(600 * time.Millisecond) {
		t.Fatal("spending exactly the remainder should be allowed")
	}
	if !b.Exhausted() {
		t.Error("budget should be exhausted")
	}
}

func TestBudgetWaitSleepsAndDrawsDown(t *testing.T) {
	var slept []time.Duration
	b := NewBudget(time.Second)
	b.Sleep = fakeSleep(&slept)

	if err := b.Wait(context.Background(), 250*time.Millisecond); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(slept) != 1 || slept[0] != 250*time.Millisecond {
		t.Fatalf("slept = %v, want [250ms]", slept)
	}
	if got := b.Remaining(); got != 750*time.Millisecond {
		t.Errorf("Remaining = %v, want 750ms", got)
	}
}

// The invariant: a wait longer than the remaining budget must not happen at
// all. Sleeping the remainder and then failing would still burn the wall-clock
// this design exists to bound.
func TestBudgetWaitRefusesToSleepBeyondRemaining(t *testing.T) {
	var slept []time.Duration
	b := NewBudget(100 * time.Millisecond)
	b.Sleep = fakeSleep(&slept)

	err := b.Wait(context.Background(), 60*time.Second)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if len(slept) != 0 {
		t.Errorf("must not sleep at all, slept %v", slept)
	}
	if !b.Exhausted() {
		t.Error("a refused oversized wait exhausts the budget")
	}
}

func TestBudgetWaitHonoursContextCancellation(t *testing.T) {
	b := NewBudget(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Wait(ctx, 10*time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestBudgetRealSleepIsBoundedByRemaining(t *testing.T) {
	b := NewBudget(30 * time.Millisecond)
	start := time.Now()
	err := b.Wait(context.Background(), 25*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("waited %v, far beyond the requested 25ms", elapsed)
	}
}
