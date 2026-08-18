package view

import (
	"context"
	"sync"
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
// explicit Unsubscribe and can never leak past that cancellation.
func (h *hub) subscribe(ctx context.Context) chan Event {
	ch := make(chan Event, subscriberBuffer)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()

	return ch
}

// publish delivers ev to every current subscriber and must never block: it is
// called from the same request-completion path metrics ingestion is on
// (spec invariant 3), so a subscriber that stopped reading — a detached TUI,
// a dropped SSE connection — must never hold up a proxied request. A full
// channel drops the event for that subscriber rather than waiting for room.
func (h *hub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
