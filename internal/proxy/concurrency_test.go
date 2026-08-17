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
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			statuses[i] = res.StatusCode
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
	}
	// Every slot taken must have been released.
	for _, a := range h.mgr.Snapshot() {
		if a.InFlight != 0 {
			t.Errorf("account %s left InFlight=%d; a slot leaked", a.ID, a.InFlight)
		}
	}
}
