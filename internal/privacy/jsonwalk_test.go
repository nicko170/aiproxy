package privacy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// spanText is the raw literal the scanner claims, quotes included. Every test
// asserts on this rather than on offsets alone: an off-by-one in Start or End
// silently redacts a quote character, and the resulting body is malformed JSON
// that upstream rejects with an unhelpful error.
func spanText(doc string, s StringSpan) string { return doc[s.Start:s.End] }

func TestWalkStringsFindsValuesWithExactSpans(t *testing.T) {
	doc := `{"model":"claude-opus-5","messages":[{"role":"user","content":"hi there"}]}`
	spans, err := WalkStrings([]byte(doc))
	if err != nil {
		t.Fatalf("WalkStrings: %v", err)
	}
	want := []struct{ key, value, literal string }{
		{"model", "claude-opus-5", `"claude-opus-5"`},
		{"role", "user", `"user"`},
		{"content", "hi there", `"hi there"`},
	}
	if len(spans) != len(want) {
		t.Fatalf("got %d spans, want %d: %+v", len(spans), len(want), spans)
	}
	for i, w := range want {
		if spans[i].Key != w.key || spans[i].Value != w.value {
			t.Errorf("span %d = {Key:%q Value:%q}, want {Key:%q Value:%q}",
				i, spans[i].Key, spans[i].Value, w.key, w.value)
		}
		if got := spanText(doc, spans[i]); got != w.literal {
			t.Errorf("span %d literal = %s, want %s", i, got, w.literal)
		}
	}
}

