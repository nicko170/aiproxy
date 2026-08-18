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
// The subtlety it exists for: a streamed message reports output_tokens as a
// RUNNING TOTAL for that message, re-sent with every delta — not an increment
// since the previous event. Summing observations therefore inflates output
// roughly in proportion to the number of deltas, so a long completion is wrong
// by a large factor while still looking like a believable number. Input and
// cache figures are the opposite: they arrive once per message and must be
// summed across messages.
//
// So: output is a high-water mark WITHIN a message and a sum ACROSS messages;
// input and cache are always sums.
//
// Not safe for concurrent use. One accumulator belongs to one request, and the
// relay observes from a single goroutine.
type UsageAccumulator struct {
	// settled holds messages already closed out.
	settled UsageTotals
	// currentOutput is the high-water running total for the message in flight.
	currentOutput int64
}

func NewUsageAccumulator() *UsageAccumulator { return &UsageAccumulator{} }

// StartMessage closes out the message in flight and begins a new one. Safe to
// call before the first observation.
func (a *UsageAccumulator) StartMessage() {
	a.settled.OutputTokens += a.currentOutput
	a.currentOutput = 0
}

// Observe records one usage report. Input and cache counts add; output replaces
// the running total when it advances.
func (a *UsageAccumulator) Observe(d *provider.UsageDelta) {
	if d == nil {
		return
	}
	a.settled.InputTokens += d.InputTokens
	a.settled.CacheReadTokens += d.CacheReadTokens
	a.settled.CacheWriteTokens += d.CacheWriteTokens

	// A running total that goes backwards (retry, reordering) must not reduce
	// the count.
	if d.OutputTokens > a.currentOutput {
		a.currentOutput = d.OutputTokens
	}
}

// Totals returns the request's accounting, including the message in flight.
func (a *UsageAccumulator) Totals() UsageTotals {
	out := a.settled
	out.OutputTokens += a.currentOutput
	return out
}
