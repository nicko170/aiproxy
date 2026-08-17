package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ErrBodyIdle reports that the upstream produced no bytes for the whole idle
// window after headers had already arrived.
var ErrBodyIdle = errors.New("upstream stream idle")

// sseTerminator separates events in a Server-Sent Events stream.
var sseTerminator = []byte("\n\n")

// RelayOptions configures one relay.
type RelayOptions struct {
	// BodyIdle bounds silence BETWEEN chunks, not total duration, so a long but
	// healthy stream is never cut.
	BodyIdle time.Duration
	// Streaming enables SSE event framing for the usage tee.
	Streaming  bool
	ParseUsage func(event []byte) (*provider.UsageDelta, bool)
	OnUsage    func(*provider.UsageDelta)
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
	var pending []byte // incomplete trailing SSE event
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
					flushRemainingUsage(pending, opts)
					return written, nil
				}
				return written, c.err
			}

			n, err := w.Write(c.buf)
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
