package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrBodyIdle reports that the upstream produced no bytes for the whole idle
// window after headers had already arrived.
var ErrBodyIdle = errors.New("upstream stream idle")

// sseTerminator separates events in a Server-Sent Events stream.
var sseTerminator = []byte("\n\n")

// maxUsageCapture bounds how much of a non-streaming body is retained to read
// its usage envelope. Usage sits near the end of a message response, but a
// pathological body must not be held in memory without limit.
//
// KNOWN LIMITATION, not covered by this round: capture retains the body's HEAD
// while usage sits at the TAIL, so a non-streaming response larger than this
// cap records zero tokens rather than the true count. Left as-is deliberately —
// zeros beat wrong numbers, and a non-streaming body this large is rare — but a
// future reader should not mistake the silence here for the cap being handled.
const maxUsageCapture = 1 << 20 // 1 MiB

// RelayOptions configures one relay.
type RelayOptions struct {
	// BodyIdle bounds silence BETWEEN chunks, not total duration, so a long but
	// healthy stream is never cut.
	BodyIdle time.Duration
	// Streaming enables SSE event framing for the usage tee.
	Streaming  bool
	ParseUsage func(event []byte) (*provider.UsageDelta, bool)
	OnUsage    func(*provider.UsageDelta)
	// ParseBody extracts usage from a complete non-streaming body. When set and
	// Streaming is false, Relay retains up to maxUsageCapture bytes and parses
	// once the body ends.
	ParseBody func(body []byte) (*provider.UsageDelta, bool)
	// Restore rewrites the response stream, substituting plaintext back for the
	// placeholders the request carried. Nil leaves the write path below exactly
	// as it was: chunks go straight to the client with no accumulation.
	//
	// Nil is the value not only when the filter is disabled but whenever the
	// request redacted nothing — see attempt.relay, which arms this only for a
	// non-empty restore table. That distinction is property 3: an armed restorer
	// buffers to the next event terminator, so arming one that provably cannot
	// resolve anything would turn a token stream into a batch for the majority
	// of requests.
	Restore *privacy.Restorer
}

type readChunk struct {
	buf []byte
	err error
}

// Relay copies body to w, flushing after every chunk, and returns the number of
// bytes written.
//
// Flushing per chunk is the whole point: a chunk received from upstream reaches
// the client before the next read begins. Buffering here — even implicitly, by
// letting the writer decide when to flush — is what makes a token stream appear
// to arrive all at once when the generation finishes.
//
// On error the response is deliberately NOT completed cleanly. A truncated
// stream ended with a clean finish looks to the client like a complete answer
// and suppresses its retry; the caller destroys the connection instead.
func Relay(ctx context.Context, w http.ResponseWriter, body io.Reader, opts RelayOptions) (int64, error) {
	if opts.BodyIdle <= 0 {
		opts.BodyIdle = 120 * time.Second
	}
	flusher, _ := w.(http.Flusher)

	// Reads run on a helper goroutine so silence is detectable: an io.Reader
	// offers no deadline of its own. Each read gets a fresh buffer because the
	// consumer may still be writing the previous one.
	chunks := make(chan readChunk, 1)
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()

	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 32*1024)
			n, err := body.Read(buf)
			if n > 0 {
				select {
				case chunks <- readChunk{buf: buf[:n]}:
				case <-readCtx.Done():
					return
				}
			}
			if err != nil {
				select {
				case chunks <- readChunk{err: err}:
				case <-readCtx.Done():
				}
				return
			}
		}
	}()

	var written int64
	var pending []byte  // incomplete trailing SSE event
	var captured []byte // non-streaming body retained for usage parsing
	var events []byte   // held for the restoring path; see restoreChunk
	idle := time.NewTimer(opts.BodyIdle)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()

		case <-idle.C:
			return written, ErrBodyIdle

		case c, ok := <-chunks:
			if !ok {
				return written, nil
			}
			if c.err != nil {
				if errors.Is(c.err, io.EOF) {
					if tail, ferr := flushRestore(&events, opts); ferr != nil {
						return written, ferr
					} else if len(tail) > 0 {
						n, werr := w.Write(tail)
						written += int64(n)
						if flusher != nil {
							flusher.Flush()
						}
						if werr != nil {
							return written, werr
						}
					}
					flushRemainingUsage(pending, opts)
					flushCapturedBody(captured, opts)
					return written, nil
				}
				return written, c.err
			}

			out, err := restoreChunk(&events, c.buf, opts)
			if err != nil {
				return written, err
			}
			n, err := w.Write(out)
			written += int64(n)
			if err != nil {
				return written, err
			}
			if flusher != nil {
				flusher.Flush()
			}

			if opts.Streaming && opts.ParseUsage != nil {
				pending = teeUsage(append(pending, c.buf...), opts)
			}

			if !opts.Streaming && opts.ParseBody != nil && len(captured) < maxUsageCapture {
				room := maxUsageCapture - len(captured)
				if room > len(c.buf) {
					room = len(c.buf)
				}
				captured = append(captured, c.buf[:room]...)
			}

			// Reset the watchdog only on real progress.
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(opts.BodyIdle)
		}
	}
}

