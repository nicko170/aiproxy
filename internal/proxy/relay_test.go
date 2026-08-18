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

	"github.com/nicko170/aiproxy/internal/privacy"
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

// restoreFixture builds a real restorer plus the placeholder its table resolves,
// so the relay-level tests below exercise the production restore path rather
// than a stub of it.
func restoreFixture(t *testing.T) (*privacy.Restorer, string, string) {
	t.Helper()
	const secret = "AKIAIOSFODNN7EXAMPLE"
	f := testFilter(t, privacy.Closed)
	redacted, table, err := f.Redact(context.Background(),
		[]byte(`{"messages":[{"role":"user","content":"key `+secret+` here"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	start, end, ok := privacy.FindPlaceholder(string(redacted))
	if !ok {
		t.Fatalf("no placeholder was minted: %s", redacted)
	}
	return f.Restorer(table), string(redacted[start:end]), secret
}

// The restoring path holds a whole non-streaming body in memory. A pathological
// body must hit a stated ceiling rather than being retained without limit, and
// the ceiling must be an error rather than a silent truncation — a truncated
// body is a corrupted one.
func TestRestoreChunkRefusesToBufferPastTheCeiling(t *testing.T) {
	r, _, _ := restoreFixture(t)
	opts := RelayOptions{Restore: r, Streaming: false}

	events := make([]byte, maxRestoreBuffer)
	if _, err := restoreChunk(&events, []byte("one byte over"), opts); err == nil {
		t.Fatal("restoreChunk accepted a body past maxRestoreBuffer")
	}

	// The same ceiling applies to a stream, where the held bytes are one
	// incomplete event rather than a whole body.
	events = make([]byte, maxRestoreBuffer)
	if _, err := restoreChunk(&events, []byte("over"), RelayOptions{Restore: r, Streaming: true}); err == nil {
		t.Fatal("restoreChunk accepted a stream event past maxRestoreBuffer")
	}
}

// A stream that ends without a final terminator still has to reach the client:
// dropping the tail silently loses whatever the last event carried, which on
// this path is the end of the model's answer.
func TestFlushRestoreEmitsAStreamThatEndedWithoutATerminator(t *testing.T) {
	r, placeholder, secret := restoreFixture(t)

	// A complete event, minus the trailing blank line that would frame it.
	partial := []byte(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"key ` + placeholder + `"}}`)
	events := append([]byte{}, partial...)

	out, err := flushRestore(&events, RelayOptions{Restore: r, Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), secret) {
		t.Errorf("the unterminated tail was not restored: %s", out)
	}
	if len(events) != 0 {
		t.Errorf("flushRestore left %d bytes held", len(events))
	}
	// Flushing twice must not re-emit: Relay calls it once at EOF, but the held
	// buffer is shared state and a double emit would duplicate output.
	again, err := flushRestore(&events, RelayOptions{Restore: r, Streaming: true})
	if err != nil || len(again) != 0 {
		t.Errorf("second flush returned %q, %v; want nothing", again, err)
	}
}

// The reason the restoring path buffers at all: a placeholder does not respect
// chunk boundaries, and neither does an SSE event. This splits one event across
// two relay reads and asserts the client still receives the plaintext — through
// the real Relay, over a real socket, which is the layer the unit tests above
// cannot reach.
func TestRelayRestoresAPlaceholderSplitAcrossReads(t *testing.T) {
	r, placeholder, secret := restoreFixture(t)

	event := `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"key ` +
		placeholder + ` here"}}` + "\n\n"
	// Cut inside the placeholder itself, which is where the naive
	// rewrite-each-chunk implementation fails.
	cut := strings.Index(event, placeholder) + 5

	chunks := &chunkedReader{parts: []string{event[:cut], event[cut:]}}
	srv := relayServer(t, func() (io.ReadCloser, http.Header) {
		return io.NopCloser(chunks), http.Header{"Content-Type": []string{"text/event-stream"}}
	}, RelayOptions{BodyIdle: 5 * time.Second, Streaming: true, Restore: r})

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)

	if !strings.Contains(string(got), secret) {
		t.Errorf("the split placeholder was not restored: %s", got)
	}
	if strings.Contains(string(got), privacy.Sentinel) {
		t.Errorf("a placeholder reached the client: %s", got)
	}
}

// chunkedReader hands out one part per Read, so a caller sees exactly the chunk
// boundaries the test chose rather than whatever the socket coalesces into.
type chunkedReader struct {
	parts []string
	i     int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.i >= len(c.parts) {
		return 0, io.EOF
	}
	n := copy(p, c.parts[c.i])
	c.i++
	return n, nil
}

// Property 3 through the composed path: a stream with no placeholder sentinel
// is passed through with NO added buffering.
//
// Filter.Redact always returns a table, so before this the relay armed a
// restorer for EVERY request once the filter was on — including the
// overwhelming majority that redact nothing. An armed restorer holds bytes
// until the next event terminator, so an upstream chunk that ends mid-event is
// withheld entirely: the first chunk here writes zero bytes and the client sees
// nothing until the next chunk arrives 100ms later.
func TestRelayIsNotArmedWhenNothingWasRedacted(t *testing.T) {
	const head = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"one\"}}\n"
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = testFilter(t, privacy.Closed)
	}, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			// Deliberately one newline short of an event terminator, which is
			// exactly what a token stream looks like in the wild: upstream chunk
			// boundaries have nothing to do with event boundaries.
			{Data: head},
			{Delay: 100 * time.Millisecond, Data: "\n"},
		},
	})

	start := time.Now()
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"nothing sensitive at all"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	buf := make([]byte, 4096)
	n, err := res.Body.Read(buf)
	firstByte := time.Since(start)
	if n == 0 {
		t.Fatalf("no bytes before %v: %v", firstByte, err)
	}
	if firstByte > 60*time.Millisecond {
		t.Errorf("first byte took %v; the stream was buffered to the next event terminator", firstByte)
	}
	io.Copy(io.Discard, res.Body)
}
