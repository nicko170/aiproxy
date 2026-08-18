// Package privacy detects sensitive spans in requests bound for a model
// provider, replaces them with reversible placeholders, and restores the
// originals in the response.
//
// It imports the standard library and internal/config, and nothing from
// internal/proxy — proxy calls into it. That is what lets the whole
// detect-redact-restore path be tested with no proxy, no network, and no model.
package privacy

import (
	"encoding/json"
	"errors"
	"fmt"
)

// maxWalkDepth bounds how deeply nested a document the walker will descend
// into. Past it the walk errors instead of recursing further.
//
// value/object/array are mutually recursive, so nesting depth is Go stack
// depth, and a Go stack that outgrows its limit is a FATAL runtime error: no
// panic, no recover, the whole proxy dies. That is remote input on both sides —
// a request from an agent that ingested a hostile web page, and, through
// Restorer.Body, a response from the provider — so "no real payload nests that
// deep" is not on its own a defence.
//
// 1024 is chosen to be uninteresting from either end. The deepest real
// Anthropic body is a tool schema nested a dozen or two levels, so this is two
// orders of magnitude of headroom and no legitimate request can reach it; and
// 1024 frames of a small walker is a few hundred KB of stack, nowhere near the
// runtime's 1 GB ceiling, so the limit fires long before the crash it exists to
// prevent. encoding/json's own decoder caps at 10000 for the same reason.
const maxWalkDepth = 1024

// ErrTooDeep reports a document nested past maxWalkDepth.
//
// A distinct sentinel because the caller must not treat it as "this body is not
// JSON": that shape is passed upstream UNFILTERED (see Filter.Redact), and a
// document engineered to be unwalkable is exactly the request that must not
// take the pass-through path. It is a scan failure, and obeys onScanFailure.
var ErrTooDeep = errors.New("privacy: JSON nested too deeply to scan")

// StringSpan is one JSON string VALUE found in a document.
//
// Start and End bracket the quoted literal *including both quote characters*,
// so a replacement literal produced by json.Marshal substitutes directly with
// no quote arithmetic at the call site. Value is the decoded content.
//
// Key is the object key this value sits under; for an array element it is the
// key the array itself sits under, so a rule keyed on "stop_sequences" applies
// to its elements. ParentKey is the key one level up, which is how the spec's
// "base64 data under an image source" exclusion is expressed.
type StringSpan struct {
	Start, End int
	Value      string
	Key        string
	ParentKey  string
}

// WalkStrings returns every string value in doc, in document order.
//
// Object keys are not returned as spans of their own — redacting a key would
// produce a request the provider cannot parse. They populate Key on the value
// that follows instead.
//
// This is a purpose-built scanner rather than encoding/json because
// json.Decoder.InputOffset reports the offset AFTER a token, so recovering a
// literal's start means walking backwards over escapes and counting
// backslashes; and because Token() cannot tell you which key a value belongs
// to, which the structural-key denylist needs.
func WalkStrings(doc []byte) ([]StringSpan, error) {
	w := &walker{doc: doc}
	if err := w.skipSpace(); err != nil {
		return nil, err
	}
	if err := w.value("", "", 0); err != nil {
		return nil, err
	}
	if err := w.skipSpace(); err == nil && w.i < len(w.doc) {
		return nil, fmt.Errorf("privacy: trailing bytes at offset %d", w.i)
	}
	return w.spans, nil
}

type walker struct {
	doc   []byte
	i     int
	spans []StringSpan
}

// errEOF marks input that ended mid-document. It is always wrapped before
// escaping WalkStrings, so callers see one error kind.
func (w *walker) errAt(what string) error {
	return fmt.Errorf("privacy: malformed JSON: %s at offset %d", what, w.i)
}

func (w *walker) skipSpace() error {
	for w.i < len(w.doc) {
		switch w.doc[w.i] {
		case ' ', '\t', '\n', '\r':
			w.i++
		default:
			return nil
		}
	}
	return w.errAt("unexpected end of input")
}

