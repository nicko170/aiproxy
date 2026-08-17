package proxy

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/testutil"
)

// Run many streaming requests at once. This is a deadlock and race hunt rather
// than an assertion about any one response: admission, the ramp, the budget, and
// the relay all take locks or block, and a mistake among them shows up only
// under concurrency. Run with -race.
func TestConcurrentStreamsAllComplete(t *testing.T) {
	const n = 40

	h := newHarness(t, 3, RetryConfig{
		Budget: 5 * time.Second, InlineAbsorbMax: time.Second, BodyIdle: 5 * time.Second,
	}, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"},
			{Delay: 10 * time.Millisecond, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n"},
		},
	})

	var wg sync.WaitGroup
	statuses := make([]int, n)
	bodies := make([]string, n)
	readErrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
				strings.NewReader(`{"model":"claude-sonnet-5"}`))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			body, readErr := io.ReadAll(res.Body)
			res.Body.Close()
			statuses[i] = res.StatusCode
			bodies[i] = string(body)
			readErrs[i] = readErr
		}(i)
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("concurrent streams did not all finish — likely a deadlock in admission or the relay")
	}

	for i, s := range statuses {
		if s != 200 {
			t.Errorf("request %d: status %d, want 200", i, s)
		}
		// Each stream must have been relayed to completion, not merely started.
		// Under concurrency a truncated relay shows up here rather than in the
		// status, which was written before the first chunk.
		if readErrs[i] != nil {
			t.Errorf("request %d: body read failed: %v", i, readErrs[i])
		}
		if !strings.Contains(bodies[i], "message_delta") {
			t.Errorf("request %d: stream did not complete: %q", i, bodies[i])
		}
	}

	// Slot accounting. A bare "InFlight == 0 at the end" is close to a tautology:
	// Release runs as soon as response headers arrive, well before the client has
	// finished reading, so by the time this line runs every slot is long since
	// returned no matter what happened in between. The count of upstream requests
	// is what makes it mean something — exactly n admissions were granted and
	// exactly n were used, with no request silently retried or dropped — and only
	// then is a non-zero InFlight evidence of a leak rather than of timing.
	if got := len(h.upstream.Requests()); got != n {
		t.Errorf("upstream saw %d requests, want exactly %d: every admitted request "+
			"should be attempted once, with none retried or lost", got, n)
	}
	for _, a := range h.mgr.Snapshot() {
		if a.InFlight != 0 {
			t.Errorf("account %s left InFlight=%d; a slot leaked", a.ID, a.InFlight)
		}
	}
}