// teeUsage consumes every complete SSE event in buf, reporting usage, and
// returns the incomplete remainder to be carried into the next chunk.
func teeUsage(buf []byte, opts RelayOptions) []byte {
	for {
		i := bytes.Index(buf, sseTerminator)
		if i < 0 {
			return buf
		}
		event := buf[:i]
		buf = buf[i+len(sseTerminator):]
		if d, ok := opts.ParseUsage(event); ok && opts.OnUsage != nil {
			opts.OnUsage(d)
		}
	}
}

func flushRemainingUsage(pending []byte, opts RelayOptions) {
	if !opts.Streaming || opts.ParseUsage == nil || len(bytes.TrimSpace(pending)) == 0 {
		return
	}
	if d, ok := opts.ParseUsage(pending); ok && opts.OnUsage != nil {
		opts.OnUsage(d)
	}
}

// flushCapturedBody parses a complete non-streaming body for usage once it has
// finished arriving.
func flushCapturedBody(captured []byte, opts RelayOptions) {
	if opts.Streaming || opts.ParseBody == nil || len(captured) == 0 {
		return
	}
	if d, ok := opts.ParseBody(captured); ok && opts.OnUsage != nil {
		opts.OnUsage(d)
	}
}

// maxRestoreBuffer bounds what the restoring path may hold. Streaming holds at
// most one SSE event, which is small; a non-streaming body is held whole, and
// this is the ceiling on that. A message response is orders of magnitude
// smaller, so the cap exists to keep a pathological body from being held in
// memory without limit rather than as a routine constraint.
const maxRestoreBuffer = 32 << 20

// restoreChunk transforms one chunk on its way to the client.
//
// With no restorer it returns buf unchanged and touches nothing — that branch is
// the pre-filter behaviour, byte for byte.
//
// Streaming: complete SSE events are extracted and rewritten; an incomplete
// trailing event is held in *events until its terminator arrives. That is one
// event of added buffering, unavoidable because a JSON event cannot be rewritten
// before it is whole, and bounded by the size of one event.
//
// Non-streaming: the whole body is accumulated and rewritten once at EOF, since
// a response the client parses as one document has nothing to gain from
// arriving in pieces.
func restoreChunk(events *[]byte, buf []byte, opts RelayOptions) ([]byte, error) {
	if opts.Restore == nil {
		return buf, nil
	}
	if len(*events)+len(buf) > maxRestoreBuffer {
		return nil, fmt.Errorf("proxy: response exceeds the %d-byte restore buffer", int64(maxRestoreBuffer))
	}
	*events = append(*events, buf...)
	if !opts.Streaming {
		return nil, nil // held until EOF
	}
	var out []byte
	for {
		i := bytes.Index(*events, sseTerminator)
		if i < 0 {
			return out, nil
		}
		event := (*events)[:i+len(sseTerminator)]
		*events = (*events)[i+len(sseTerminator):]
		rewritten, err := opts.Restore.Event(event)
		if err != nil {
			return nil, err
		}
		out = append(out, rewritten...)
	}
}

// flushRestore emits whatever the restoring path still holds at EOF: a trailing
// partial SSE event, or the whole non-streaming body.
func flushRestore(events *[]byte, opts RelayOptions) ([]byte, error) {
	if opts.Restore == nil || len(*events) == 0 {
		return nil, nil
	}
	held := *events
	*events = nil
	if !opts.Streaming {
		return opts.Restore.Body(held)
	}
	// A stream that ended without a final terminator: rewrite what arrived
	// rather than dropping it.
	return opts.Restore.Event(held)
}