// value dispatches on the next non-space byte. key is the object key this
// value sits under and parent the key one level above it. depth is how many
// containers enclose this value; it is checked where the recursion happens.
func (w *walker) value(key, parent string, depth int) error {
	if err := w.skipSpace(); err != nil {
		return err
	}
	switch c := w.doc[w.i]; {
	case c == '{':
		return w.object(key, depth)
	case c == '[':
		return w.array(key, parent, depth)
	case c == '"':
		start, decoded, err := w.stringLiteral()
		if err != nil {
			return err
		}
		w.spans = append(w.spans, StringSpan{
			Start: start, End: w.i, Value: decoded, Key: key, ParentKey: parent,
		})
		return nil
	default:
		// A number, true, false, or null: consume until a structural byte. No
		// span is recorded — nothing sensitive can hide in a JSON scalar that
		// is not a string.
		return w.scalar()
	}
}

func (w *walker) object(parent string, depth int) error {
	if depth >= maxWalkDepth {
		return fmt.Errorf("%w: more than %d levels at offset %d", ErrTooDeep, maxWalkDepth, w.i)
	}
	depth++
	w.i++ // '{'
	if err := w.skipSpace(); err != nil {
		return err
	}
	if w.doc[w.i] == '}' {
		w.i++
		return nil
	}
	for {
		if err := w.skipSpace(); err != nil {
			return err
		}
		if w.doc[w.i] != '"' {
			return w.errAt("expected an object key")
		}
		_, key, err := w.stringLiteral()
		if err != nil {
			return err
		}
		if err := w.skipSpace(); err != nil {
			return err
		}
		if w.doc[w.i] != ':' {
			return w.errAt("expected ':' after an object key")
		}
		w.i++
		if err := w.value(key, parent, depth); err != nil {
			return err
		}
		if err := w.skipSpace(); err != nil {
			return err
		}
		switch w.doc[w.i] {
		case ',':
			w.i++
		case '}':
			w.i++
			return nil
		default:
			return w.errAt("expected ',' or '}'")
		}
	}
}

func (w *walker) array(key, parent string, depth int) error {
	if depth >= maxWalkDepth {
		return fmt.Errorf("%w: more than %d levels at offset %d", ErrTooDeep, maxWalkDepth, w.i)
	}
	depth++
	w.i++ // '['
	if err := w.skipSpace(); err != nil {
		return err
	}
	if w.doc[w.i] == ']' {
		w.i++
		return nil
	}
	for {
		// Elements inherit the array's own key, so a rule or denylist entry
		// keyed on that name still reaches them.
		if err := w.value(key, parent, depth); err != nil {
			return err
		}
		if err := w.skipSpace(); err != nil {
			return err
		}
		switch w.doc[w.i] {
		case ',':
			w.i++
		case ']':
			w.i++
			return nil
		default:
			return w.errAt("expected ',' or ']'")
		}
	}
}

// stringLiteral consumes a quoted literal starting at w.i and returns its start
// offset and decoded value, leaving w.i just past the closing quote.
//
// Decoding delegates to encoding/json on the literal's own bytes: escape
// handling — \uXXXX, surrogate pairs, the lot — is exactly the place to reuse
// the standard library rather than reimplement it.
func (w *walker) stringLiteral() (int, string, error) {
	start := w.i
	w.i++ // opening quote
	for w.i < len(w.doc) {
		switch w.doc[w.i] {
		case '\\':
			// Skip the escape and whatever it escapes, so an escaped quote does
			// not end the literal. \uXXXX's four hex digits are ordinary bytes
			// to this loop and are consumed by subsequent iterations.
			w.i += 2
			continue
		case '"':
			w.i++
			var decoded string
			if err := json.Unmarshal(w.doc[start:w.i], &decoded); err != nil {
				return 0, "", fmt.Errorf("privacy: malformed string literal at offset %d: %w", start, err)
			}
			return start, decoded, nil
		default:
			w.i++
		}
	}
	w.i = start
	return 0, "", w.errAt("unterminated string")
}

func (w *walker) scalar() error {
	start := w.i
	for w.i < len(w.doc) {
		switch w.doc[w.i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			if w.i == start {
				return w.errAt("expected a value")
			}
			return nil
		default:
			w.i++
		}
	}
	if w.i == start {
		return w.errAt("expected a value")
	}
	// A bare scalar at the end of input is only valid at the document root,
	// which WalkStrings' trailing-bytes check already accepts.
	return nil
}
