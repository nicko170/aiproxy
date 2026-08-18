package proxy

import "github.com/nicko170/aiproxy/internal/provider"

// UsageTotals is one request's complete token accounting.
type UsageTotals struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// UsageAccumulator folds a stream of provider usage observations into one
// correct total.
//
// The subtlety it exists for: every usage field in a streamed message is
// cumulative WITHIN that message, re-sent in full with every event — not an
// increment since the previous one. A message_delta routinely repeats
// input_tokens and cache_read_input_tokens alongside an advancing
// output_tokens. Summing observations therefore inflates every field roughly
// in proportion to the number of events, and since cache reads dominate an
// agent workload by volume, a summing accumulator overstates cost by close to
// 2x while still looking like a believable number.
//
// So: every field is a high-water mark WITHIN a message, and those marks are
// summed ACROSS messages (a multi-turn stream genuinely does spend more input
// on message 2 than message 1).
//
// Not safe for concurrent use. One accumulator belongs to one request, and the
// relay observes from a single goroutine.
type UsageAccumulator struct {
	// settled holds messages already closed out.
	settled UsageTotals
	// current is the high-water mark for the message in flight.
	current UsageTotals
}

func NewUsageAccumulator() *UsageAccumulator { return &UsageAccumulator{} }

// StartMessage closes out the message in flight and begins a new one. Safe to
// call before the first observation.
func (a *UsageAccumulator) StartMessage() {
	a.settled.InputTokens += a.current.InputTokens
	a.settled.OutputTokens += a.current.OutputTokens
	a.settled.CacheReadTokens += a.current.CacheReadTokens
	a.settled.CacheWriteTokens += a.current.CacheWriteTokens
	a.current = UsageTotals{}
}

// Observe records one usage report. Every field replaces the running total
// for the message in flight when it advances; a value that goes backwards
// (retry, reordering) must not reduce the count.
func (a *UsageAccumulator) Observe(d *provider.UsageDelta) {
	if d == nil {
		return
	}
	if d.InputTokens > a.current.InputTokens {
		a.current.InputTokens = d.InputTokens
	}
	if d.OutputTokens > a.current.OutputTokens {
		a.current.OutputTokens = d.OutputTokens
	}
	if d.CacheReadTokens > a.current.CacheReadTokens {
		a.current.CacheReadTokens = d.CacheReadTokens
	}
	if d.CacheWriteTokens > a.current.CacheWriteTokens {
		a.current.CacheWriteTokens = d.CacheWriteTokens
	}
}

// Totals returns the request's accounting, including the message in flight.
func (a *UsageAccumulator) Totals() UsageTotals {
	out := a.settled
	out.InputTokens += a.current.InputTokens
	out.OutputTokens += a.current.OutputTokens
	out.CacheReadTokens += a.current.CacheReadTokens
	out.CacheWriteTokens += a.current.CacheWriteTokens
	return out
}
