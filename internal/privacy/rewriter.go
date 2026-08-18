package privacy

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
)

// ErrUnresolved reports a complete placeholder with no entry in the restore
// table. Returned only under ErrorOut; Passthrough emits it verbatim instead.
var ErrUnresolved = errors.New("privacy: placeholder has no restore entry")

// UnresolvedMode selects what happens to a placeholder the table cannot resolve.
type UnresolvedMode int

const (
	// Passthrough emits the placeholder verbatim and reports it. This is the
	// default because the alternative is guessing at a value and writing it into
	// the operator's files: a visibly wrong string is recoverable, a plausible
	// wrong one is not.
	Passthrough UnresolvedMode = iota
	// ErrorOut severs the stream instead.
	ErrorOut
)

// partialRe matches a byte run that could still GROW into a complete
// placeholder. It is the holdback decision: anything matching must be withheld
// until more bytes arrive or the stream ends.
//
// Deliberately permissive — a false "could be" costs one chunk of latency on a
// few bytes, while a false "could not be" emits half a placeholder and loses the
// substitution forever.
var partialRe = regexp.MustCompile(`^\[\[AIPROXY_[A-Z]*(_[0-9a-f]{0,12}\]?)?$`)

// rewriter substitutes plaintext for placeholders in a byte stream arriving in
// arbitrary chunks.
//
// It holds back at most MaxPlaceholderBytes-1 bytes: exactly enough that a
// placeholder split across any number of writes is still recognised whole. Since
// the sentinel is "[[AIPROXY_", holdback engages only when a chunk ends
// part-way through it — in practice a trailing "[" — so ordinary prose and code
// stream through untouched and Relay's per-chunk flushing is preserved.
//
// Not safe for concurrent use. One per content block, owned by the Restorer.
type rewriter struct {
	table        *Table
	mode         UnresolvedMode
	onUnresolved func(string)
	pending      []byte
	// escape transforms a restored value before it is emitted. Identity for
	// plain text; jsonInner for input_json_delta, whose fragments sit inside a
	// JSON string literal of the tool-input document being assembled.
	escape func(string) string
}

func newRewriter(table *Table, mode UnresolvedMode, onUnresolved func(string)) *rewriter {
	return &rewriter{table: table, mode: mode, onUnresolved: onUnresolved}
}

// newJSONRewriter is newRewriter for a stream whose text is JSON string content
// one level down — input_json_delta. See restore_json.go.
func newJSONRewriter(table *Table, mode UnresolvedMode, onUnresolved func(string)) *rewriter {
	w := newRewriter(table, mode, onUnresolved)
	w.escape = jsonInner
	return w
}

// Pending is the number of bytes currently withheld. Tests assert this is zero
// for text that cannot contain a placeholder.
func (w *rewriter) Pending() int { return len(w.pending) }

// Write feeds s in and returns the text that is safe to emit now.
func (w *rewriter) Write(s string) (string, error) {
	buf := append(w.pending, s...)
	w.pending = nil

	var out bytes.Buffer
	for {
		start, end, ok := FindPlaceholder(string(buf))
		if !ok {
			break
		}
		out.Write(buf[:start])
		placeholder := string(buf[start:end])
		// buf is advanced past the placeholder BEFORE the substituted text is
		// written, and the substitution goes straight to out — so restored
		// plaintext is never rescanned. A value that happens to look like
		// another placeholder therefore cannot cause a second substitution.
		buf = buf[end:]
		if value, found := w.table.Lookup(placeholder); found {
			if w.escape != nil {
				value = w.escape(value)
			}
			out.WriteString(value)
			continue
		}
		if w.mode == ErrorOut {
			return "", fmt.Errorf("%w: %s", ErrUnresolved, placeholder)
		}
		if w.onUnresolved != nil {
			w.onUnresolved(placeholder)
		}
		out.WriteString(placeholder)
	}

	// Whatever trailing bytes could still become a placeholder are withheld.
	if i := partialStart(buf); i >= 0 {
		out.Write(buf[:i])
		w.pending = append([]byte{}, buf[i:]...)
	} else {
		out.Write(buf)
	}
	return out.String(), nil
}

// Flush emits any withheld bytes. Called when a content block ends: a partial
// placeholder at that point is just text, and dropping it would silently
// truncate the model's answer.
func (w *rewriter) Flush() (string, error) {
	out := string(w.pending)
	w.pending = nil
	return out, nil
}

// partialStart returns the index of the shortest trailing run of buf that could
// still grow into a placeholder, or -1 if none can.
//
// The search is bounded to the last MaxPlaceholderBytes-1 bytes, which is what
// makes the holdback bounded: a stream of "[" characters cannot grow the pending
// buffer without limit.
func partialStart(buf []byte) int {
	lo := len(buf) - (MaxPlaceholderBytes - 1)
	if lo < 0 {
		lo = 0
	}
	for i := lo; i < len(buf); i++ {
		if buf[i] != '[' {
			continue
		}
		if couldBecomePlaceholder(buf[i:]) {
			return i
		}
	}
	return -1
}

// couldBecomePlaceholder reports whether tail is a prefix of some complete
// placeholder.
func couldBecomePlaceholder(tail []byte) bool {
	if len(tail) <= len(Sentinel) {
		return bytes.HasPrefix([]byte(Sentinel), tail)
	}
	return partialRe.Match(tail)
}
