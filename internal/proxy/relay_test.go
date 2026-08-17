package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// relayServer fronts an upstream reader with a handler that relays it, so the
// test measures what a real client observes over a real socket.
func relayServer(t *testing.T, open func() (io.ReadCloser, http.Header), opts RelayOptions) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, hdr := open()
		defer body.Close()
		for k, vs := range hdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(http.StatusOK)
		Relay(r.Context(), w, body, opts)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// LOAD-BEARING. A chunk received from upstream must reach the client before the
// next one is read. If this fails the proxy is buffering, which presents to a
// user as a response that arrives all at once at the end.
func TestRelayStreamsIncrementallyWithoutBuffering(t *testing.T) {
	const gap = 100 * time.Millisecond
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Delay: 0, Data: "data: one\n\n"},
			{Delay: gap, Data: "data: two\n\n"},
			{Delay: gap, Data: "data: three\n\n"},
		},
	})

	srv := relayServer(t, func() (io.ReadCloser, http.Header) {
		res, err := http.Get(up.URL() + "/v1/messages")
		if err != nil {
			t.Fatalf("upstream: %v", err)
		}
		return res.Body, res.Header
	}, RelayOptions{BodyIdle: 5 * time.Second, Streaming: true})

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()

	start := time.Now()
	arrivals := []time.Duration{}
	buf := make([]byte, 4096)
	for len(arrivals) < 3 {
		n, err := res.Body.Read(buf)
		if n > 0 {
			arrivals = append(arrivals, time.Since(start))
		}
		if err != nil {
			break
		}
	}

	if len(arrivals) < 3 {
		t.Fatalf("observed %d chunks, want 3 — the relay buffered instead of streaming", len(arrivals))
	}
	if arrivals[0] > 60*time.Millisecond {
		t.Errorf("first chunk arrived after %v, expected promptly", arrivals[0])
	}
	// Each later chunk must trail the previous by roughly the upstream gap. A
	// buffering relay delivers all three at once at the end instead.
	for i := 1; i < len(arrivals); i++ {
		delta := arrivals[i] - arrivals[i-1]
		if delta < 60*time.Millisecond {
			t.Errorf("chunk %d arrived only %v after chunk %d; chunks are not being flushed as they arrive",
				i, delta, i-1)
		}
	}
}

func TestRelayCopiesBodyAndReportsByteCount(t *testing.T) {
	body := io.NopCloser(strings.NewReader("hello world"))
	var n int64
	var relayErr error

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, relayErr = Relay(r.Context(), w, body, RelayOptions{BodyIdle: time.Second})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)

	if string(got) != "hello world" {
		t.Errorf("body = %q", got)
	}
	if relayErr != nil {
		t.Errorf("Relay err = %v", relayErr)
	}
	if n != 11 {
		t.Errorf("wrote %d bytes, want 11", n)
	}
}

// A stalled upstream after headers must become a fast failure, not a hang. The
// headers timeout does not cover this window: it is disarmed once headers land.
func TestRelayFailsFastOnIdleUpstream(t *testing.T) {
	stall := &stallingReader{ready: make(chan struct{})}

	var relayErr error
	var wg sync.WaitGroup
	wg.Add(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		_, relayErr = Relay(r.Context(), w, stall, RelayOptions{BodyIdle: 60 * time.Millisecond})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	wg.Wait()
	close(stall.ready)

	if !errors.Is(relayErr, ErrBodyIdle) {
		t.Fatalf("Relay err = %v, want ErrBodyIdle", relayErr)
	}
}

// A healthy but slow stream must never be cut: the watchdog measures silence
// between chunks, not total duration.
func TestRelayDoesNotCutASlowButActiveStream(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Delay: 40 * time.Millisecond, Data: "data: a\n\n"},
			{Delay: 40 * time.Millisecond, Data: "data: b\n\n"},
			{Delay: 40 * time.Millisecond, Data: "data: c\n\n"},
		},
	})

	var relayErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res, err := http.Get(up.URL() + "/v1/messages")
		if err != nil {
			t.Errorf("upstream: %v", err)
			return
		}
		defer res.Body.Close()
		// Total duration (120ms) exceeds the idle window (90ms), but no single
		// gap does.
		_, relayErr = Relay(r.Context(), w, res.Body, RelayOptions{
			BodyIdle: 90 * time.Millisecond, Streaming: true,
		})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if relayErr != nil {
		t.Errorf("Relay err = %v, want nil for a slow but active stream", relayErr)
	}
	if !strings.Contains(string(got), "data: c") {
		t.Errorf("stream truncated: %q", got)
	}
}

func TestRelayTeesUsageFromSSEEvents(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":500}}}` +
		"\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n"

	var mu sync.Mutex
	seen := []provider.UsageDelta{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Relay(r.Context(), w, io.NopCloser(strings.NewReader(sse)), RelayOptions{
			BodyIdle:  time.Second,
			Streaming: true,
			ParseUsage: func(event []byte) (*provider.UsageDelta, bool) {
				var d provider.UsageDelta
				s := string(event)
				switch {
				case strings.Contains(s, "message_start"):
					d = provider.UsageDelta{InputTokens: 10, CacheReadTokens: 500}
				case strings.Contains(s, "message_delta"):
					d = provider.UsageDelta{OutputTokens: 7}
				default:
					return nil, false
				}
				return &d, true
			},
			OnUsage: func(d *provider.UsageDelta) {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, *d)
			},
		})
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("observed %d usage deltas, want 2: %+v", len(seen), seen)
	}
	if seen[0].InputTokens != 10 || seen[0].CacheReadTokens != 500 {
		t.Errorf("first delta = %+v", seen[0])
	}
	if seen[1].OutputTokens != 7 {
		t.Errorf("second delta = %+v", seen[1])
	}
}

func TestRelayStopsWhenClientDisconnects(t *testing.T) {
	// An endless upstream: the relay must stop on client cancellation rather
	// than reading forever.
	endless := io.NopCloser(&endlessReader{})

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		Relay(r.Context(), w, endless, RelayOptions{BodyIdle: 5 * time.Second})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	res.Body.Read(buf)
	cancel()
	res.Body.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not stop after the client disconnected")
	}
}

// stallingReader blocks until ready is closed, then reports EOF.
type stallingReader struct{ ready chan struct{} }

func (s *stallingReader) Read(p []byte) (int, error) {
	<-s.ready
	return 0, io.EOF
}

type endlessReader struct{}

func (e *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	time.Sleep(5 * time.Millisecond)
	return len(p), nil
}
