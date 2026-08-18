package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/testutil"
)

// A streamed completion must land on Result with the LAST running output total,
// not the sum of the deltas.
func TestResultCarriesStreamedTokenTotals(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":" +
				"{\"input_tokens\":120,\"output_tokens\":1,\"cache_read_input_tokens\":4000," +
				"\"cache_creation_input_tokens\":16}}}\n\n"},
			{Delay: 10 * time.Millisecond,
				Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"},
			{Delay: 10 * time.Millisecond,
				Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":250}}\n\n"},
		},
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	got := h.last()
	if got.OutputTokens != 250 {
		t.Errorf("OutputTokens = %d, want 250 (last running total, not 1+40+250)", got.OutputTokens)
	}
	if got.InputTokens != 120 {
		t.Errorf("InputTokens = %d, want 120", got.InputTokens)
	}
	if got.CacheReadTokens != 4000 || got.CacheWriteTokens != 16 {
		t.Errorf("cache = %d/%d, want 4000/16", got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// The gap this task closes: a non-streaming response previously contributed
// nothing at all.
func TestResultCarriesNonStreamingTokenTotals(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{"type":"message","content":[{"type":"text","text":"hi"}],
		        "usage":{"input_tokens":55,"output_tokens":12,
		                 "cache_read_input_tokens":700,"cache_creation_input_tokens":3}}`,
	})

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	got := h.last()
	if got.InputTokens != 55 || got.OutputTokens != 12 ||
		got.CacheReadTokens != 700 || got.CacheWriteTokens != 3 {
		t.Errorf("tokens = %d/%d/%d/%d, want 55/12/700/3",
			got.InputTokens, got.OutputTokens, got.CacheReadTokens, got.CacheWriteTokens)
	}
}

// A response with no usage at all must report zeros, not garbage.
func TestResultTokensAreZeroWhenUpstreamReportsNone(t *testing.T) {
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   `{"ok":true}`,
	})

	h.post()
	got := h.last()
	if got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Errorf("tokens = %d/%d, want 0/0", got.InputTokens, got.OutputTokens)
	}
}

// Streaming must still stream — capturing usage must not buffer the response.
func TestUsageCaptureDoesNotBufferAStream(t *testing.T) {
	const gap = 80 * time.Millisecond
	h := newHarness(t, 1, defaultRetry(), testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"},
			{Delay: gap, Data: "data: {\"type\":\"content_block_delta\"}\n\n"},
			{Delay: gap, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"},
		},
	})

	res, elapsed := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if elapsed < 2*gap {
		t.Errorf("completed in %v, faster than the upstream produced it — impossible unless buffered", elapsed)
	}
	if got := h.last().OutputTokens; got != 5 {
		t.Errorf("OutputTokens = %d, want 5", got)
	}
}
