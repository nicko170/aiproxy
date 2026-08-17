// Package testutil provides a scriptable fake upstream for proxy tests.
package testutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// SSEChunk is one streamed frame, written after Delay has elapsed.
type SSEChunk struct {
	Delay time.Duration
	Data  string
}

// Script is one canned response. When SSE is non-empty the response streams
// those chunks and Body is ignored.
//
// HeaderDelay withholds the response HEADERS for that long before writing
// anything. It models the one thing SSEChunk.Delay cannot: time-to-first-token.
// A large context with extended thinking legitimately spends seconds upstream
// before the first byte of the response line exists, and that is the shape a
// per-attempt deadline has to tolerate rather than sever.
type Script struct {
	Status      int
	Header      http.Header
	Body        string
	SSE         []SSEChunk
	HeaderDelay time.Duration
}

// RecordedRequest is what the fake upstream observed.
type RecordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// FakeUpstream serves scripts in order, repeating the final script for any
// further requests, and records every request it received.
type FakeUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	scripts []Script
	n       int
	seen    []RecordedRequest
}

// NewFakeUpstream starts a fake upstream that is shut down when the test ends.
func NewFakeUpstream(t *testing.T, scripts ...Script) *FakeUpstream {
	t.Helper()
	if len(scripts) == 0 {
		t.Fatal("NewFakeUpstream needs at least one script")
	}
	f := &FakeUpstream{scripts: scripts}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *FakeUpstream) URL() string { return f.srv.URL }

func (f *FakeUpstream) Close() { f.srv.Close() }

// Requests returns a copy of everything recorded so far.
func (f *FakeUpstream) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedRequest, len(f.seen))
	copy(out, f.seen)
	return out
}

func (f *FakeUpstream) next(rec RecordedRequest) Script {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, rec)
	i := f.n
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1 // last script repeats
	}
	f.n++
	return f.scripts[i]
}

func (f *FakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s := f.next(RecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	})

	if s.HeaderDelay > 0 {
		select {
		case <-time.After(s.HeaderDelay):
		case <-r.Context().Done():
			return
		}
	}

	for k, vs := range s.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := s.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	if len(s.SSE) == 0 {
		io.WriteString(w, s.Body)
		return
	}
	flusher, _ := w.(http.Flusher)
	for _, c := range s.SSE {
		if c.Delay > 0 {
			select {
			case <-time.After(c.Delay):
			case <-r.Context().Done():
				return
			}
		}
		io.WriteString(w, c.Data)
		if flusher != nil {
			flusher.Flush()
		}
	}
}
