package proxy

import (
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func d(in, out, cr, cw int64) *provider.UsageDelta {
	return &provider.UsageDelta{
		InputTokens: in, OutputTokens: out,
		CacheReadTokens: cr, CacheWriteTokens: cw,
	}
}

// THE trap this type exists for. message_delta reports output_tokens as a
// RUNNING TOTAL for the message, not a delta since the previous event. Summing
// them inflates output badly, and the longer the completion the worse it gets.
func TestAccumulatorTakesLastOutputNotTheSum(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(100, 1, 900, 5)) // message_start
	a.Observe(d(0, 10, 0, 0))    // message_delta, running total
	a.Observe(d(0, 250, 0, 0))   // message_delta, running total
	a.Observe(d(0, 812, 0, 0))   // message_delta, final running total

	got := a.Totals()
	if got.OutputTokens != 812 {
		t.Errorf("OutputTokens = %d, want 812 (the last running total, not 1+10+250+812=1073)",
			got.OutputTokens)
	}
	if got.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", got.InputTokens)
	}
	if got.CacheReadTokens != 900 || got.CacheWriteTokens != 5 {
		t.Errorf("cache = read %d / write %d, want 900 / 5", got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// A running total that goes backwards (retry, reordering) must not lower the
// count — take the high-water mark within a message.
func TestAccumulatorOutputNeverDecreasesWithinAMessage(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(0, 500, 0, 0))
	a.Observe(d(0, 300, 0, 0))

	if got := a.Totals().OutputTokens; got != 500 {
		t.Errorf("OutputTokens = %d, want 500 — a lower running total must not reduce the count", got)
	}
}

// Across messages, output ACCUMULATES: each message's final running total adds
// to the request's total. Only within one message is it a replacement.
func TestAccumulatorSumsAcrossMessages(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(100, 1, 0, 0))
	a.Observe(d(0, 40, 0, 0))
	a.StartMessage()
	a.Observe(d(50, 1, 0, 0))
	a.Observe(d(0, 60, 0, 0))

	got := a.Totals()
	if got.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100 (40 + 60)", got.OutputTokens)
	}
	if got.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150 (100 + 50)", got.InputTokens)
	}
}

// A non-streaming response reports one complete usage object with no
// message_start/message_delta split — one Observe must be recorded whole.
func TestAccumulatorHandlesASingleCompleteUsage(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(d(320, 77, 1200, 8))

	got := a.Totals()
	if got.InputTokens != 320 || got.OutputTokens != 77 ||
		got.CacheReadTokens != 1200 || got.CacheWriteTokens != 8 {
		t.Errorf("totals = %+v, want 320/77/1200/8", got)
	}
}

func TestAccumulatorZeroValueIsEmpty(t *testing.T) {
	if got := NewUsageAccumulator().Totals(); got != (UsageTotals{}) {
		t.Errorf("Totals() = %+v, want zero", got)
	}
}

// Observing without an explicit StartMessage must still record, so a provider
// that never emits a message boundary is not silently dropped.
func TestAccumulatorObserveWithoutStartMessageStillCounts(t *testing.T) {
	a := NewUsageAccumulator()
	a.Observe(d(10, 20, 0, 0))

	got := a.Totals()
	if got.InputTokens != 10 || got.OutputTokens != 20 {
		t.Errorf("totals = %+v, want 10/20", got)
	}
}

func TestAccumulatorIgnoresNilDelta(t *testing.T) {
	a := NewUsageAccumulator()
	a.StartMessage()
	a.Observe(nil)
	if got := a.Totals(); got != (UsageTotals{}) {
		t.Errorf("Totals() = %+v, want zero after a nil observation", got)
	}
}
