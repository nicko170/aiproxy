package view

import (
	"context"
	"testing"
	"time"
)

// A subscriber that never reads must never be able to stall a publish: spec
// invariant 3 ("accounting never slows the proxy") extends to this seam,
// since Publish is called from the same OnResult hook the request path
// blocks on. Fill one subscriber's buffer past capacity and confirm Publish
// still returns promptly.
func TestPublishNeverBlocksOnAFullSubscriber(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx)

	done := make(chan struct{})
	go func() {
		// Deliberately never drain ch. One extra event beyond the buffer must
		// still not block this loop.
		for i := 0; i < subscriberBuffer+10; i++ {
			h.publish(Event{Model: "m"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a full, undrained subscriber")
	}
	_ = ch
}

// A publish with zero subscribers must be a no-op, not a panic or a hang —
// the ordinary case before anything ever subscribes.
func TestPublishWithNoSubscribersDoesNothing(t *testing.T) {
	h := newHub()
	done := make(chan struct{})
	go func() {
		h.publish(Event{Model: "m"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish with no subscribers hung")
	}
}

func TestSubscribeDeliversPublishedEvents(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx)

	want := Event{Model: "claude-sonnet-5", Account: "acct-0"}
	h.publish(want)

	select {
	case got := <-ch:
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber never received the published event")
	}
}

func TestPublishFansOutToEveryConcurrentSubscriber(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = 5
	chans := make([]<-chan Event, n)
	for i := range chans {
		chans[i] = h.subscribe(ctx)
	}

	h.publish(Event{Model: "fan-out"})

	for i, ch := range chans {
		select {
		case got := <-ch:
			if got.Model != "fan-out" {
				t.Errorf("subscriber %d got %+v", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d never received the event", i)
		}
	}
}

// Subscribe's documented contract (source.go) is that the channel is closed
// when ctx is cancelled, not merely abandoned — so a caller ranging over it
// (`for ev := range ch`) terminates instead of hanging forever. Before this
// was fixed, the subscriber's map entry was deleted but the channel itself
// was never closed.
func TestSubscribeChannelClosesOnCancellation(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	ch := h.subscribe(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		for range ch {
			// Drain until closed; a range over a channel that is never
			// closed blocks here forever, which is exactly the defect this
			// test catches.
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("range over the subscriber channel never terminated: channel was not closed on cancellation")
	}
}

// A full subscriber must not merely have its events dropped silently: the
// drop is counted so Status.EventsDropped can report it (spec invariant 3:
// a proxied request is never blocked to feed a UI, but a drop "says so").
func TestPublishCountsDroppedEventsForAFullSubscriber(t *testing.T) {
	h := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx)

	for i := 0; i < subscriberBuffer+5; i++ {
		h.publish(Event{Model: "m"})
	}
	if got := h.droppedCount(); got != 5 {
		t.Errorf("droppedCount() = %d, want 5", got)
	}
	_ = ch
}

// Cancelling a subscriber's context must release both its channel and its
// watcher goroutine. Repeating this many times must not grow the hub's
// subscriber set — a leak here would eventually make every publish slower
// and, more importantly, contradicts the "never blocks the request path"
// guarantee once enough dead subscribers accumulate.
func TestUnsubscribeOnCancelDoesNotLeak(t *testing.T) {
	h := newHub()

	for i := 0; i < 1000; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		h.subscribe(ctx)
		cancel()
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.subs)
		h.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub still holds %d subscriber(s) after every context was cancelled", n)
		}
		time.Sleep(time.Millisecond)
	}
}
