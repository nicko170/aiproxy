package view

import (
	"context"
	"sync"
	"sync/atomic"
)

// subscriberBuffer bounds how many events a slow subscriber may lag behind
// before events are dropped for it. It exists only to absorb a brief burst;
// a subscriber that is chronically behind is expected to lose events, not to
// slow anything down (see publish below).
const subscriberBuffer = 64

// hub fans a stream of Events out to every current subscriber. It backs
// Local.Subscribe; a future view.HTTP has no hub of its own; it reads the
// control API's SSE stream instead.
type hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}

	// dropped counts every event discarded for a full subscriber (see
	// publish). Surfaced as Status.EventsDropped so a drop is visible rather
	// than silent, matching the reasoning behind Status.MetricsDropped.
	dropped atomic.Int64
}

func newHub() *hub {
	return &hub{subs: map[chan Event]struct{}{}}
}

// subscribe registers a new subscriber and returns its channel. The channel
// is buffered so a momentary lag does not cost the publisher anything; publish
// below drops rather than blocks once that buffer is full.
//
// A subscriber is removed the moment ctx is done, via its own goroutine, so a
// caller that unsubscribes by cancelling its context never has to call an
// explicit Unsubscribe and can never leak past that cancellation. The channel
// is closed at that same point (source.go's Subscribe doc comment promises
// this), which is race-free because publish only ever sends to ch while
// holding h.mu — the same lock this goroutine holds across the delete-and-
// close pair, so a send can never land on an already-closed channel.
func (h *hub) subscribe(ctx context.Context) chan Event {
	ch := make(chan Event, subscriberBuffer)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subs, ch)
		close(ch)
		h.mu.Unlock()
	}()

	return ch
}

// publish delivers ev to every current subscriber and must never block: it is
// called from the same request-completion path metrics ingestion is on
// (spec invariant 3), so a subscriber that stopped reading — a detached TUI,
// a dropped SSE connection — must never hold up a proxied request. A full
// channel drops the event for that subscriber rather than waiting for room,
// and counts it in dropped so the drop is observable (spec invariant 3: "and
// says so").
func (h *hub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			h.dropped.Add(1)
		}
	}
}

// droppedCount reports how many events have been discarded for a full
// subscriber since the hub was created.
func (h *hub) droppedCount() int64 { return h.dropped.Load() }