// Escapes are where a hand-rolled scanner earns its keep: the decoded value and
// the on-the-wire literal have different lengths, and both must be right.
func TestWalkStringsDecodesEscapes(t *testing.T) {
	doc := `{"a":"line\none \"quoted\" back\\slash é"}`
	spans, err := WalkStrings([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	const wantValue = "line\none \"quoted\" back\\slash é"
	if spans[0].Value != wantValue {
		t.Errorf("Value = %q, want %q", spans[0].Value, wantValue)
	}
	// The literal must still be the raw escaped form, quotes included.
	if got := spanText(doc, spans[0]); got != `"line\none \"quoted\" back\\slash é"` {
		t.Errorf("literal = %s", got)
	}
	// And it must round-trip through the stdlib, proving the span is a valid
	// JSON literal on its own.
	var back string
	if err := json.Unmarshal([]byte(spanText(doc, spans[0])), &back); err != nil {
		t.Fatalf("span is not a valid JSON literal: %v", err)
	}
	if back != wantValue {
		t.Errorf("round trip = %q", back)
	}
}

// An escaped quote inside a string must not end it. This is the single most
// likely scanner bug and it corrupts every span after it.
func TestWalkStringsDoesNotEndOnEscapedQuote(t *testing.T) {
	doc := `{"a":"he said \"no\"","b":"second"}`
	spans, err := WalkStrings([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %+v", len(spans), spans)
	}
	if spans[1].Key != "b" || spans[1].Value != "second" {
		t.Errorf("second span = %+v", spans[1])
	}
}

// ParentKey is how the spec's "base64 image data" exclusion is expressed: the
// key is "data", nested under "source".
func TestWalkStringsReportsParentKey(t *testing.T) {
	doc := `{"content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}`
	spans, err := WalkStrings([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range spans {
		if s.Key == "data" {
			found = true
			if s.ParentKey != "source" {
				t.Errorf("ParentKey = %q, want source", s.ParentKey)
			}
		}
	}
	if !found {
		t.Fatal("no span for the data key")
	}
}

// Array elements have no key of their own; they inherit the array's key so a
// denylist entry still applies to them.
func TestWalkStringsArrayElementsInheritTheArrayKey(t *testing.T) {
	doc := `{"stop_sequences":["END","STOP"]}`
	spans, err := WalkStrings([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	for _, s := range spans {
		if s.Key != "stop_sequences" {
			t.Errorf("Key = %q, want stop_sequences", s.Key)
		}
	}
}

func TestWalkStringsRejectsMalformedInput(t *testing.T) {
	for _, doc := range []string{`{"a":`, `{"a":"unterminated`, `{`, ``, `{"a":"x"`} {
		if _, err := WalkStrings([]byte(doc)); err == nil {
			t.Errorf("WalkStrings(%q) succeeded, want an error", doc)
		}
	}
}

// A realistic body must be walkable and every span must be a valid literal.
// This is the invariant the splice in Task 6 depends on.
func TestWalkStringsEverySpanIsAValidLiteral(t *testing.T) {
	doc := `{"model":"claude-opus-5","system":[{"type":"text","text":"be terse"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"AKIAIOSFODNN7EXAMPLE"},` +
		`{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}],"max_tokens":1024}`
	spans, err := WalkStrings([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) == 0 {
		t.Fatal("no spans")
	}
	for _, s := range spans {
		lit := spanText(doc, s)
		if !strings.HasPrefix(lit, `"`) || !strings.HasSuffix(lit, `"`) {
			t.Errorf("span %+v literal %s is not quoted", s, lit)
		}
		var back string
		if err := json.Unmarshal([]byte(lit), &back); err != nil {
			t.Errorf("span %+v literal %s does not unmarshal: %v", s, lit, err)
		} else if back != s.Value {
			t.Errorf("span %+v decoded to %q", s, back)
		}
	}
}

// FuzzWalkStrings asserts the two properties the splice in Task 6 relies on:
// the scanner never panics, and every span it returns is a valid JSON string
// literal whose decoded form matches the reported Value. Anything else and a
// redaction produces a body the provider cannot parse.
func FuzzWalkStrings(f *testing.F) {
	f.Add([]byte(`{"a":"b"}`))
	f.Add([]byte(`{"a":["x",{"b":"c\"d"}]}`))
	f.Add([]byte(`{"a":"é😀"}`))
	f.Add([]byte(`[1,2,{"k":"v"}]`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		spans, err := WalkStrings(doc)
		if err != nil {
			return // malformed input is a legitimate outcome
		}
		for _, s := range spans {
			if s.Start < 0 || s.End > len(doc) || s.Start >= s.End {
				t.Fatalf("span %+v out of range for %d bytes", s, len(doc))
			}
			var back string
			if err := json.Unmarshal(doc[s.Start:s.End], &back); err != nil {
				t.Fatalf("span %+v is not a valid literal: %v", s, err)
			}
			if back != s.Value {
				t.Fatalf("span %+v decodes to %q", s, back)
			}
		}
	})
}

// nested builds a document of n containers wrapped around one string, e.g.
// [[["x"]]] for n=3.
func nested(n int) []byte {
	return []byte(strings.Repeat("[", n) + `"x"` + strings.Repeat("]", n))
}

// value/object/array are mutually recursive, so nesting depth IS Go stack
// depth, and a Go stack past its limit is a fatal runtime error that no recover
// can catch — the whole proxy dies. Both directions feed this walker remote
// input: a request body from an agent that read a hostile page, and, through
// Restorer.Body, a response body from the provider. So a pathological document
// has to come back as an error rather than as a crash.
func TestWalkStringsRefusesPathologicallyNestedDocuments(t *testing.T) {
	_, err := WalkStrings(nested(maxWalkDepth * 8))
	if err == nil {
		t.Fatal("WalkStrings accepted a document nested far past the limit")
	}
	if !errors.Is(err, ErrTooDeep) {
		t.Errorf("err = %v, want it to wrap ErrTooDeep so callers can tell it from malformed JSON", err)
	}
	// Not ErrNotJSON: that sentinel routes a body UPSTREAM UNFILTERED, which is
	// the one thing a document engineered to defeat the walker must not get.
	if errors.Is(err, ErrNotJSON) {
		t.Error("a too-deep document reported ErrNotJSON, which is the pass-through path")
	}
}

// The bound has to be generous enough that no real payload can trip it; a
// document at the limit still walks normally.
func TestWalkStringsAcceptsNestingUpToTheLimit(t *testing.T) {
	spans, err := WalkStrings(nested(maxWalkDepth))
	if err != nil {
		t.Fatalf("WalkStrings(%d levels) = %v, want it accepted", maxWalkDepth, err)
	}
	if len(spans) != 1 || spans[0].Value != "x" {
		t.Errorf("spans = %+v, want the one string at the bottom", spans)
	}
}
