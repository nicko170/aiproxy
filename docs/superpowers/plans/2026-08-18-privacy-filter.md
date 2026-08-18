# Local Privacy Filter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Detect sensitive spans in requests locally, replace them with stable placeholders before they reach Anthropic, and restore the originals in the response stream so the coding agent never sees a difference.

**Architecture:** A purpose-built JSON string-literal scanner yields byte spans and decoded values from the buffered request body; detectors (a deterministic rule table, and later an ONNX token classifier) report findings on the decoded values; findings become placeholders derived from a keyed hash of the value, spliced back into the original bytes last-span-first so nothing else moves. On the way out, a per-content-block streaming rewriter inside `Relay` holds back at most 39 bytes and substitutes plaintext back, including inside `input_json_delta` where the agent's file writes live. Tier 2 adds `openai/privacy-filter` in-process through a vendored purego binding to ONNX Runtime, gated behind a tokenizer that must reproduce reference offsets exactly.

**Tech Stack:** Go 1.26.5, standard library, plus two pure-Go dependencies for Tier 2 only: `github.com/dlclark/regexp2` (the o200k pretokenizer pattern needs lookahead, which Go's RE2 does not support) and a vendored copy of `github.com/shota3506/onnxruntime-purego`. `CGO_ENABLED=0` is preserved throughout.

**Spec:** `docs/superpowers/specs/2026-08-18-privacy-filter-design.md`

## Global Constraints

- **`CGO_ENABLED=0` must keep working.** The release workflow cross-compiles four targets from one runner, and `install.sh` plus the self-updater depend on that. Tier 2 reaches ONNX Runtime through `purego`, never cgo.
- **Redact-then-restore is the identity.** For any input, the bytes the agent receives equal the bytes upstream would have produced unredacted. A botched restore corrupts a file on the operator's disk, which is worse than the leak this prevents.
- **A stream with no `[[AIPROXY_` sentinel is emitted byte-for-byte with no added buffering.** `Relay` exists to flush every chunk as it arrives.
- **Nothing outside a replaced span is altered.** No re-serialization, no key reordering, no whitespace changes. Splices are applied last-span-first.
- **No plaintext sensitive value is written to disk or retained beyond the request that carried it.** The scan cache holds byte offsets and labels, never values.
- **A collision never restores the wrong value.** Widen to 12 hex, or fail.
- **`maxPlaceholderBytes = 40`** — one constant shared by the recogniser and the streaming rewriter. Longest actual form is 33 bytes: `[[AIPROXY_` (10) + label ≤8 + `_` (1) + 12 hex + `]]` (2).
- **Placeholder grammar:** `\[\[AIPROXY_[A-Z]+_[0-9a-f]{8,12}\]\]`. Labels: `SECRET`, `EMAIL`, `PHONE`, `ADDRESS`, `PERSON`, `URL`, `DATE`, `ACCOUNT`, `ID`.
- **The filter is off by default** (`privacy.enabled: false`). Enabling it turns on the deterministic rules; every NER label is individually opt-in with an empty default set.
- **Never filter passthrough paths** (`proxy.DefaultPassthroughPrefixes`): they carry the client's own OAuth credential.
- Run the suite with `go test ./... -race` from the repo root. `gofmt -l .` must print nothing; `go vet ./...` and `staticcheck ./...` must pass — CI runs all three.

## File Structure

**Created — `internal/privacy` (pipeline, no proxy dependency):**
- `jsonwalk.go` — the JSON string-literal scanner: spans, decoded values, key context.
- `placeholder.go` — install key, keyed hash, minting, the recogniser, `maxPlaceholderBytes`.
- `table.go` — the per-request restore table and collision widening.
- `detector.go` — the `Detector` interface, `Finding`, overlap resolution.
- `redact.go` — the request-side pipeline: walk, scan, resolve, splice.
- `restore.go` — the streaming rewriter (`text_delta`, `thinking_delta`, `content_block_start`).
- `restore_json.go` — `input_json_delta` and its two escaping levels.
- `sse.go` — minimal SSE event framing and re-emission shared by the two above.
- `cache.go` — the bounded LRU of findings, keyed with the version salt.
- `filter.go` — `Filter`: the object `internal/proxy` holds, assembling the above from config.

**Created — `internal/privacy/rules` (Tier 1 detectors):**
- `rules.go` — the rule table and the regex detector.
- `entropy.go` — the Shannon-entropy qualifier.
- `allow.go` — the false-positive allowlist.
- `denylist.go` — the operator denylist detector.

**Created — Tier 2:**
- `internal/privacy/tokenizer/` — `tokenizer.go` (byte-level BPE over the model's own `tokenizer.json`), `testdata/offsets.json` (reference fixtures).
- `internal/privacy/onnxrt/` — vendored purego ONNX Runtime binding.
- `internal/privacy/ner/` — `ner.go` (the `Detector`), `viterbi.go` (constrained BIOES decode), `assets.go` (pinned download + verify).

**Modified:**
- `internal/proxy/handler.go` — redact between the blocked-model check and `o.Attempter.Do`.
- `internal/proxy/attempt.go` — carry `Request.Restore` into `RelayOptions`.
- `internal/proxy/relay.go` — `RelayOptions.Restore`, applied as a stream transform.
- `internal/config/config.go`, `store.go` — the `privacy` block and its defaults.
- `internal/view/types.go`, `local.go` — `Status.Privacy`, the new `Settings` fields.
- `internal/tui/app.go`, `activity.go`, `settings.go`, `frames_test.go` — header segment, per-request counts, settings rows, goldens.
- `cmd/aiproxy/main.go`, `update.go` — construct the `Filter`; add the `privacy install` subcommand.
- `README.md`, `go.mod`.

---

### Task 1: The JSON string-literal scanner

**Files:**
- Create: `internal/privacy/jsonwalk.go`
- Test: `internal/privacy/jsonwalk_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type StringSpan struct { Start, End int; Value, Key, ParentKey string }` — `Start`/`End` bracket the **quoted literal including both quotes**, so a replacement literal substitutes directly.
  - `func WalkStrings(doc []byte) ([]StringSpan, error)` — every string **value** in document order. Object keys are not returned as spans; they populate `Key` on the value that follows.

**Why a purpose-built scanner rather than `encoding/json`:** `json.Decoder.Token()` reports values but `InputOffset()` only gives the offset *after* a token, so recovering the literal's start means scanning backwards over escapes and counting backslashes. A 70-line scanner gives exact spans directly, and it is the only way to know each value's *key* — which §4.1 of the spec needs for the structural-key denylist.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/jsonwalk_test.go`:

```go
package privacy

import (
	"encoding/json"
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run TestWalkStrings`
Expected: FAIL — the package does not build, `undefined: StringSpan`, `undefined: WalkStrings`.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/jsonwalk.go`:

```go
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
	"fmt"
)

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
	if err := w.value("", ""); err != nil {
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
// value sits under and parent the key one level above it.
func (w *walker) value(key, parent string) error {
	if err := w.skipSpace(); err != nil {
		return err
	}
	switch c := w.doc[w.i]; {
	case c == '{':
		return w.object(key)
	case c == '[':
		return w.array(key, parent)
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

func (w *walker) object(parent string) error {
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
		if err := w.value(key, parent); err != nil {
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

func (w *walker) array(key, parent string) error {
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
		if err := w.value(key, parent); err != nil {
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v`
Expected: PASS — all seven `TestWalkStrings*` tests.

- [ ] **Step 5: Add a fuzz target and run it briefly**

Append to `internal/privacy/jsonwalk_test.go`:

```go
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
```

Run: `go test ./internal/privacy -run FuzzWalkStrings -fuzz FuzzWalkStrings -fuzztime 30s`
Expected: no failures. If the fuzzer finds a crasher it writes it to `testdata/fuzz/`; fix the scanner and keep the corpus file — do not delete it.

- [ ] **Step 6: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): scan JSON string values with exact byte spans"
```

---

### Task 2: Placeholders and the install key

**Files:**
- Create: `internal/privacy/placeholder.go`
- Test: `internal/privacy/placeholder_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `const MaxPlaceholderBytes = 40`, `const Sentinel = "[[AIPROXY_"`
  - `type Label string` with `LabelSecret`, `LabelEmail`, `LabelPhone`, `LabelAddress`, `LabelPerson`, `LabelURL`, `LabelDate`, `LabelAccount`, `LabelID`
  - `func Mint(key []byte, label Label, value string, hexLen int) string`
  - `func IsPlaceholder(s string) bool`
  - `func FindPlaceholder(s string) (start, end int, ok bool)` — leftmost match, used by the streaming rewriter
  - `func LoadOrCreateKey(path string) ([]byte, error)`
  - `func KeyPath() string` — `~/.config/aiproxy/privacy.key`

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/placeholder_test.go`:

```go
package privacy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMintIsStableAndKeyed(t *testing.T) {
	k1 := []byte("0123456789abcdef0123456789abcdef")
	k2 := []byte("fedcba9876543210fedcba9876543210")

	a := Mint(k1, LabelSecret, "AKIAIOSFODNN7EXAMPLE", 8)
	b := Mint(k1, LabelSecret, "AKIAIOSFODNN7EXAMPLE", 8)
	if a != b {
		t.Errorf("minting is not stable: %q vs %q — a prompt cache prefix depends on this", a, b)
	}
	if c := Mint(k2, LabelSecret, "AKIAIOSFODNN7EXAMPLE", 8); c == a {
		t.Error("the install key does not affect the placeholder; it must, or a known-format value is brute-forceable")
	}
	if d := Mint(k1, LabelSecret, "AKIAIOSFODNN7DIFFERENT", 8); d == a {
		t.Error("two different values minted the same placeholder")
	}
}

func TestMintFormat(t *testing.T) {
	got := Mint([]byte("0123456789abcdef0123456789abcdef"), LabelEmail, "a@b.example", 8)
	if !strings.HasPrefix(got, Sentinel) {
		t.Errorf("%q does not start with the sentinel %q", got, Sentinel)
	}
	if !strings.HasPrefix(got, "[[AIPROXY_EMAIL_") || !strings.HasSuffix(got, "]]") {
		t.Errorf("format = %q", got)
	}
	if !IsPlaceholder(got) {
		t.Errorf("%q is not recognised by IsPlaceholder", got)
	}
}

// The longest form must fit the constant the streaming rewriter budgets its
// holdback against. If this ever fails, a placeholder can cross more bytes than
// the rewriter withholds and restoration silently misses it.
func TestLongestPlaceholderFitsTheBudget(t *testing.T) {
	longest := 0
	for _, l := range AllLabels() {
		got := Mint([]byte("0123456789abcdef0123456789abcdef"), l, "x", 12)
		if len(got) > longest {
			longest = len(got)
		}
	}
	if longest > MaxPlaceholderBytes {
		t.Fatalf("longest placeholder is %d bytes, budget is %d", longest, MaxPlaceholderBytes)
	}
	t.Logf("longest placeholder is %d bytes of a %d budget", longest, MaxPlaceholderBytes)
}

func TestIsPlaceholderRejectsNearMisses(t *testing.T) {
	for _, s := range []string{
		"", "[[AIPROXY_]]", "[[AIPROXY_SECRET_]]",
		"[[AIPROXY_SECRET_XYZ12345]]",       // hex only
		"[[AIPROXY_secret_a1b2c3d4]]",       // label is uppercase
		"[[AIPROXY_SECRET_a1b2c3]]",         // too short
		"[[AIPROXY_SECRET_a1b2c3d4e5f6a7]]", // too long
		"[AIPROXY_SECRET_a1b2c3d4]",         // single brackets
		"[[AIPROXY_SECRET_a1b2c3d4]",        // unbalanced
	} {
		if IsPlaceholder(s) {
			t.Errorf("IsPlaceholder(%q) = true, want false", s)
		}
	}
}

func TestFindPlaceholderReturnsLeftmostMatch(t *testing.T) {
	s := "before [[AIPROXY_SECRET_a1b2c3d4]] middle [[AIPROXY_EMAIL_00112233]] after"
	start, end, ok := FindPlaceholder(s)
	if !ok {
		t.Fatal("FindPlaceholder found nothing")
	}
	if got := s[start:end]; got != "[[AIPROXY_SECRET_a1b2c3d4]]" {
		t.Errorf("found %q, want the leftmost placeholder", got)
	}
	if _, _, ok := FindPlaceholder("nothing here at all"); ok {
		t.Error("FindPlaceholder matched a string with no placeholder")
	}
}

func TestLoadOrCreateKeyIsStableAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.key")

	first, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if len(first) != 32 {
		t.Errorf("key is %d bytes, want 32", len(first))
	}

	second, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Error("the key changed between calls; every placeholder in every cached prefix would change with it")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode is %v, want 0600", perm)
	}
}

func TestLoadOrCreateKeyRejectsAShortKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("a truncated key file was accepted; a weak key defeats the point of keying the hash")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run 'TestMint|TestIsPlaceholder|TestFindPlaceholder|TestLoadOrCreateKey|TestLongestPlaceholder'`
Expected: FAIL — `undefined: Mint`, `undefined: Sentinel`, and so on.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/placeholder.go`:

```go
package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/nicko170/aiproxy/internal/config"
)

// Sentinel opens every placeholder. The streaming rewriter withholds bytes only
// when a chunk ends part-way through this prefix, so its rarity in ordinary
// prose and code is what keeps per-chunk flushing intact.
const Sentinel = "[[AIPROXY_"

// MaxPlaceholderBytes bounds every placeholder, and therefore bounds the
// streaming rewriter's holdback at MaxPlaceholderBytes-1. The longest actual
// form is 33 bytes — Sentinel (10) + label (<=8) + "_" (1) + 12 hex + "]]" (2)
// — and the slack is deliberate headroom so that adding a longer label is a
// test failure (TestLongestPlaceholderFitsTheBudget) rather than a silent
// stream bug.
const MaxPlaceholderBytes = 40

// keyBytes is the install key's length. 32 bytes of HMAC-SHA256 key is far more
// than the 32-48 bits of output actually used; the cost is nothing and it
// removes key length as a thing to think about.
const keyBytes = 32

// Label selects a placeholder's middle segment. It is included so the model can
// still reason about what was removed — "update the email address" is possible
// with [[AIPROXY_EMAIL_...]] and guesswork with an opaque blob.
type Label string

const (
	LabelSecret  Label = "SECRET"
	LabelEmail   Label = "EMAIL"
	LabelPhone   Label = "PHONE"
	LabelAddress Label = "ADDRESS"
	LabelPerson  Label = "PERSON"
	LabelURL     Label = "URL"
	LabelDate    Label = "DATE"
	LabelAccount Label = "ACCOUNT"
	LabelID      Label = "ID"
)

// AllLabels is every label, so a test can assert the longest placeholder fits
// MaxPlaceholderBytes without that list being maintained in two places.
func AllLabels() []Label {
	return []Label{
		LabelSecret, LabelEmail, LabelPhone, LabelAddress, LabelPerson,
		LabelURL, LabelDate, LabelAccount, LabelID,
	}
}

// placeholderRe matches a complete placeholder. The hex run is 8 to 12
// characters: 8 normally, widened to 12 on the collision path (see Table.Add).
var placeholderRe = regexp.MustCompile(`\[\[AIPROXY_[A-Z]+_[0-9a-f]{8,12}\]\]`)

// IsPlaceholder reports whether s is exactly one placeholder and nothing else.
func IsPlaceholder(s string) bool {
	loc := placeholderRe.FindStringIndex(s)
	return loc != nil && loc[0] == 0 && loc[1] == len(s)
}

// FindPlaceholder returns the leftmost placeholder's bounds in s.
func FindPlaceholder(s string) (int, int, bool) {
	loc := placeholderRe.FindStringIndex(s)
	if loc == nil {
		return 0, 0, false
	}
	return loc[0], loc[1], true
}

// Mint derives the placeholder for value under label, using hexLen hex
// characters of HMAC-SHA256(key, value).
//
// Two properties are load-bearing. The same value always yields the same
// placeholder, so a redacted prefix is byte-identical across turns and the
// provider's prompt cache still hits — a positional counter would renumber as
// content shifts and quietly multiply cost. And the hash is KEYED, because an
// AWS access key ID carries only ~20 bits of entropy after its fixed prefix and
// an unkeyed digest of one is brute-forceable from the placeholder alone.
//
// hexLen is 8 or 12; anything else is clamped into that range rather than
// producing a placeholder the recogniser would reject.
func Mint(key []byte, label Label, value string, hexLen int) string {
	if hexLen < 8 {
		hexLen = 8
	}
	if hexLen > 12 {
		hexLen = 12
	}
	mac := hmac.New(sha256.New, key)
	// The label is part of the input, so the same bytes classified two ways
	// cannot collide into one placeholder.
	mac.Write([]byte(label))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	sum := hex.EncodeToString(mac.Sum(nil))
	return Sentinel + string(label) + "_" + sum[:hexLen] + "]]"
}

// KeyPath is where the install key lives: beside the config, not inside it.
// config.json is rendered on the Settings screen and rewritten on every
// mutation, and a key belongs in neither.
func KeyPath() string { return filepath.Join(config.Dir(), "privacy.key") }

// LoadOrCreateKey reads the install key, generating it on first use.
//
// A rotated key changes every placeholder, which invalidates every cached
// prefix at the provider. That is survivable but expensive, so the key is
// written once and never touched again — and a file that exists but is the
// wrong length is an error rather than something to silently replace, because
// replacing it is the expensive outcome.
func LoadOrCreateKey(path string) ([]byte, error) {
	switch b, err := os.ReadFile(path); {
	case err == nil:
		if len(b) != keyBytes {
			return nil, fmt.Errorf("privacy: %s holds %d bytes, want %d; "+
				"delete it to regenerate, accepting that every placeholder changes", path, len(b), keyBytes)
		}
		return b, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("privacy: read install key: %w", err)
	}

	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("privacy: generate install key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("privacy: create config dir: %w", err)
	}
	// Written through a temp file and renamed, for the same reason
	// config.Store does it: a crash mid-write must not leave a truncated key
	// that the check above would then refuse on every subsequent start.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".privacy.key-*")
	if err != nil {
		return nil, fmt.Errorf("privacy: create install key: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, fmt.Errorf("privacy: install key: %w", err)
	}
	return key, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v -run 'TestMint|TestIsPlaceholder|TestFindPlaceholder|TestLoadOrCreateKey|TestLongestPlaceholder'`
Expected: PASS. `TestLongestPlaceholderFitsTheBudget` logs the actual longest length — confirm it reads 33.

- [ ] **Step 5: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): mint stable keyed placeholders"
```

---

### Task 3: The restore table and collision widening

**Files:**
- Create: `internal/privacy/table.go`
- Test: `internal/privacy/table_test.go`

**Interfaces:**
- Consumes: `Mint`, `Label`, `IsPlaceholder` (Task 2).
- Produces:
  - `type Table struct{ ... }`, `func NewTable(key []byte) *Table`
  - `func (t *Table) Add(label Label, value string) (string, error)` — idempotent for the same `(label, value)`
  - `func (t *Table) Lookup(placeholder string) (string, bool)`
  - `func (t *Table) Len() int`
  - `var ErrCollision = errors.New(...)`

The table is **per request**. The model can only echo a placeholder it was shown, and everything it was shown was in the request just redacted — so the table built during redaction is complete by construction, lives in memory for the life of the request, and is dropped when the response completes. There is no persistence and no encrypted store to design.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/table_test.go`:

```go
package privacy

import (
	"errors"
	"strings"
	"testing"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestTableAddIsIdempotent(t *testing.T) {
	tab := NewTable(testKey)
	a, err := tab.Add(LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	if err != nil {
		t.Fatal(err)
	}
	b, err := tab.Add(LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("the same value minted two placeholders: %q and %q", a, b)
	}
	if tab.Len() != 1 {
		t.Errorf("Len = %d, want 1", tab.Len())
	}
	if got, ok := tab.Lookup(a); !ok || got != "AKIAIOSFODNN7EXAMPLE" {
		t.Errorf("Lookup(%q) = %q, %v", a, got, ok)
	}
}

func TestTableLookupMisses(t *testing.T) {
	tab := NewTable(testKey)
	if _, ok := tab.Lookup("[[AIPROXY_SECRET_deadbeef]]"); ok {
		t.Error("Lookup succeeded for a placeholder never added")
	}
}

// The collision path is the one place a bug would restore the WRONG secret, so
// it is forced rather than hoped for: two values whose 8-hex suffixes are made
// to agree must both round-trip.
func TestTableWidensOnCollision(t *testing.T) {
	tab := NewTable(testKey)
	first, second, ok := findColliding8Hex(t, tab)
	if !ok {
		t.Skip("no 8-hex collision found in the search budget")
	}

	p1, err := tab.Add(LabelSecret, first)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := tab.Add(LabelSecret, second)
	if err != nil {
		t.Fatalf("Add on a colliding value failed: %v", err)
	}
	if p1 == p2 {
		t.Fatal("two distinct values share one placeholder; restoration would return the wrong secret")
	}
	if got, _ := tab.Lookup(p1); got != first {
		t.Errorf("Lookup(p1) = %q, want %q", got, first)
	}
	if got, _ := tab.Lookup(p2); got != second {
		t.Errorf("Lookup(p2) = %q, want %q", got, second)
	}
	if !strings.Contains(p2, "_") || len(p2) <= len(p1) {
		t.Errorf("the colliding placeholder was not widened: %q vs %q", p2, p1)
	}
}

// findColliding8Hex searches for two distinct values whose 8-hex placeholders
// match under testKey. 32 bits means a birthday collision turns up within a few
// hundred thousand candidates; the budget is generous and the test skips rather
// than fails if it does not, so a slow machine never sees a red suite.
func findColliding8Hex(t *testing.T, tab *Table) (string, string, bool) {
	t.Helper()
	seen := make(map[string]string, 1<<20)
	for i := 0; i < 4_000_000; i++ {
		v := "candidate-" + itoa(i)
		p := Mint(testKey, LabelSecret, v, 8)
		if prev, ok := seen[p]; ok && prev != v {
			return prev, v, true
		}
		seen[p] = v
	}
	return "", "", false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// A 12-hex collision is only reachable by deliberate construction, and the
// answer is to fail rather than to guess. This asserts the error is reported
// rather than silently overwriting a mapping.
func TestTableReportsAnUnresolvableCollision(t *testing.T) {
	tab := NewTable(testKey)
	// Force the condition directly: pre-seed both the 8- and 12-hex forms for a
	// value other than the one being added.
	v := "the-real-value"
	tab.forceForTest(Mint(testKey, LabelSecret, v, 8), "something-else")
	tab.forceForTest(Mint(testKey, LabelSecret, v, 12), "something-else-again")

	if _, err := tab.Add(LabelSecret, v); !errors.Is(err, ErrCollision) {
		t.Fatalf("err = %v, want ErrCollision", err)
	}
}

func TestTableAddProducesRecognisablePlaceholders(t *testing.T) {
	tab := NewTable(testKey)
	for _, l := range AllLabels() {
		p, err := tab.Add(l, "value-for-"+string(l))
		if err != nil {
			t.Fatal(err)
		}
		if !IsPlaceholder(p) {
			t.Errorf("Add returned %q, which IsPlaceholder rejects", p)
		}
		if len(p) > MaxPlaceholderBytes {
			t.Errorf("%q is %d bytes, over the %d budget", p, len(p), MaxPlaceholderBytes)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run TestTable`
Expected: FAIL — `undefined: NewTable`, `undefined: ErrCollision`.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/table.go`:

```go
package privacy

import (
	"errors"
	"fmt"
)

// ErrCollision reports that two distinct values mint the same placeholder at
// both 8 and 12 hex characters. Reaching it requires deliberate construction;
// the response is to fail the request, because the alternative is restoring one
// value where the other belongs.
var ErrCollision = errors.New("privacy: placeholder collision")

// Table maps placeholders to the plaintext they replaced, for the life of ONE
// request.
//
// It is per-request by construction, not by policy: the model can only
// reference a placeholder it was shown, and everything it was shown was in the
// request this table was built from — including any prefix served from the
// provider's cache, since cache_control avoids re-billing content we still
// sent. So the table is complete when redaction finishes, and nothing needs to
// outlive the response. That is what keeps plaintext off disk entirely.
//
// Not safe for concurrent use. One request, one goroutine chain, one table.
type Table struct {
	key []byte
	// byPlaceholder is the restore direction the response path reads.
	byPlaceholder map[string]string
	// byValue makes Add idempotent without re-minting, and is what lets the
	// same secret appearing twenty times in a body produce one entry.
	byValue map[valueKey]string
}

type valueKey struct {
	label Label
	value string
}

func NewTable(key []byte) *Table {
	return &Table{
		key:           key,
		byPlaceholder: map[string]string{},
		byValue:       map[valueKey]string{},
	}
}

// Add returns the placeholder for value, minting it on first sight.
//
// On collision — the placeholder already maps to a DIFFERENT value — the
// suffix widens from 8 hex to 12 and the check repeats. If the wide form
// collides too, Add fails with ErrCollision rather than overwriting a mapping:
// restoring the wrong secret is never an acceptable outcome, and a request that
// fails loudly is recoverable.
func (t *Table) Add(label Label, value string) (string, error) {
	vk := valueKey{label: label, value: value}
	if p, ok := t.byValue[vk]; ok {
		return p, nil
	}
	for _, hexLen := range [...]int{8, 12} {
		p := Mint(t.key, label, value, hexLen)
		switch existing, taken := t.byPlaceholder[p]; {
		case !taken:
			t.byPlaceholder[p] = value
			t.byValue[vk] = p
			return p, nil
		case existing == value:
			// Same value reached by a different label path; reuse it.
			t.byValue[vk] = p
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: %s collides at 8 and 12 hex", ErrCollision, label)
}

// Lookup returns the plaintext a placeholder stands for.
func (t *Table) Lookup(placeholder string) (string, bool) {
	v, ok := t.byPlaceholder[placeholder]
	return v, ok
}

// Len is the number of distinct placeholders minted, which the TUI reports as
// the per-request redaction count.
func (t *Table) Len() int { return len(t.byPlaceholder) }

// forceForTest seeds a mapping directly, so the collision path can be exercised
// without searching for a 12-hex collision that does not naturally occur.
func (t *Table) forceForTest(placeholder, value string) {
	t.byPlaceholder[placeholder] = value
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v -run TestTable`
Expected: PASS. `TestTableWidensOnCollision` may take a few seconds while it searches for a 32-bit collision; if it skips on a slow machine that is acceptable, but confirm it passes at least once locally so the widening path is genuinely covered.

- [ ] **Step 5: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): per-request restore table with collision widening"
```

---

### Task 4: The Detector contract and overlap resolution

**Files:**
- Create: `internal/privacy/detector.go`
- Test: `internal/privacy/detector_test.go`

**Interfaces:**
- Consumes: `Label` (Task 2).
- Produces:
  - `type Finding struct { Start, End int; Label Label; Rule string; Confidence float64 }`
  - `type Detector interface { Name() string; Scan(ctx context.Context, text string) ([]Finding, error) }`
  - `func Resolve(perDetector [][]Finding) []Finding`

`Resolve` takes results **grouped by detector, in registration order**, rather than one flat slice. That makes the spec's third sort key structural: the outer index *is* registration order, so no `Finding` field has to carry it and no detector can lie about its own priority.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/detector_test.go`:

```go
package privacy

import (
	"reflect"
	"testing"
)

func TestResolveSortsByPosition(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 20, End: 30, Label: LabelEmail, Rule: "b"},
		{Start: 0, End: 10, Label: LabelSecret, Rule: "a"},
	}})
	if len(got) != 2 || got[0].Start != 0 || got[1].Start != 20 {
		t.Fatalf("Resolve did not order by position: %+v", got)
	}
}

// The longer span wins: a connection string is more useful to redact whole than
// to redact the password inside it and leave the host and user behind.
func TestResolvePrefersTheLongerOverlappingSpan(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 10, End: 20, Label: LabelSecret, Rule: "password"},
		{Start: 0, End: 40, Label: LabelSecret, Rule: "connection-string"},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if got[0].Rule != "connection-string" {
		t.Errorf("kept %q, want connection-string", got[0].Rule)
	}
}

// Two detectors reporting the identical span must always resolve the same way,
// or the same body redacts differently on different runs and the prompt cache
// stops hitting.
func TestResolveBreaksIdenticalSpansByRegistrationOrder(t *testing.T) {
	first := []Finding{{Start: 5, End: 15, Label: LabelSecret, Rule: "rules"}}
	second := []Finding{{Start: 5, End: 15, Label: LabelPerson, Rule: "ner"}}

	got := Resolve([][]Finding{first, second})
	if len(got) != 1 || got[0].Rule != "rules" {
		t.Fatalf("expected the first-registered detector to win, got %+v", got)
	}
	// Registering them the other way round must flip the winner — proving the
	// tiebreak is registration order and not something incidental like the
	// label's or rule's spelling.
	got = Resolve([][]Finding{second, first})
	if len(got) != 1 || got[0].Rule != "ner" {
		t.Fatalf("registration order is not the tiebreak, got %+v", got)
	}
}

func TestResolveKeepsAdjacentNonOverlappingSpans(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 0, End: 10, Label: LabelSecret, Rule: "a"},
		{Start: 10, End: 20, Label: LabelEmail, Rule: "b"},
	}})
	if len(got) != 2 {
		t.Fatalf("adjacent spans must both survive: %+v", got)
	}
}

func TestResolveDropsEmptyAndInvertedSpans(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 5, End: 5, Rule: "empty"},
		{Start: 9, End: 4, Rule: "inverted"},
		{Start: 0, End: 3, Rule: "good"},
	}})
	want := []Finding{{Start: 0, End: 3, Rule: "good"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolveOnNothing(t *testing.T) {
	if got := Resolve(nil); len(got) != 0 {
		t.Errorf("Resolve(nil) = %+v", got)
	}
	if got := Resolve([][]Finding{{}, {}}); len(got) != 0 {
		t.Errorf("Resolve of empty groups = %+v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run TestResolve`
Expected: FAIL — `undefined: Resolve`, `undefined: Finding`.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/detector.go`:

```go
package privacy

import (
	"context"
	"sort"
)

// Finding is one sensitive span within a single decoded string. Offsets are
// byte offsets into that string, not into the request body: detectors never see
// the body, only values handed to them one at a time.
type Finding struct {
	Start, End int
	Label      Label
	// Rule names what fired, for the Activity feed and the logs. It is
	// diagnostic, never load-bearing.
	Rule string
	// Confidence is 1.0 for deterministic rules and the model's score for NER
	// findings. Nothing filters on it today; it exists so a future threshold is
	// a config change rather than an interface change.
	Confidence float64
}

// Detector finds sensitive spans in one decoded string.
//
// Implementations must be safe for concurrent use: one Filter is shared by every
// in-flight request. They must also be pure with respect to the input — the
// scan cache keys on content alone, so a detector that returned different
// findings for the same text would produce results that depend on cache state.
type Detector interface {
	Name() string
	Scan(ctx context.Context, text string) ([]Finding, error)
}

// Resolve flattens per-detector results into one ordered, non-overlapping set.
//
// perDetector is grouped BY DETECTOR IN REGISTRATION ORDER, and that ordering is
// the point: it is the third sort key, so two detectors reporting the identical
// span always resolve the same way. Were it a field on Finding, a detector could
// misreport its own priority and the same body would redact differently between
// runs — which shows up as a collapsed prompt-cache hit rate rather than as an
// obvious bug.
//
// Sort is start ascending, then length descending, then registration order. The
// longer span wins an overlap: the whole connection string rather than the
// password inside it.
func Resolve(perDetector [][]Finding) []Finding {
	type ranked struct {
		f   Finding
		idx int
	}
	var all []ranked
	for i, group := range perDetector {
		for _, f := range group {
			if f.End <= f.Start {
				// An empty or inverted span is a detector bug. Dropping it here
				// keeps every consumer downstream free of the check.
				continue
			}
			all = append(all, ranked{f: f, idx: i})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.f.Start != b.f.Start {
			return a.f.Start < b.f.Start
		}
		if a.f.End != b.f.End {
			return a.f.End > b.f.End // longer first
		}
		return a.idx < b.idx
	})

	out := make([]Finding, 0, len(all))
	end := -1
	for _, r := range all {
		if r.f.Start < end {
			continue // overlaps something already kept
		}
		out = append(out, r.f)
		end = r.f.End
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -run TestResolve -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): detector contract and deterministic overlap resolution"
```

---

### Task 5: The rule table and the false-positive allowlist

**Files:**
- Create: `internal/privacy/rules/rules.go`, `internal/privacy/rules/allow.go`
- Test: `internal/privacy/rules/rules_test.go`

**Interfaces:**
- Consumes: `privacy.Finding`, `privacy.Label`, `privacy.Detector` (Tasks 2, 4).
- Produces:
  - `type Rule struct { Name string; Label privacy.Label; Re *regexp.Regexp; Group int; MinEntropy float64 }`
  - `func Builtin() []Rule`
  - `type Detector struct{ ... }`, `func New(rules []Rule, extraAllow []string) (*Detector, error)`
  - `func (d *Detector) Name() string`, `func (d *Detector) Scan(ctx context.Context, text string) ([]privacy.Finding, error)`
  - `func Allowed(s string) bool`

The allowlist ships with the rules rather than separately because a rule table without it has an unacceptable false-positive rate: redacting a commit SHA replaces it with a placeholder and derails the agent's reasoning about history.

`Group` selects which capture group is the sensitive span — 0 means the whole match. That is how `API_KEY = "…"` redacts the value and leaves the assignment readable.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/rules/rules_test.go`:

```go
package rules

import (
	"context"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/privacy"
)

func scan(t *testing.T, text string) []privacy.Finding {
	t.Helper()
	d, err := New(Builtin(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return got
}

// found reports whether any finding covers exactly want.
func found(text string, findings []privacy.Finding, want string) bool {
	for _, f := range findings {
		if text[f.Start:f.End] == want {
			return true
		}
	}
	return false
}

func TestRulesDetectCommonCredentials(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"aws access key", `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`, "AKIAIOSFODNN7EXAMPLE"},
		{"github pat", `token: ghp_1234567890abcdefghijklmnopqrstuvwx`, "ghp_1234567890abcdefghijklmnopqrstuvwx"},
		{"anthropic key", `sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789`, "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
		{"google api key", `key=AIzaSyA1234567890abcdefghijklmnopqrstuv`, "AIzaSyA1234567890abcdefghijklmnopqrstuv"},
		{"slack token", `xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx`, "xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx"},
		{"stripe key", `sk_live_1234567890abcdefghijklmn`, "sk_live_1234567890abcdefghijklmn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scan(t, c.text)
			if !found(c.text, got, c.want) {
				t.Errorf("did not find %q in %q; findings = %+v", c.want, c.text, got)
			}
		})
	}
}

// The whole PEM block is one finding: redacting only the header would leave the
// key material behind, which is the opposite of the point.
func TestRulesRedactAWholePrivateKeyBlock(t *testing.T) {
	text := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nqqqq\n-----END RSA PRIVATE KEY-----\nafter"
	got := scan(t, text)
	if len(got) == 0 {
		t.Fatal("no finding for a PEM private key block")
	}
	span := text[got[0].Start:got[0].End]
	if !strings.HasPrefix(span, "-----BEGIN") || !strings.HasSuffix(span, "-----") {
		t.Errorf("span is not the whole block: %q", span)
	}
	if strings.Contains(span, "before") || strings.Contains(span, "after") {
		t.Errorf("span swallowed surrounding text: %q", span)
	}
}

// Only the value is redacted, so the agent can still see WHICH setting it is.
func TestRulesRedactOnlyTheValueOfAnAssignment(t *testing.T) {
	text := `DATABASE_PASSWORD=s3cr3t-hunter2-correct-horse`
	got := scan(t, text)
	if len(got) == 0 {
		t.Fatal("no finding")
	}
	span := text[got[0].Start:got[0].End]
	if strings.Contains(span, "DATABASE_PASSWORD") {
		t.Errorf("the key name was redacted too: %q", span)
	}
	if !strings.Contains(span, "s3cr3t") {
		t.Errorf("the value was not redacted: %q", span)
	}
}

func TestRulesDetectCredentialsInAURL(t *testing.T) {
	text := `postgres://admin:sup3rs3cret@db.internal:5432/app`
	got := scan(t, text)
	if len(got) == 0 {
		t.Fatal("no finding for a connection string with a password")
	}
}

// False positives are the failure mode that makes the filter unusable: a
// redacted commit SHA breaks the agent's reasoning about history for no gain.
func TestRulesDoNotFireOnBenignHighEntropyStrings(t *testing.T) {
	for _, text := range []string{
		"commit 9edb09c1f4a3b2c5d6e7f8091a2b3c4d5e6f7a8b",
		"digest sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"id 550e8400-e29b-41d4-a716-446655440000",
		"version 1.26.5-rc1+build.42",
		"see https://docs.example.com/api/v1/reference",
		`API_KEY = "YOUR_API_KEY"`,
		`password = "changeme"`,
		`token: "<redacted>"`,
	} {
		if got := scan(t, text); len(got) != 0 {
			t.Errorf("false positive on %q: %+v", text, got)
		}
	}
}

func TestAllowedRecognisesBenignShapes(t *testing.T) {
	for _, s := range []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"9edb09c1f4a3b2c5d6e7f8091a2b3c4d5e6f7a8b",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"YOUR_API_KEY", "changeme", "xxxxxxxxxxxx", "<redacted>",
		"example.com", "1.26.5",
	} {
		if !Allowed(s) {
			t.Errorf("Allowed(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_1234567890abcdefghijklmnopqrstuvwx"} {
		if Allowed(s) {
			t.Errorf("Allowed(%q) = true; a real credential must not be allowlisted", s)
		}
	}
}

// An operator-supplied allow entry suppresses a rule that would otherwise fire.
func TestExtraAllowSuppressesAFinding(t *testing.T) {
	const text = `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`
	if got := scan(t, text); len(got) == 0 {
		t.Fatal("precondition: the rule should fire without an allow entry")
	}
	d, err := New(Builtin(), []string{"AKIAIOSFODNN7EXAMPLE"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("extra allow entry did not suppress the finding: %+v", got)
	}
}

func TestNewRejectsABadExtraAllowPattern(t *testing.T) {
	if _, err := New(Builtin(), []string{"/(unclosed/"}); err == nil {
		t.Fatal("New accepted an invalid regex; it must be reported at construction, not at scan time")
	}
}

func TestDetectorNameIsStable(t *testing.T) {
	d, _ := New(Builtin(), nil)
	if d.Name() != "rules" {
		t.Errorf("Name = %q, want rules — it appears in the Activity feed and in cache keys", d.Name())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy/rules`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write the allowlist**

Create `internal/privacy/rules/allow.go`:

```go
// Package rules is the deterministic tier of the privacy filter: a table of
// credential patterns, an entropy qualifier, and the allowlist that keeps their
// false-positive rate low enough to be usable.
//
// Deterministic detection beats a model for credentials in both directions.
// Precision, because a key format is a format — there is nothing to infer. And
// recall, because a model's single "secret" class has never seen the key format
// a vendor shipped last week, while a regex for it is one table row.
package rules

import (
	"fmt"
	"regexp"
	"strings"
)

// allowRes are shapes that look high-entropy but are not secrets. Every one of
// them is common in source code and terminal output, and redacting one costs
// the agent real context: a placeholder where a commit SHA belongs makes git
// history unreadable to it.
var allowRes = []*regexp.Regexp{
	// UUID
	regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
	// git SHA-1 and SHA-256 object ids, and bare content digests
	regexp.MustCompile(`(?i)^[0-9a-f]{40}$`),
	regexp.MustCompile(`(?i)^[0-9a-f]{64}$`),
	// semver, with optional prerelease and build metadata
	regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`),
	// RFC 2606 / RFC 6761 reserved names, and RFC 5737 documentation addresses
	regexp.MustCompile(`(?i)^([a-z0-9-]+\.)*(example\.(com|net|org)|example|test|invalid|localhost)$`),
	regexp.MustCompile(`^(192\.0\.2|198\.51\.100|203\.0\.113)\.\d{1,3}$`),
	// Obvious stand-ins. Anything that is all one repeated character is in the
	// same family and is covered by the runes check in Allowed.
	regexp.MustCompile(`(?i)^(your[_-]?|my[_-]?|the[_-]?)?(api[_-]?key|secret|token|password|passwd|pwd|credential)s?$`),
	regexp.MustCompile(`(?i)^(changeme|change[_-]?me|placeholder|redacted|removed|hidden|none|null|nil|todo|fixme|dummy|sample|test)$`),
	regexp.MustCompile(`(?i)^<[^>]*>$`), // <redacted>, <your key here>
	regexp.MustCompile(`(?i)^\$\{?[a-z0-9_]+\}?$`), // $VAR, ${VAR} — a reference, not a value
}

// Allowed reports whether s is a known non-secret and must never produce a
// finding.
func Allowed(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if isRepeatedRune(s) {
		return true // xxxxxxxx, ********, 00000000
	}
	for _, re := range allowRes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// isRepeatedRune reports whether s is one rune repeated, which is how every
// hand-written placeholder in every README looks.
func isRepeatedRune(s string) bool {
	rs := []rune(s)
	if len(rs) < 4 {
		return false
	}
	for _, r := range rs[1:] {
		if r != rs[0] {
			return false
		}
	}
	return true
}

// compileExtraAllow turns operator-supplied entries into matchers. A bare
// string is a literal; /.../ is a regex, matching the denylist's own syntax so
// an operator learns one convention rather than two.
func compileExtraAllow(entries []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, e := range entries {
		if len(e) >= 2 && strings.HasPrefix(e, "/") && strings.HasSuffix(e, "/") {
			re, err := regexp.Compile(e[1 : len(e)-1])
			if err != nil {
				return nil, fmt.Errorf("rules: allow pattern %s: %w", e, err)
			}
			out = append(out, re)
			continue
		}
		out = append(out, regexp.MustCompile(`^`+regexp.QuoteMeta(e)+`$`))
	}
	return out, nil
}
```

- [ ] **Step 4: Write the rule table and detector**

Create `internal/privacy/rules/rules.go`:

```go
package rules

import (
	"context"
	"regexp"

	"github.com/nicko170/aiproxy/internal/privacy"
)

// Rule is one credential pattern.
//
// Group selects which capture group is the sensitive span: 0 is the whole
// match. That is how an assignment rule redacts the value and leaves the key
// name readable, so the agent can still tell a DATABASE_PASSWORD from an
// API_KEY.
//
// MinEntropy, when non-zero, requires the captured span to clear that many bits
// per character before a finding is produced. It is what keeps the generic
// assignment rule from firing on `password = "changeme"`.
type Rule struct {
	Name       string
	Label      privacy.Label
	Re         *regexp.Regexp
	Group      int
	MinEntropy float64
}

// Builtin is the shipped rule table. Adding a credential format is adding a
// row, which is the whole reason this is data rather than code.
//
// Patterns are anchored on the credential's own fixed prefix wherever the
// vendor provides one, because a prefix is worth more than any amount of
// entropy heuristics: it is precise by construction.
func Builtin() []Rule {
	return []Rule{
		{
			Name: "aws-access-key-id", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16})\b`), Group: 1,
		},
		{
			Name: "github-token", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b((?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{82})\b`), Group: 1,
		},
		{
			Name: "anthropic-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_-]{20,})`), Group: 1,
		},
		{
			Name: "openai-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(sk-(?:proj-)?[A-Za-z0-9_-]{20,})`), Group: 1,
		},
		{
			Name: "google-api-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{35})\b`), Group: 1,
		},
		{
			Name: "slack-token", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(xox[baprs]-[A-Za-z0-9-]{10,})`), Group: 1,
		},
		{
			Name: "stripe-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b((?:sk|rk)_live_[A-Za-z0-9]{16,})\b`), Group: 1,
		},
		{
			Name: "jwt", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`), Group: 1,
		},
		{
			// (?s) so the block spans newlines; the lazy body stops at the first
			// END line rather than swallowing everything to the last one.
			Name: "private-key-block", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`(?s)(-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----)`), Group: 1,
		},
		{
			// Credentials in a URL: the password only, so the scheme, user, and
			// host stay legible.
			Name: "url-credentials", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]+:([^\s/:@]+)@`), Group: 1,
		},
		{
			// The generic catch-all, and the only rule that leans on entropy.
			// Without the entropy floor it fires on every `password = "changeme"`
			// in every example config in every repository.
			Name: "assigned-credential", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password|passwd|pwd|credential)s?\b\s*[:=]\s*["']?([^\s"',;]{12,})`),
			Group: 1, MinEntropy: 3.0,
		},
	}
}

// Detector scans with a rule table, suppressing anything the allowlist claims.
type Detector struct {
	rules []Rule
	extra []*regexp.Regexp
}

// New builds a Detector. extraAllow entries are operator-supplied allowlist
// additions — a literal, or /regex/ — and an invalid one is reported here
// rather than being discovered mid-request.
func New(rules []Rule, extraAllow []string) (*Detector, error) {
	extra, err := compileExtraAllow(extraAllow)
	if err != nil {
		return nil, err
	}
	return &Detector{rules: rules, extra: extra}, nil
}

func (d *Detector) Name() string { return "rules" }

// Scan applies every rule and returns unresolved findings; the pipeline's
// Resolve handles overlaps between them.
//
// Safe for concurrent use: regexp.Regexp is, and nothing here is mutated after
// New.
func (d *Detector) Scan(_ context.Context, text string) ([]privacy.Finding, error) {
	var out []privacy.Finding
	for _, r := range d.rules {
		for _, m := range r.Re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[0], m[1]
			if r.Group > 0 && len(m) > 2*r.Group+1 {
				start, end = m[2*r.Group], m[2*r.Group+1]
			}
			if start < 0 || end <= start {
				continue // the group did not participate in the match
			}
			span := text[start:end]
			if d.allowed(span) {
				continue
			}
			if r.MinEntropy > 0 && ShannonBits(span) < r.MinEntropy {
				continue
			}
			out = append(out, privacy.Finding{
				Start: start, End: end, Label: r.Label, Rule: r.Name, Confidence: 1.0,
			})
		}
	}
	return out, nil
}

func (d *Detector) allowed(span string) bool {
	if Allowed(span) {
		return true
	}
	for _, re := range d.extra {
		if re.MatchString(span) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/privacy/rules -race -v`
Expected: FAIL on `undefined: ShannonBits` — that arrives in Task 6. Comment out the `MinEntropy` check temporarily *only if* you want a green run here; otherwise proceed to Task 6 and run both together. Do **not** commit with the check removed.

- [ ] **Step 6: Commit after Task 6 passes**

This task and Task 6 share one commit, because the rule table's `assigned-credential` row is unusable without the entropy floor and committing it alone would ship a rule that fires on every example config.

---

### Task 6: The entropy qualifier

**Files:**
- Create: `internal/privacy/rules/entropy.go`
- Test: `internal/privacy/rules/entropy_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func ShannonBits(s string) float64`

Entropy is a **qualifier, not a detector**. It only ever narrows a contextual rule's capture group. A standalone high-entropy string in source code is usually a hash, an id, or a fixture, so promoting entropy to a detector of its own would produce exactly the false positives Task 5's allowlist exists to prevent.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/rules/entropy_test.go`:

```go
package rules

import "testing"

func TestShannonBitsOrdersInputsSensibly(t *testing.T) {
	// A repeated character carries no information.
	if got := ShannonBits("aaaaaaaaaaaa"); got != 0 {
		t.Errorf("ShannonBits(repeated) = %v, want 0", got)
	}
	// An English word is low; a random base64 run is high. The exact values
	// matter less than the ordering and the thresholds around them.
	word := ShannonBits("changeme")
	random := ShannonBits("Zm9vYmFyYmF6cXV1eDEyMzQ1Njc4OTA")
	if !(word < random) {
		t.Errorf("expected changeme (%v) below a random run (%v)", word, random)
	}
	if random < 4.0 {
		t.Errorf("a random base64 run scored %v; the assigned-credential rule's 3.0 floor would never fire", random)
	}
	if word >= 3.0 {
		t.Errorf("changeme scored %v, at or above the 3.0 floor — it would be redacted", word)
	}
}

func TestShannonBitsOnEmptyAndSingle(t *testing.T) {
	if got := ShannonBits(""); got != 0 {
		t.Errorf("ShannonBits(\"\") = %v, want 0", got)
	}
	if got := ShannonBits("a"); got != 0 {
		t.Errorf("ShannonBits(\"a\") = %v, want 0", got)
	}
}

// Entropy is computed over bytes, so multi-byte input must not panic or produce
// a nonsensical value.
func TestShannonBitsHandlesMultiByteInput(t *testing.T) {
	if got := ShannonBits("héllo wörld 😀"); got <= 0 {
		t.Errorf("ShannonBits(multibyte) = %v, want > 0", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy/rules -run TestShannonBits`
Expected: FAIL — `undefined: ShannonBits`.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/rules/entropy.go`:

```go
package rules

import "math"

// ShannonBits is the Shannon entropy of s in bits per byte, from 0 (one
// repeated byte) to 8 (uniform over all byte values). A random base64 run lands
// near 5-6; English prose near 3-4; a hand-written placeholder like "changeme"
// below 3.
//
// Computed over bytes rather than runes deliberately: the inputs this qualifies
// are credential-shaped, which is to say ASCII, and byte frequencies are what
// the thresholds in Builtin were chosen against.
func ShannonBits(s string) float64 {
	if len(s) < 2 {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	var bits float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		bits -= p * math.Log2(p)
	}
	return bits
}
```

- [ ] **Step 4: Run the whole rules package**

Run: `go test ./internal/privacy/rules -race -v`
Expected: PASS — every test from Tasks 5 and 6, including
`TestRulesDoNotFireOnBenignHighEntropyStrings`. If `password = "changeme"`
still produces a finding, the entropy floor is not being applied; check that
`Scan` consults `r.MinEntropy` against the *captured group*, not the whole
match.

- [ ] **Step 5: Commit**

```bash
git add internal/privacy/rules
git commit -m "feat(privacy): deterministic credential rules with an entropy floor"
```

---

### Task 7: The operator denylist, and who applies the minimum length

**Files:**
- Create: `internal/privacy/rules/denylist.go`
- Modify: `internal/privacy/rules/rules.go` (add the minimum-length guard to `Detector.Scan`)
- Create: `internal/privacy/minlen.go`
- Test: `internal/privacy/rules/denylist_test.go`

**Interfaces:**
- Consumes: `privacy.Finding`, `privacy.LabelID` (Tasks 2, 4).
- Produces:
  - `privacy.MinScanBytes = 8`
  - `type Denylist struct{ ... }`, `func NewDenylist(entries []string) (*Denylist, error)`
  - `func (d *Denylist) Name() string` → `"denylist"`, `func (d *Denylist) Scan(ctx, text) ([]privacy.Finding, error)`

**The minimum length belongs in the detectors, not the pipeline.** The rule and NER detectors skip strings under `MinScanBytes` because no credential format fits and most strings in a request are short protocol values. The denylist is exempt — an internal codename or short hostname is easily under 8 bytes, and silently not matching a literal the operator explicitly asked for is the worst failure available here. Putting the check inside each detector keeps the pipeline free of per-detector special cases.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/rules/denylist_test.go`:

```go
package rules

import (
	"context"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/privacy"
)

func denyScan(t *testing.T, entries []string, text string) []privacy.Finding {
	t.Helper()
	d, err := NewDenylist(entries)
	if err != nil {
		t.Fatalf("NewDenylist: %v", err)
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return got
}

func TestDenylistMatchesLiterals(t *testing.T) {
	text := "deploying to acme-prod.internal from ci"
	got := denyScan(t, []string{"acme-prod.internal"}, text)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if span := text[got[0].Start:got[0].End]; span != "acme-prod.internal" {
		t.Errorf("span = %q", span)
	}
	if got[0].Label != privacy.LabelID {
		t.Errorf("Label = %q, want ID", got[0].Label)
	}
}

func TestDenylistMatchesEveryOccurrence(t *testing.T) {
	text := "acme-prod talks to acme-prod over tls"
	if got := denyScan(t, []string{"acme-prod"}, text); len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
}

func TestDenylistSupportsRegexEntries(t *testing.T) {
	text := "buckets: acme-data-eu, acme-data-us, other-data"
	got := denyScan(t, []string{`/acme-data-[a-z]{2}/`}, text)
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if !strings.HasPrefix(text[f.Start:f.End], "acme-data-") {
			t.Errorf("span = %q", text[f.Start:f.End])
		}
	}
}

// The denylist is deliberately exempt from MinScanBytes: an operator asking for
// "acme" to be redacted must get it, and four bytes is a plausible codename.
func TestDenylistMatchesShortEntries(t *testing.T) {
	text := "project acme ships friday"
	if got := denyScan(t, []string{"acme"}, text); len(got) != 1 {
		t.Fatalf("a 4-byte denylist entry did not match: %+v", got)
	}
}

// ...whereas the rule detector skips short strings entirely, because no
// credential format fits in one and most strings in a request are protocol
// values.
func TestRulesSkipStringsBelowTheMinimum(t *testing.T) {
	d, err := New(Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Shorter than privacy.MinScanBytes, and contrived to match nothing anyway;
	// the assertion is that Scan returns early rather than that it finds nothing.
	got, err := d.Scan(context.Background(), "sk-ant-")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("findings on a sub-minimum string: %+v", got)
	}
}

func TestNewDenylistRejectsABadPattern(t *testing.T) {
	if _, err := NewDenylist([]string{"/(unclosed/"}); err == nil {
		t.Fatal("NewDenylist accepted an invalid regex; it must fail at construction")
	}
}

func TestEmptyDenylistFindsNothing(t *testing.T) {
	if got := denyScan(t, nil, "anything at all"); len(got) != 0 {
		t.Errorf("empty denylist produced findings: %+v", got)
	}
}

func TestDenylistNameIsStable(t *testing.T) {
	d, _ := NewDenylist(nil)
	if d.Name() != "denylist" {
		t.Errorf("Name = %q, want denylist", d.Name())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy/rules -run 'TestDenylist|TestRulesSkip|TestNewDenylist|TestEmptyDenylist'`
Expected: FAIL — `undefined: NewDenylist`, `undefined: privacy.MinScanBytes`.

- [ ] **Step 3: Declare the minimum**

Create `internal/privacy/minlen.go`:

```go
package privacy

// MinScanBytes is the shortest string the rule and model detectors will look at.
//
// No credential format fits in fewer bytes, and the overwhelming majority of
// strings in a provider request are short protocol values — "user", "text",
// "tool_use" — so skipping them is most of the scan budget saved for free.
//
// The check lives inside each detector rather than in the pipeline, because the
// operator denylist must NOT honour it: a four-byte project codename is a
// perfectly reasonable thing to ask to have redacted, and silently ignoring an
// explicit instruction is the worst failure this component could have. Keeping
// the rule per-detector means the pipeline needs no exceptions.
const MinScanBytes = 8
```

- [ ] **Step 4: Add the guard to the rule detector**

In `internal/privacy/rules/rules.go`, at the top of `Detector.Scan`:

```go
func (d *Detector) Scan(_ context.Context, text string) ([]privacy.Finding, error) {
	if len(text) < privacy.MinScanBytes {
		return nil, nil
	}
	var out []privacy.Finding
	// ... unchanged
```

- [ ] **Step 5: Write the denylist detector**

Create `internal/privacy/rules/denylist.go`:

```go
package rules

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nicko170/aiproxy/internal/privacy"
)

// Denylist matches operator-supplied literals and regexes, labelled ID.
//
// This is what covers internal identifiers — hostnames, bucket names, project
// codenames — which no general model can know about and no shipped rule can
// guess. It is deliberately exempt from privacy.MinScanBytes: if an operator
// asks for a four-character codename to be redacted, redacting it is the whole
// job.
type Denylist struct {
	res []*regexp.Regexp
}

// NewDenylist compiles entries. A bare string is a literal; /.../ is a regex,
// matching the allowlist's syntax so there is one convention to learn. An
// invalid pattern is an error here rather than a surprise mid-request.
func NewDenylist(entries []string) (*Denylist, error) {
	d := &Denylist{}
	for _, e := range entries {
		if e == "" {
			continue
		}
		if len(e) >= 2 && strings.HasPrefix(e, "/") && strings.HasSuffix(e, "/") {
			re, err := regexp.Compile(e[1 : len(e)-1])
			if err != nil {
				return nil, fmt.Errorf("privacy: denylist pattern %s: %w", e, err)
			}
			d.res = append(d.res, re)
			continue
		}
		// Literals are matched case-insensitively: an operator writing a
		// hostname in lower case means it wherever it appears, and mixed casing
		// in logs is routine.
		d.res = append(d.res, regexp.MustCompile(`(?i)`+regexp.QuoteMeta(e)))
	}
	return d, nil
}

func (d *Denylist) Name() string { return "denylist" }

// Scan reports every occurrence of every entry. Overlaps between entries, and
// with rule findings, are settled by privacy.Resolve.
func (d *Denylist) Scan(_ context.Context, text string) ([]privacy.Finding, error) {
	if len(d.res) == 0 {
		return nil, nil
	}
	var out []privacy.Finding
	for _, re := range d.res {
		for _, m := range re.FindAllStringIndex(text, -1) {
			out = append(out, privacy.Finding{
				Start: m[0], End: m[1], Label: privacy.LabelID,
				Rule: "denylist", Confidence: 1.0,
			})
		}
	}
	return out, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/privacy ./internal/privacy/rules -race -v`
Expected: PASS — everything from Tasks 1 through 7.

- [ ] **Step 7: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): operator denylist for internal identifiers"
```

---

### Task 8: The redaction pipeline

**Files:**
- Create: `internal/privacy/redact.go`
- Test: `internal/privacy/redact_test.go`

**Interfaces:**
- Consumes: `WalkStrings`, `StringSpan` (Task 1); `Table` (Task 3); `Detector`, `Finding`, `Resolve` (Task 4); `MinScanBytes` (Task 7).
- Produces:
  - `type findingsCache interface { Get(text string) ([]Finding, bool); Put(text string, findings []Finding) }` — Task 12 implements it; pass `nil` until then.
  - `type Redactor struct{ ... }`, `func NewRedactor(detectors []Detector, cache findingsCache) *Redactor`
  - `func (r *Redactor) Redact(ctx context.Context, doc []byte, table *Table) ([]byte, error)`
  - `func SkipKey(key, parentKey string) bool`

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/redact_test.go`:

```go
package privacy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeDetector reports a finding wherever needle appears, and counts calls so a
// test can assert what was and was not scanned.
type fakeDetector struct {
	name   string
	needle string
	label  Label
	calls  int
	texts  []string
	err    error
}

func (f *fakeDetector) Name() string { return f.name }

func (f *fakeDetector) Scan(_ context.Context, text string) ([]Finding, error) {
	f.calls++
	f.texts = append(f.texts, text)
	if f.err != nil {
		return nil, f.err
	}
	var out []Finding
	for i := 0; ; {
		j := strings.Index(text[i:], f.needle)
		if j < 0 {
			break
		}
		start := i + j
		out = append(out, Finding{
			Start: start, End: start + len(f.needle),
			Label: f.label, Rule: f.name, Confidence: 1,
		})
		i = start + len(f.needle)
	}
	return out, nil
}

func redactWith(t *testing.T, doc string, dets ...Detector) ([]byte, *Table) {
	t.Helper()
	tab := NewTable(testKey)
	out, err := NewRedactor(dets, nil).Redact(context.Background(), []byte(doc), tab)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	return out, tab
}

func TestRedactReplacesAFindingAndLeavesEverythingElseAlone(t *testing.T) {
	doc := `{"model":"claude-opus-5","messages":[{"role":"user","content":"key is SEKRIT here"}]}`
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	out, tab := redactWith(t, doc, det)

	if strings.Contains(string(out), "SEKRIT") {
		t.Fatalf("the secret survived: %s", out)
	}
	if !strings.Contains(string(out), Sentinel) {
		t.Fatalf("no placeholder in the output: %s", out)
	}
	if tab.Len() != 1 {
		t.Errorf("table has %d entries, want 1", tab.Len())
	}
	// Everything structural is byte-identical.
	for _, frag := range []string{`"model":"claude-opus-5"`, `"role":"user"`, `"messages":[`} {
		if !strings.Contains(string(out), frag) {
			t.Errorf("output lost %s: %s", frag, out)
		}
	}
	// And it is still valid JSON with the same shape.
	var got, want map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(doc), &want); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("top-level key count changed: %d vs %d", len(got), len(want))
	}
}

// Two findings in one string, and several across strings, exercise the
// last-span-first splice. Applied in the other order, the second replacement
// would land at a stale offset.
func TestRedactHandlesMultipleFindingsPerStringAndPerDocument(t *testing.T) {
	doc := `{"a":"SEKRIT then SEKRIT again","b":"and SEKRIT here too"}`
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	out, tab := redactWith(t, doc, det)

	if strings.Contains(string(out), "SEKRIT") {
		t.Fatalf("a secret survived: %s", out)
	}
	if n := strings.Count(string(out), Sentinel); n != 3 {
		t.Errorf("got %d placeholders, want 3: %s", n, out)
	}
	// One distinct value, so one table entry, however many times it occurs.
	if tab.Len() != 1 {
		t.Errorf("table has %d entries, want 1 — the same value must reuse its placeholder", tab.Len())
	}
	var back map[string]string
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
}

// The model name must never be redacted: routing and selection read it, and a
// placeholder there is a request the provider cannot serve.
func TestRedactNeverTouchesStructuralKeys(t *testing.T) {
	doc := `{"model":"SEKRIT-model","type":"SEKRIT","role":"SEKRIT","anthropic_version":"SEKRIT"}`
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	out, _ := redactWith(t, doc, det)
	if string(out) != doc {
		t.Errorf("a structural key was rewritten:\n got %s\nwant %s", out, doc)
	}
	if det.calls != 0 {
		t.Errorf("structural values were handed to a detector %d times; they must not be scanned at all", det.calls)
	}
}

// Base64 image payloads are megabytes of maximum-entropy text. Scanning them
// would dominate the budget and trip every entropy heuristic.
func TestRedactSkipsBase64ImageData(t *testing.T) {
	doc := `{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"SEKRITSEKRITSEKRIT"}}]}`
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	out, _ := redactWith(t, doc, det)
	if string(out) != doc {
		t.Errorf("image data was rewritten: %s", out)
	}
	for _, seen := range det.texts {
		if strings.Contains(seen, "SEKRITSEKRIT") {
			t.Error("image data was handed to a detector")
		}
	}
}

func TestRedactEscapesReplacementsCorrectly(t *testing.T) {
	// The value carries characters that must stay escaped in the output.
	doc := `{"a":"pre \"SEKRIT\" post"}`
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	out, _ := redactWith(t, doc, det)
	var back string
	if err := json.Unmarshal(out[len(`{"a":`):len(out)-1], &back); err != nil {
		t.Fatalf("replacement produced an invalid literal: %v\n%s", err, out)
	}
	if !strings.Contains(back, "pre \"") || !strings.Contains(back, "\" post") {
		t.Errorf("surrounding escapes were lost: %q", back)
	}
}

func TestRedactSurfacesADetectorError(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "x", err: context.DeadlineExceeded}
	_, err := NewRedactor([]Detector{det}, nil).Redact(
		context.Background(), []byte(`{"a":"a long enough value"}`), NewTable(testKey))
	if err == nil {
		t.Fatal("Redact hid a detector error; fail-closed depends on seeing it")
	}
}

func TestRedactOnAMalformedBodyIsAnError(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "x"}
	if _, err := NewRedactor([]Detector{det}, nil).Redact(
		context.Background(), []byte(`{"a":`), NewTable(testKey)); err == nil {
		t.Fatal("Redact accepted malformed JSON")
	}
}

// With no findings the body must come back byte-identical — not merely
// equivalent. This is what makes the filter free when nothing matches.
func TestRedactWithNoFindingsReturnsTheOriginalBytes(t *testing.T) {
	doc := `{"a":"nothing sensitive here at all","b":[1,2,3]}`
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	out, tab := redactWith(t, doc, det)
	if string(out) != doc {
		t.Errorf("body changed with no findings:\n got %s\nwant %s", out, doc)
	}
	if tab.Len() != 0 {
		t.Errorf("table has %d entries, want 0", tab.Len())
	}
}

func TestSkipKey(t *testing.T) {
	for _, c := range []struct {
		key, parent string
		want        bool
	}{
		{"model", "", true},
		{"type", "", true},
		{"role", "", true},
		{"id", "", true},
		{"name", "", true},
		{"stop_reason", "", true},
		{"anthropic_version", "", true},
		{"cache_control", "", true},
		{"data", "source", true},
		{"data", "", false},     // only under a source block
		{"content", "", false},
		{"text", "", false},
	} {
		if got := SkipKey(c.key, c.parent); got != c.want {
			t.Errorf("SkipKey(%q, %q) = %v, want %v", c.key, c.parent, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run 'TestRedact|TestSkipKey'`
Expected: FAIL — `undefined: NewRedactor`, `undefined: SkipKey`.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/redact.go`:

```go
package privacy

import (
	"context"
	"encoding/json"
	"fmt"
)

// findingsCache lets the pipeline skip detection for a string it has already
// scanned. It is an interface so the pipeline can be built and tested before the
// cache exists, and so a nil cache is a legitimate configuration rather than a
// special case — see NewRedactor.
//
// Implementations hold findings only: byte offsets and labels, never the text
// and never the values found in it. That is what keeps sensitive plaintext out
// of any structure living longer than one request.
type findingsCache interface {
	Get(text string) ([]Finding, bool)
	Put(text string, findings []Finding)
}

// skipKeys are JSON keys whose values are protocol, not content. Redacting one
// produces a request the provider rejects — or worse, serves differently.
//
// This is a denylist rather than an allowlist of scannable paths on purpose: an
// allowlist silently stops covering a field the day the provider adds one, while
// a denylist fails in the safe direction by scanning more than strictly needed.
var skipKeys = map[string]bool{
	"model":             true, // routing and selection read it
	"type":              true,
	"role":              true,
	"id":                true,
	"name":              true, // tool names, not prose
	"stop_reason":       true,
	"stop_sequence":     true,
	"anthropic_version": true,
	"anthropic_beta":    true, // rewriting this costs the 1M-token context
	"cache_control":     true,
	"media_type":        true,
}

// SkipKey reports whether a value under key (nested inside parentKey) must not
// be scanned or rewritten.
func SkipKey(key, parentKey string) bool {
	if skipKeys[key] {
		return true
	}
	// Base64 payloads: megabytes of maximum-entropy text that would dominate
	// scan time and trip every entropy heuristic for nothing. Only skipped under
	// a source block, so an ordinary "data" field of prose is still scanned.
	if key == "data" && parentKey == "source" {
		return true
	}
	return false
}

// Redactor runs detectors over a request body and splices placeholders in.
//
// Safe for concurrent use: one Redactor is shared by every in-flight request,
// and per-request state lives entirely in the Table the caller passes.
type Redactor struct {
	detectors []Detector
	cache     findingsCache
}

// NewRedactor builds a Redactor. Detector order is significant — it is the
// tiebreak Resolve uses for identical spans (see Resolve) — so callers register
// deterministic rules before the model. cache may be nil, which disables
// caching without changing any other behaviour.
func NewRedactor(detectors []Detector, cache findingsCache) *Redactor {
	return &Redactor{detectors: detectors, cache: cache}
}

// Redact returns doc with every detected span replaced by a placeholder, and
// records each replacement in table.
//
// Returned bytes are the ORIGINAL bytes with only the replaced literals
// substituted: no re-serialization, so key order and whitespace are the
// client's. With no findings, the input slice's contents are returned unchanged.
//
// A detector error is returned rather than swallowed. Fail-closed depends on the
// caller being able to tell "scanned, found nothing" from "could not scan".
func (r *Redactor) Redact(ctx context.Context, doc []byte, table *Table) ([]byte, error) {
	spans, err := WalkStrings(doc)
	if err != nil {
		return nil, err
	}

	// replacement is one literal to splice, in document order.
	type replacement struct {
		start, end int
		literal    []byte
	}
	var reps []replacement

	for _, span := range spans {
		if SkipKey(span.Key, span.ParentKey) {
			continue
		}
		findings, err := r.scan(ctx, span.Value)
		if err != nil {
			return nil, err
		}
		if len(findings) == 0 {
			continue
		}
		newValue, err := applyFindings(span.Value, findings, table)
		if err != nil {
			return nil, err
		}
		if newValue == span.Value {
			continue
		}
		lit, err := json.Marshal(newValue)
		if err != nil {
			return nil, fmt.Errorf("privacy: encode redacted value: %w", err)
		}
		reps = append(reps, replacement{start: span.Start, end: span.End, literal: lit})
	}
	if len(reps) == 0 {
		return doc, nil
	}

	// Splice LAST SPAN FIRST. Rewriting a span changes the length of everything
	// after it, so applying in document order would invalidate every subsequent
	// offset. WalkStrings returns spans in document order, so walking reps
	// backwards is the whole of the fix.
	out := make([]byte, len(doc))
	copy(out, doc)
	for i := len(reps) - 1; i >= 0; i-- {
		rep := reps[i]
		tail := append([]byte{}, out[rep.end:]...)
		out = append(out[:rep.start], rep.literal...)
		out = append(out, tail...)
	}
	return out, nil
}

// scan consults the cache, falling back to the detectors in registration order.
func (r *Redactor) scan(ctx context.Context, text string) ([]Finding, error) {
	if r.cache != nil {
		if findings, ok := r.cache.Get(text); ok {
			return findings, nil
		}
	}
	perDetector := make([][]Finding, 0, len(r.detectors))
	for _, d := range r.detectors {
		found, err := d.Scan(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("privacy: detector %s: %w", d.Name(), err)
		}
		perDetector = append(perDetector, found)
	}
	resolved := Resolve(perDetector)
	if r.cache != nil {
		r.cache.Put(text, resolved)
	}
	return resolved, nil
}

// applyFindings rewrites one decoded value, replacing each finding's span with
// a placeholder. findings must be resolved (ordered, non-overlapping); it walks
// them backwards for the same reason Redact does.
func applyFindings(value string, findings []Finding, table *Table) (string, error) {
	out := value
	for i := len(findings) - 1; i >= 0; i-- {
		f := findings[i]
		if f.Start < 0 || f.End > len(out) || f.End <= f.Start {
			// A detector reporting offsets outside the string it was given is a
			// bug, and splicing on them would corrupt the body. Refuse instead.
			return "", fmt.Errorf("privacy: detector %s reported span [%d,%d) for a %d-byte value",
				f.Rule, f.Start, f.End, len(out))
		}
		placeholder, err := table.Add(f.Label, out[f.Start:f.End])
		if err != nil {
			return "", err
		}
		out = out[:f.Start] + placeholder + out[f.End:]
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v -run 'TestRedact|TestSkipKey'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): redaction pipeline with last-span-first splicing"
```

---

### Task 9: The holdback rewriter — where property 2 is proven

**Files:**
- Create: `internal/privacy/rewriter.go`
- Test: `internal/privacy/rewriter_test.go`

**Interfaces:**
- Consumes: `Table` (Task 3); `Sentinel`, `MaxPlaceholderBytes`, `FindPlaceholder` (Task 2).
- Produces:
  - `type UnresolvedMode int` with `Passthrough` and `ErrorOut`
  - `var ErrUnresolved = errors.New(...)`
  - `type rewriter struct{ ... }` (unexported; the SSE layer in Task 10 owns them)
  - `func newRewriter(table *Table, mode UnresolvedMode, onUnresolved func(string)) *rewriter`
  - `func (w *rewriter) Write(s string) (string, error)` — returns text safe to emit now
  - `func (w *rewriter) Flush() (string, error)` — returns whatever was held
  - `func (w *rewriter) Pending() int` — held bytes, so a test can assert nothing is withheld

This is the single most important unit in the feature. Everything above it is
ordinary code; this is the part that has to be right for *any* division of the
byte stream, and the test that proves it is exhaustive over split points rather
than illustrative.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/rewriter_test.go`:

```go
package privacy

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// feed pushes s through a fresh rewriter in the given chunk sizes and returns
// the complete output. Any split must produce the same bytes — that is the
// property this whole file exists to establish.
func feed(t *testing.T, tab *Table, s string, chunks []int) string {
	t.Helper()
	w := newRewriter(tab, Passthrough, nil)
	var b strings.Builder
	i := 0
	for _, n := range chunks {
		if i >= len(s) {
			break
		}
		j := i + n
		if j > len(s) {
			j = len(s)
		}
		out, err := w.Write(s[i:j])
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		b.WriteString(out)
		i = j
	}
	if i < len(s) {
		out, err := w.Write(s[i:])
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		b.WriteString(out)
	}
	tail, err := w.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	b.WriteString(tail)
	return b.String()
}

func tableWith(t *testing.T, label Label, value string) (*Table, string) {
	t.Helper()
	tab := NewTable(testKey)
	p, err := tab.Add(label, value)
	if err != nil {
		t.Fatal(err)
	}
	return tab, p
}

func TestRewriterSubstitutesInOneWrite(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	in := "your key " + p + " is stale"
	if got := feed(t, tab, in, []int{len(in)}); got != "your key AKIAIOSFODNN7EXAMPLE is stale" {
		t.Errorf("got %q", got)
	}
}

// Property 2, exhaustively: split the stream at EVERY byte offset and assert the
// output never changes. A placeholder straddling the split is the normal case,
// not the exotic one — token boundaries do not respect placeholders.
func TestRewriterIsInvariantAcrossEverySingleSplit(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	in := "prefix " + p + " middle " + p + " suffix"
	want := strings.ReplaceAll(in, p, "AKIAIOSFODNN7EXAMPLE")

	for split := 0; split <= len(in); split++ {
		got := feed(t, tab, in, []int{split})
		if got != want {
			t.Fatalf("split at %d produced %q, want %q", split, got, want)
		}
	}
}

// One byte at a time is the worst case for the holdback and the cheapest way to
// catch an off-by-one in the pending buffer.
func TestRewriterIsInvariantByteByByte(t *testing.T) {
	tab, p := tableWith(t, LabelPerson, "Ada Lovelace")
	in := "hello " + p + ", welcome"
	want := "hello Ada Lovelace, welcome"

	ones := make([]int, len(in))
	for i := range ones {
		ones[i] = 1
	}
	if got := feed(t, tab, in, ones); got != want {
		t.Errorf("byte-by-byte gave %q, want %q", got, want)
	}
}

func TestRewriterIsInvariantUnderRandomSplits(t *testing.T) {
	tab, p := tableWith(t, LabelEmail, "ada@example.com")
	in := "a " + p + " b " + p + " c " + p + " d"
	want := strings.ReplaceAll(in, p, "ada@example.com")

	rng := rand.New(rand.NewSource(1)) // fixed seed: a failure must reproduce
	for iter := 0; iter < 500; iter++ {
		var chunks []int
		for total := 0; total < len(in); {
			n := 1 + rng.Intn(7)
			chunks = append(chunks, n)
			total += n
		}
		if got := feed(t, tab, in, chunks); got != want {
			t.Fatalf("chunks %v produced %q, want %q", chunks, got, want)
		}
	}
}

// Property 3: text with no sentinel comes out byte-for-byte, and nothing is
// withheld unless a chunk actually ends mid-sentinel.
func TestRewriterWithholdsNothingWithoutASentinel(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	const in = "ordinary prose and some code: arr[0] = f(x); // fine"
	out, err := w.Write(in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("output %q != input %q", out, in)
	}
	if n := w.Pending(); n != 0 {
		t.Errorf("withheld %d bytes with no sentinel present; per-chunk flushing must be untouched", n)
	}
}

// A chunk ending in "[" holds back exactly one byte — the minimum needed to
// stay correct, and the only case where ordinary text is delayed at all.
func TestRewriterHoldsBackOnlyAPartialSentinel(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	out, err := w.Write("index[")
	if err != nil {
		t.Fatal(err)
	}
	if out != "index" {
		t.Errorf("emitted %q, want index", out)
	}
	if w.Pending() != 1 {
		t.Errorf("withheld %d bytes, want 1", w.Pending())
	}
	// The next write resolves it: "[0]" cannot begin a placeholder.
	out, err = w.Write("0] = 1")
	if err != nil {
		t.Fatal(err)
	}
	if out != "[0] = 1" {
		t.Errorf("emitted %q, want [0] = 1", out)
	}
	if w.Pending() != 0 {
		t.Errorf("still withholding %d bytes", w.Pending())
	}
}

// Holdback is bounded, so a stream of open brackets cannot grow the buffer.
func TestRewriterHoldbackIsBounded(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	for i := 0; i < 1000; i++ {
		if _, err := w.Write("[[AIPROXY_"); err != nil {
			t.Fatal(err)
		}
		if n := w.Pending(); n >= MaxPlaceholderBytes {
			t.Fatalf("pending grew to %d bytes, budget is %d", n, MaxPlaceholderBytes)
		}
	}
}

// An unresolvable placeholder passes through verbatim and is reported. Guessing
// a value would write something wrong into the operator's files; a visibly
// wrong string is recoverable.
func TestRewriterPassesThroughAnUnresolvedPlaceholder(t *testing.T) {
	tab := NewTable(testKey)
	var seen []string
	w := newRewriter(tab, Passthrough, func(p string) { seen = append(seen, p) })

	const orphan = "[[AIPROXY_SECRET_deadbeef]]"
	out, err := w.Write("before " + orphan + " after")
	if err != nil {
		t.Fatal(err)
	}
	tail, err := w.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if got := out + tail; got != "before "+orphan+" after" {
		t.Errorf("got %q, want the placeholder verbatim", got)
	}
	if len(seen) != 1 || seen[0] != orphan {
		t.Errorf("onUnresolved saw %v, want one report of %q", seen, orphan)
	}
}

func TestRewriterErrorsOnUnresolvedWhenConfigured(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, ErrorOut, nil)
	_, err := w.Write("before [[AIPROXY_SECRET_deadbeef]] after")
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
}

// A partial placeholder at end of stream is emitted, not swallowed. Losing it
// would silently truncate the model's answer.
func TestRewriterFlushEmitsAPartialTail(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	out, err := w.Write("ends with [[AIPROXY_SEC")
	if err != nil {
		t.Fatal(err)
	}
	tail, err := w.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if got := out + tail; got != "ends with [[AIPROXY_SEC" {
		t.Errorf("got %q; a partial placeholder must survive Flush", got)
	}
}

// Restoring a value that itself contains a sentinel-looking prefix must not
// re-enter the matcher: substituted text is emitted, never rescanned.
func TestRewriterDoesNotRescanSubstitutedText(t *testing.T) {
	tab := NewTable(testKey)
	// A pathological plaintext that looks like another placeholder.
	p, err := tab.Add(LabelSecret, "[[AIPROXY_SECRET_00000000]]")
	if err != nil {
		t.Fatal(err)
	}
	got := feed(t, tab, "x "+p+" y", []int{3, 5, 100})
	if got != "x [[AIPROXY_SECRET_00000000]] y" {
		t.Errorf("got %q; substituted text must be emitted as-is", got)
	}
}

// Multi-byte text must not be split mid-rune by the holdback logic. The
// rewriter works on bytes, so this asserts the bytes reassemble.
func TestRewriterPreservesMultiByteText(t *testing.T) {
	tab, p := tableWith(t, LabelPerson, "Ada")
	in := "héllo 😀 " + p + " wörld"
	want := "héllo 😀 Ada wörld"
	ones := make([]int, len(in))
	for i := range ones {
		ones[i] = 1
	}
	if got := feed(t, tab, in, ones); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run TestRewriter`
Expected: FAIL — `undefined: newRewriter`, `undefined: Passthrough`.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/rewriter.go`:

```go
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
}

func newRewriter(table *Table, mode UnresolvedMode, onUnresolved func(string)) *rewriter {
	return &rewriter{table: table, mode: mode, onUnresolved: onUnresolved}
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v -run TestRewriter`
Expected: PASS — all twelve. `TestRewriterIsInvariantAcrossEverySingleSplit` is the one that matters; if any split fails, the reported offset tells you exactly where the pending buffer is mishandled.

- [ ] **Step 5: Add a fuzz target for arbitrary content and splits**

Append to `internal/privacy/rewriter_test.go`:

```go
// FuzzRewriterRoundTrip is property 1 at the rewriter level: for arbitrary
// surrounding text and an arbitrary chunking, redact-then-restore is the
// identity. The fuzzer supplies both the text and the split sizes, so it
// explores boundary placements no hand-written case would think of.
func FuzzRewriterRoundTrip(f *testing.F) {
	f.Add("hello world", "secret-value", uint8(3))
	f.Add("", "x", uint8(1))
	f.Add("[[AIPROXY_", "v", uint8(2))
	f.Add("a[[b[[AIPROXY_SEC", "v", uint8(1))
	f.Fuzz(func(t *testing.T, surround, secret string, chunk uint8) {
		if secret == "" || chunk == 0 {
			return
		}
		tab := NewTable(testKey)
		p, err := tab.Add(LabelSecret, secret)
		if err != nil {
			return
		}
		in := surround + p + surround
		want := surround + secret + surround

		w := newRewriter(tab, Passthrough, nil)
		var b strings.Builder
		for i := 0; i < len(in); i += int(chunk) {
			j := i + int(chunk)
			if j > len(in) {
				j = len(in)
			}
			out, err := w.Write(in[i:j])
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			b.WriteString(out)
		}
		tail, err := w.Flush()
		if err != nil {
			t.Fatalf("Flush: %v", err)
		}
		b.WriteString(tail)

		// surround may itself contain a placeholder-looking run that resolves,
		// so compare against the same transformation applied whole.
		ref := newRewriter(tab, Passthrough, nil)
		whole, err := ref.Write(in)
		if err != nil {
			t.Fatal(err)
		}
		refTail, _ := ref.Flush()
		if got := b.String(); got != whole+refTail {
			t.Fatalf("chunked output differs from whole-input output:\n chunked %q\n whole   %q", got, whole+refTail)
		}
		if !strings.Contains(whole+refTail, secret) && strings.Contains(want, secret) {
			t.Fatalf("the secret was not restored: %q", whole+refTail)
		}
	})
}
```

Run: `go test ./internal/privacy -run FuzzRewriterRoundTrip -fuzz FuzzRewriterRoundTrip -fuzztime 60s`
Expected: no failures. Keep any corpus file the fuzzer writes.

- [ ] **Step 6: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): boundary-safe streaming rewriter with bounded holdback"
```

---

### Task 10: The SSE restorer

**Files:**
- Create: `internal/privacy/sse.go`, `internal/privacy/restore.go`
- Test: `internal/privacy/restore_test.go`

**Interfaces:**
- Consumes: `rewriter`, `UnresolvedMode`, `ErrUnresolved` (Task 9); `Table` (Task 3).
- Produces:
  - `func dataPayload(raw []byte) (line int, payload []byte, ok bool)`
  - `func replacePayload(raw []byte, line int, payload []byte) []byte`
  - `type Restorer struct{ ... }`, `func NewRestorer(table *Table, mode UnresolvedMode, onUnresolved func(string)) *Restorer`
  - `func (r *Restorer) Event(raw []byte) ([]byte, error)` — the returned bytes may contain **more than one** event
  - `func (r *Restorer) Body(body []byte) ([]byte, error)` — the non-streaming path

Two rules keep this honest. **An event that needs no change is returned as the
original bytes**, never re-marshalled — that is what makes property 3 hold
byte-for-byte rather than merely semantically. And **events are decoded into
`map[string]any`**, not a typed struct, so a field this code has never heard of
survives the round trip instead of being silently dropped.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/restore_test.go`:

```go
package privacy

import (
	"encoding/json"
	"strings"
	"testing"
)

func ev(payload string) []byte {
	var typ struct{ Type string }
	json.Unmarshal([]byte(payload), &typ)
	return []byte("event: " + typ.Type + "\ndata: " + payload + "\n\n")
}

func textDelta(index int, text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return ev(string(b))
}

// emitted concatenates the restorer's output for a sequence of events.
func emitted(t *testing.T, r *Restorer, events ...[]byte) string {
	t.Helper()
	var b strings.Builder
	for _, e := range events {
		out, err := r.Event(e)
		if err != nil {
			t.Fatalf("Event: %v", err)
		}
		b.Write(out)
	}
	return b.String()
}

// textOf pulls every text_delta's text out of a stream, in order — the string
// the agent actually assembles.
func textOf(t *testing.T, stream string) string {
	t.Helper()
	var b strings.Builder
	for _, chunk := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(chunk, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m struct {
				Type  string `json:"type"`
				Delta struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					Thinking string `json:"thinking"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("undecodable event %q: %v", line, err)
			}
			b.WriteString(m.Delta.Text)
			b.WriteString(m.Delta.Thinking)
		}
	}
	return b.String()
}

func TestRestorerSubstitutesAcrossTwoEvents(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	r := NewRestorer(tab, Passthrough, nil)

	// The placeholder is split mid-sentinel, which is the ordinary case.
	cut := len(Sentinel) - 3
	stream := emitted(t, r,
		textDelta(0, "your key "+p[:cut]),
		textDelta(0, p[cut:]+" is stale"),
		ev(`{"type":"content_block_stop","index":0}`),
	)
	if got := textOf(t, stream); got != "your key AKIAIOSFODNN7EXAMPLE is stale" {
		t.Errorf("assembled text = %q", got)
	}
}

// Property 3 at the SSE level: an event needing no change must come back as the
// exact same bytes, not a re-encoded equivalent.
func TestRestorerReturnsUnchangedEventsByteForByte(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	for _, raw := range [][]byte{
		textDelta(0, "just ordinary prose"),
		ev(`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12}}}`),
		ev(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`),
		ev(`{"type":"ping"}`),
	} {
		out, err := r.Event(raw)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(raw) {
			t.Errorf("event was rewritten when it needed no change:\n got %q\nwant %q", out, raw)
		}
	}
}

// Unknown fields must survive. A provider adding one must not have it dropped by
// a filter that only knows about the fields it cares about.
func TestRestorerPreservesUnknownFields(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "V")
	r := NewRestorer(tab, Passthrough, nil)
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta":     map[string]any{"type": "text_delta", "text": p},
		"brand_new": "keep me",
	})
	out, err := r.Event(ev(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "brand_new") || !strings.Contains(string(out), "keep me") {
		t.Errorf("unknown field was dropped: %s", out)
	}
}

// A tail withheld when the block ends is emitted as a synthetic delta before the
// stop event, so nothing is lost and the stop still arrives last.
func TestRestorerFlushesAPartialTailBeforeStop(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	stream := emitted(t, r,
		textDelta(0, "trailing bracket ["),
		ev(`{"type":"content_block_stop","index":0}`),
	)
	if got := textOf(t, stream); got != "trailing bracket [" {
		t.Errorf("assembled text = %q; the withheld byte must be flushed", got)
	}
	if !strings.Contains(stream, "content_block_stop") {
		t.Error("the stop event was lost")
	}
	if strings.Index(stream, "content_block_stop") < strings.LastIndex(stream, "text_delta") {
		t.Error("the synthetic flush must precede the stop event")
	}
}

// Two blocks interleave; each keeps its own pending buffer.
func TestRestorerKeepsBlocksIndependent(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "SEKRIT")
	r := NewRestorer(tab, Passthrough, nil)
	cut := len(Sentinel) - 2
	stream := emitted(t, r,
		textDelta(0, "a"+p[:cut]),
		textDelta(1, "b"+p[:cut]),
		textDelta(0, p[cut:]+"A"),
		textDelta(1, p[cut:]+"B"),
	)
	got := textOf(t, stream)
	if !strings.Contains(got, "aSEKRITA") || !strings.Contains(got, "bSEKRITB") {
		t.Errorf("blocks bled into each other: %q", got)
	}
}

func TestRestorerHandlesThinkingDeltas(t *testing.T) {
	tab, p := tableWith(t, LabelPerson, "Ada Lovelace")
	r := NewRestorer(tab, Passthrough, nil)
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "thinking_delta", "thinking": "about " + p},
	})
	stream := emitted(t, r, ev(string(b)))
	if got := textOf(t, stream); got != "about Ada Lovelace" {
		t.Errorf("thinking text = %q", got)
	}
}

// A complete text on content_block_start is restored whole.
func TestRestorerHandlesContentBlockStart(t *testing.T) {
	tab, p := tableWith(t, LabelEmail, "ada@example.com")
	r := NewRestorer(tab, Passthrough, nil)
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": "mail " + p},
	})
	out, err := r.Event(ev(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ada@example.com") {
		t.Errorf("content_block_start was not restored: %s", out)
	}
}

func TestRestorerBodyHandlesNonStreamingResponses(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	r := NewRestorer(tab, Passthrough, nil)
	body := []byte(`{"id":"msg_1","content":[{"type":"text","text":"key ` + p + ` here"}],"model":"claude-opus-5"}`)
	out, err := r.Body(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("body was not restored: %s", out)
	}
	if !strings.Contains(string(out), `"model":"claude-opus-5"`) {
		t.Errorf("body lost structure: %s", out)
	}
}

func TestRestorerBodyWithNothingToDoIsByteIdentical(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	body := []byte(`{"id":"msg_1","content":[{"type":"text","text":"nothing here"}]}`)
	out, err := r.Body(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("body changed with nothing to restore:\n got %s\nwant %s", out, body)
	}
}

func TestRestorerReportsUnresolvedPlaceholders(t *testing.T) {
	var seen []string
	r := NewRestorer(NewTable(testKey), Passthrough, func(p string) { seen = append(seen, p) })
	stream := emitted(t, r, textDelta(0, "orphan [[AIPROXY_SECRET_deadbeef]] here"))
	if got := textOf(t, stream); !strings.Contains(got, "[[AIPROXY_SECRET_deadbeef]]") {
		t.Errorf("the placeholder must pass through verbatim, got %q", got)
	}
	if len(seen) != 1 {
		t.Errorf("onUnresolved reports = %v, want 1", seen)
	}
}

func TestRestorerIgnoresMalformedEvents(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	// Not our business to police the provider's framing: an event we cannot
	// parse is relayed untouched rather than turned into an error that severs a
	// working stream.
	for _, raw := range [][]byte{
		[]byte("event: ping\n\n"),
		[]byte("data: not json\n\n"),
		[]byte(": comment\n\n"),
		[]byte("\n\n"),
	} {
		out, err := r.Event(raw)
		if err != nil {
			t.Errorf("Event(%q) errored: %v", raw, err)
		}
		if string(out) != string(raw) {
			t.Errorf("Event(%q) = %q, want it unchanged", raw, out)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run TestRestorer`
Expected: FAIL — `undefined: NewRestorer`.

- [ ] **Step 3: Write the SSE helpers**

Create `internal/privacy/sse.go`:

```go
package privacy

import "bytes"

// dataPayload finds the "data:" line in one raw SSE event and returns its index
// among the event's lines plus the payload bytes.
//
// Anthropic sends exactly one data line per event. Multi-line data — legal SSE,
// where the payload is the lines joined by "\n" — is deliberately NOT handled:
// ok is false and the caller relays the event untouched, which is the safe
// outcome for a shape this code has never seen.
func dataPayload(raw []byte) (int, []byte, bool) {
	lines := bytes.Split(raw, []byte("\n"))
	idx, count := -1, 0
	for i, l := range lines {
		if bytes.HasPrefix(l, []byte("data:")) {
			idx, count = i, count+1
		}
	}
	if count != 1 {
		return 0, nil, false
	}
	payload := bytes.TrimPrefix(lines[idx], []byte("data:"))
	return idx, bytes.TrimPrefix(payload, []byte(" ")), true
}

// replacePayload rebuilds raw with the data line's payload swapped out,
// preserving every other line and the event's trailing blank line exactly.
func replacePayload(raw []byte, line int, payload []byte) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	lines[line] = append([]byte("data: "), payload...)
	return bytes.Join(lines, []byte("\n"))
}
```

- [ ] **Step 4: Write the restorer**

Create `internal/privacy/restore.go`:

```go
package privacy

import (
	"encoding/json"
	"fmt"
)

// Restorer substitutes plaintext back into a response, event by event.
//
// One per response. Not safe for concurrent use, which matches how Relay reads a
// body: one goroutine, one event at a time.
type Restorer struct {
	table        *Table
	mode         UnresolvedMode
	onUnresolved func(string)
	// blocks holds one rewriter per content-block index, because each block is
	// its own independent text stream and a placeholder never spans two.
	blocks map[int]*blockState
}

// blockState is one content block's rewriter plus which delta field carries its
// text, so a synthetic flush event can be built with the right shape.
type blockState struct {
	rw        *rewriter
	deltaType string // text_delta | thinking_delta | input_json_delta
	field     string // text | thinking | partial_json
}

func NewRestorer(table *Table, mode UnresolvedMode, onUnresolved func(string)) *Restorer {
	return &Restorer{
		table: table, mode: mode, onUnresolved: onUnresolved,
		blocks: map[int]*blockState{},
	}
}

// deltaFields maps a delta type to the field holding its text.
var deltaFields = map[string]string{
	"text_delta":       "text",
	"thinking_delta":   "thinking",
	"input_json_delta": "partial_json",
}

// Event rewrites one complete raw SSE event.
//
// The returned bytes may contain MORE than one event: a content_block_stop with
// bytes still withheld is preceded by a synthetic delta carrying them, so nothing
// is dropped and the stop still arrives last.
//
// An event that needs no change is returned as the original bytes. That is not an
// optimisation — it is what makes "a stream with no sentinel is passed through
// byte-for-byte" true rather than approximately true.
func (r *Restorer) Event(raw []byte) ([]byte, error) {
	line, payload, ok := dataPayload(raw)
	if !ok {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		// Unparseable is the provider's business, not ours. Relaying it untouched
		// beats severing a stream that the client might handle perfectly well.
		return raw, nil
	}

	switch m["type"] {
	case "content_block_delta":
		return r.delta(raw, line, m)
	case "content_block_start":
		return r.blockStart(raw, line, m)
	case "content_block_stop":
		return r.blockStop(raw, m)
	default:
		return raw, nil
	}
}

func (r *Restorer) delta(raw []byte, line int, m map[string]any) ([]byte, error) {
	delta, _ := m["delta"].(map[string]any)
	if delta == nil {
		return raw, nil
	}
	deltaType, _ := delta["type"].(string)
	field, known := deltaFields[deltaType]
	if !known {
		return raw, nil
	}
	text, isString := delta[field].(string)
	if !isString {
		return raw, nil
	}

	st := r.block(indexOf(m), deltaType, field)
	out, err := st.rewrite(text)
	if err != nil {
		return nil, err
	}
	// Unchanged and nothing withheld: hand back the original bytes.
	if out == text && st.rw.Pending() == 0 {
		return raw, nil
	}
	delta[field] = out
	return r.reencode(raw, line, m)
}

func (r *Restorer) blockStart(raw []byte, line int, m map[string]any) ([]byte, error) {
	// A new block supersedes any state under the same index; a stale rewriter
	// here would leak one block's pending bytes into the next.
	idx := indexOf(m)
	delete(r.blocks, idx)

	block, _ := m["content_block"].(map[string]any)
	if block == nil {
		return raw, nil
	}
	text, isString := block["text"].(string)
	if !isString || text == "" {
		return raw, nil
	}
	// A complete value: restore it whole with a throwaway rewriter, so no
	// pending state is created for a block that has not started streaming.
	w := newRewriter(r.table, r.mode, r.onUnresolved)
	out, err := w.Write(text)
	if err != nil {
		return nil, err
	}
	tail, err := w.Flush()
	if err != nil {
		return nil, err
	}
	if out+tail == text {
		return raw, nil
	}
	block["text"] = out + tail
	return r.reencode(raw, line, m)
}

func (r *Restorer) blockStop(raw []byte, m map[string]any) ([]byte, error) {
	idx := indexOf(m)
	st, ok := r.blocks[idx]
	if !ok {
		return raw, nil
	}
	delete(r.blocks, idx)

	tail, err := st.rw.Flush()
	if err != nil {
		return nil, err
	}
	if tail == "" {
		return raw, nil
	}
	synthetic, err := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": st.deltaType, st.field: tail},
	})
	if err != nil {
		return nil, fmt.Errorf("privacy: encode flush event: %w", err)
	}
	out := append([]byte("event: content_block_delta\ndata: "), synthetic...)
	out = append(out, '\n', '\n')
	return append(out, raw...), nil
}

// reencode rebuilds the event around a modified payload.
//
// The payload is re-marshalled from a map, so key order changes and unknown
// fields survive. Order does not matter here: unlike the request side, nothing
// downstream hashes these bytes — the client parses them.
func (r *Restorer) reencode(raw []byte, line int, m map[string]any) ([]byte, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("privacy: encode event: %w", err)
	}
	return replacePayload(raw, line, payload), nil
}

func (r *Restorer) block(idx int, deltaType, field string) *blockState {
	if st, ok := r.blocks[idx]; ok {
		return st
	}
	st := &blockState{
		rw:        newRewriter(r.table, r.mode, r.onUnresolved),
		deltaType: deltaType,
		field:     field,
	}
	r.blocks[idx] = st
	return st
}

// rewrite is the hook Task 11 overrides for input_json_delta, which needs a
// second level of escaping. Every other delta type is plain text.
func (st *blockState) rewrite(text string) (string, error) {
	return st.rw.Write(text)
}

// indexOf reads a content block index. JSON numbers decode as float64 through
// map[string]any; a missing or non-numeric index is treated as block 0, which is
// what a single-block response uses anyway.
func indexOf(m map[string]any) int {
	if f, ok := m["index"].(float64); ok {
		return int(f)
	}
	return 0
}

// Body restores a complete non-streaming response.
//
// It reuses the request side's machinery: the same JSON walker finds string
// values, and each is rewritten whole. Structural keys are skipped for the same
// reason they are on the way in — rewriting "model" or "id" would corrupt the
// response — and the original bytes are returned when nothing changes.
func (r *Restorer) Body(body []byte) ([]byte, error) {
	spans, err := WalkStrings(body)
	if err != nil {
		return body, nil // an unparseable body is the provider's business
	}
	type replacement struct {
		start, end int
		literal    []byte
	}
	var reps []replacement
	for _, span := range spans {
		if SkipKey(span.Key, span.ParentKey) {
			continue
		}
		w := newRewriter(r.table, r.mode, r.onUnresolved)
		out, err := w.Write(span.Value)
		if err != nil {
			return nil, err
		}
		tail, err := w.Flush()
		if err != nil {
			return nil, err
		}
		if out+tail == span.Value {
			continue
		}
		lit, err := json.Marshal(out + tail)
		if err != nil {
			return nil, fmt.Errorf("privacy: encode restored value: %w", err)
		}
		reps = append(reps, replacement{start: span.Start, end: span.End, literal: lit})
	}
	if len(reps) == 0 {
		return body, nil
	}
	out := make([]byte, len(body))
	copy(out, body)
	// Last span first, for the same reason Redact does it: an earlier rewrite
	// would invalidate every offset after it.
	for i := len(reps) - 1; i >= 0; i-- {
		rep := reps[i]
		tail := append([]byte{}, out[rep.end:]...)
		out = append(out[:rep.start], rep.literal...)
		out = append(out, tail...)
	}
	return out, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v -run TestRestorer`
Expected: PASS — all eleven.

- [ ] **Step 6: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): restore placeholders across SSE events"
```

---

### Task 11: `input_json_delta` and the second level of escaping

**Files:**
- Modify: `internal/privacy/rewriter.go` (add the `escape` hook)
- Modify: `internal/privacy/restore.go` (`blockState.rewrite` selects it)
- Create: `internal/privacy/restore_json.go`
- Test: `internal/privacy/restore_json_test.go`

**Interfaces:**
- Consumes: `rewriter`, `blockState`, `Restorer` (Tasks 9, 10).
- Produces: `func jsonInner(s string) string` — JSON string-content escaping without the surrounding quotes.

**This is where a mistake corrupts a file rather than leaking one.**
`input_json_delta` is how the agent writes files: `partial_json` fragments
concatenate into the tool's input document, and a placeholder inside it sits
inside a JSON string literal of *that* document. So a restored value containing
`"`, `\`, or a newline must be escaped for the inner document, and then escaped
again for the `partial_json` string in the SSE event.

No JSON state machine is needed to know the context: **a placeholder can only
appear where redaction put one, which is inside a string value**, so on a match
we are inside a string literal by construction. A model emitting a placeholder
outside one would already be producing invalid JSON, and that case is not
handled.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/restore_json_test.go`:

```go
package privacy

import (
	"encoding/json"
	"strings"
	"testing"
)

func inputJSONDelta(index int, partial string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	})
	return ev(string(b))
}

// assembledInput concatenates every partial_json fragment in a stream and
// unmarshals the result — exactly what the agent does before acting on a tool
// call. If this fails to parse, the agent's file write fails.
func assembledInput(t *testing.T, stream string) map[string]any {
	t.Helper()
	var b strings.Builder
	for _, chunk := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(chunk, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m struct {
				Delta struct {
					Type    string `json:"type"`
					Partial string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("undecodable event: %v", err)
			}
			if m.Delta.Type == "input_json_delta" {
				b.WriteString(m.Delta.Partial)
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(b.String()), &out); err != nil {
		t.Fatalf("assembled tool input is not valid JSON: %v\n%s", err, b.String())
	}
	return out
}

// The headline case: a file write whose content contains a secret carrying every
// character that needs escaping. The agent must receive the original content
// byte-exactly.
func TestInputJSONDeltaRestoresContentExactly(t *testing.T) {
	const secret = "line1\nkey=\"quoted\"\\slash\ttab"
	tab := NewTable(testKey)
	p, err := tab.Add(LabelSecret, secret)
	if err != nil {
		t.Fatal(err)
	}

	// What upstream sends: the tool input document, with the placeholder where
	// the secret was.
	toolInput := map[string]any{"file_path": "/etc/app.conf", "content": "before\n" + p + "\nafter"}
	doc, err := json.Marshal(toolInput)
	if err != nil {
		t.Fatal(err)
	}

	r := NewRestorer(tab, Passthrough, nil)
	// Split the document into 5-byte fragments, the way a real stream arrives.
	var events [][]byte
	for i := 0; i < len(doc); i += 5 {
		j := i + 5
		if j > len(doc) {
			j = len(doc)
		}
		events = append(events, inputJSONDelta(0, string(doc[i:j])))
	}
	events = append(events, ev(`{"type":"content_block_stop","index":0}`))

	got := assembledInput(t, emitted(t, r, events...))
	if got["file_path"] != "/etc/app.conf" {
		t.Errorf("file_path = %v", got["file_path"])
	}
	want := "before\n" + secret + "\nafter"
	if got["content"] != want {
		t.Errorf("content = %q\nwant %q", got["content"], want)
	}
}

// Byte-level invariance: every fragment size must produce the same assembled
// document. A single escaping bug shows up at one particular split.
func TestInputJSONDeltaIsInvariantAcrossFragmentSizes(t *testing.T) {
	const secret = `p@ss"w\ord`
	tab := NewTable(testKey)
	p, _ := tab.Add(LabelSecret, secret)
	doc, _ := json.Marshal(map[string]any{"content": "x" + p + "y"})

	for size := 1; size <= len(doc); size++ {
		r := NewRestorer(tab, Passthrough, nil)
		var events [][]byte
		for i := 0; i < len(doc); i += size {
			j := i + size
			if j > len(doc) {
				j = len(doc)
			}
			events = append(events, inputJSONDelta(0, string(doc[i:j])))
		}
		events = append(events, ev(`{"type":"content_block_stop","index":0}`))

		got := assembledInput(t, emitted(t, r, events...))
		if got["content"] != "x"+secret+"y" {
			t.Fatalf("fragment size %d gave content %q, want %q", size, got["content"], "x"+secret+"y")
		}
	}
}

// With nothing to restore, the fragments must be untouched — a tool call that
// contained no secret must not be re-encoded at all.
func TestInputJSONDeltaWithNothingToRestoreIsByteIdentical(t *testing.T) {
	r := NewRestorer(NewTable(testKey), Passthrough, nil)
	raw := inputJSONDelta(0, `{"file_path":"/a/b","content":"plain"}`)
	out, err := r.Event(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(raw) {
		t.Errorf("fragment was rewritten:\n got %q\nwant %q", out, raw)
	}
}

func TestJSONInnerEscaping(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`plain`, `plain`},
		{`with "quotes"`, `with \"quotes\"`},
		{`back\slash`, `back\\slash`},
		{"new\nline", `new\nline`},
		{"tab\there", `tab\there`},
	} {
		if got := jsonInner(c.in); got != c.want {
			t.Errorf("jsonInner(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The escaping is only applied to input_json_delta. A text_delta carrying the
// same secret must come back raw, because there is only one level of escaping
// there and double-escaping would show the agent literal backslashes.
func TestTextDeltaIsNotDoubleEscaped(t *testing.T) {
	const secret = `has "quotes" and \slash`
	tab := NewTable(testKey)
	p, _ := tab.Add(LabelSecret, secret)
	r := NewRestorer(tab, Passthrough, nil)
	stream := emitted(t, r, textDelta(0, "value: "+p))
	if got := textOf(t, stream); got != "value: "+secret {
		t.Errorf("text delta = %q, want %q", got, "value: "+secret)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run 'TestInputJSON|TestJSONInner|TestTextDeltaIsNot'`
Expected: FAIL — `undefined: jsonInner`, and `TestInputJSONDeltaRestoresContentExactly` failing to parse the assembled document because the plaintext is substituted unescaped.

- [ ] **Step 3: Add the escape hook to the rewriter**

In `internal/privacy/rewriter.go`, add the field and use it at the substitution point:

```go
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
```

`newRewriter` sets `escape: nil`, and the substitution in `Write` becomes:

```go
		if value, found := w.table.Lookup(placeholder); found {
			if w.escape != nil {
				value = w.escape(value)
			}
			out.WriteString(value)
			continue
		}
```

Add a constructor for the escaping variant:

```go
// newJSONRewriter is newRewriter for a stream whose text is JSON string content
// one level down — input_json_delta. See restore_json.go.
func newJSONRewriter(table *Table, mode UnresolvedMode, onUnresolved func(string)) *rewriter {
	w := newRewriter(table, mode, onUnresolved)
	w.escape = jsonInner
	return w
}
```

- [ ] **Step 4: Write the escaping helper and select it per block**

Create `internal/privacy/restore_json.go`:

```go
package privacy

import "encoding/json"

// jsonInner escapes s as JSON string CONTENT — the bytes that would appear
// between the quotes — without the quotes themselves.
//
// This is the inner of two escaping levels on the input_json_delta path. The
// fragments of that delta concatenate into the tool's input document, and a
// placeholder inside it sits inside a string literal of THAT document; so a
// restored value containing a quote, a backslash, or a newline has to be escaped
// for it. The outer level — escaping partial_json itself for the SSE event's
// JSON — is json.Marshal's job when the event is re-encoded.
//
// Getting this wrong does not leak a secret. It produces a tool input the agent
// cannot parse, or worse, one it parses differently: a file write that lands
// mangled content on the operator's disk. That is why it has its own test file.
func jsonInner(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail; returning s unchanged is the
		// conservative branch if it somehow did.
		return s
	}
	return string(b[1 : len(b)-1])
}
```

In `internal/privacy/restore.go`, make the block's rewriter depend on its delta
type, replacing the `block` method and dropping the now-unnecessary
`blockState.rewrite` indirection:

```go
func (r *Restorer) block(idx int, deltaType, field string) *blockState {
	if st, ok := r.blocks[idx]; ok {
		return st
	}
	st := &blockState{deltaType: deltaType, field: field}
	if deltaType == "input_json_delta" {
		st.rw = newJSONRewriter(r.table, r.mode, r.onUnresolved)
	} else {
		st.rw = newRewriter(r.table, r.mode, r.onUnresolved)
	}
	r.blocks[idx] = st
	return st
}

// rewrite feeds text through this block's rewriter. The escaping difference
// between a text stream and a JSON-fragment stream is carried by the rewriter
// chosen in block(), so there is exactly one place that decision is made.
func (st *blockState) rewrite(text string) (string, error) {
	return st.rw.Write(text)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v`
Expected: PASS — every test in the package, including
`TestInputJSONDeltaIsInvariantAcrossFragmentSizes` which exercises every
fragment size, and `TestTextDeltaIsNotDoubleEscaped` which proves the escaping is
scoped to the JSON path only.

- [ ] **Step 6: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): restore inside input_json_delta with correct escaping"
```

---

### Task 12: The Filter facade and per-session stats

**Files:**
- Create: `internal/privacy/filter.go`, `internal/privacy/stats.go`
- Test: `internal/privacy/filter_test.go`

**Interfaces:**
- Consumes: `Redactor`, `Table`, `Restorer`, `UnresolvedMode`, `Detector` (Tasks 3–11).
- Produces:
  - `type FailureMode int` with `Closed`, `Open`; `func ParseFailureMode(s string) (FailureMode, error)`
  - `type Options struct { Detectors []Detector; Cache findingsCache; Key []byte; Unresolved UnresolvedMode; OnScanFailure FailureMode }`
  - `type Filter struct{ ... }`, `func New(o Options) *Filter`
  - `func (f *Filter) Redact(ctx context.Context, body []byte) ([]byte, *Table, error)`
  - `func (f *Filter) Restorer(t *Table) *Restorer`
  - `func (f *Filter) OnScanFailure() FailureMode`
  - `type Snapshot struct { Redactions map[string]int64; Unresolved, CacheHits, CacheMisses int64 }`
  - `func (f *Filter) Snapshot() Snapshot`

`Filter` is the only type `internal/proxy` needs to know about. Everything else in the package is reachable through it, so the proxy never touches a detector, a table, or a rewriter directly.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/filter_test.go`:

```go
package privacy

import (
	"context"
	"strings"
	"testing"
)

func newTestFilter(t *testing.T, dets ...Detector) *Filter {
	t.Helper()
	return New(Options{Detectors: dets, Key: testKey, Unresolved: Passthrough, OnScanFailure: Closed})
}

func TestFilterRedactThenRestoreIsTheIdentity(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	f := newTestFilter(t, det)

	body := []byte(`{"messages":[{"role":"user","content":"the SEKRIT value"}]}`)
	redacted, table, err := f.Redact(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "SEKRIT") {
		t.Fatalf("secret survived redaction: %s", redacted)
	}

	// The response echoes the redacted text back; restoring must reproduce the
	// original exactly. This is property 1 through the public surface.
	r := f.Restorer(table)
	out, err := r.Body(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Errorf("round trip changed the bytes:\n got %s\nwant %s", out, body)
	}
}

func TestFilterCountsRedactionsByLabel(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	f := newTestFilter(t, det)
	if _, _, err := f.Redact(context.Background(),
		[]byte(`{"a":"SEKRIT and SEKRIT","b":"SEKRIT"}`)); err != nil {
		t.Fatal(err)
	}
	snap := f.Snapshot()
	if snap.Redactions["SECRET"] != 1 {
		t.Errorf("Redactions[SECRET] = %d, want 1 — one distinct value however many occurrences",
			snap.Redactions["SECRET"])
	}
}

func TestFilterCountsUnresolvedPlaceholders(t *testing.T) {
	f := newTestFilter(t)
	r := f.Restorer(NewTable(testKey))
	if _, err := r.Body([]byte(`{"a":"orphan [[AIPROXY_SECRET_deadbeef]]"}`)); err != nil {
		t.Fatal(err)
	}
	if got := f.Snapshot().Unresolved; got != 1 {
		t.Errorf("Unresolved = %d, want 1 — this is the one condition that means the agent got something wrong", got)
	}
}

func TestFilterSurfacesAScanError(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "x", err: context.DeadlineExceeded}
	f := newTestFilter(t, det)
	if _, _, err := f.Redact(context.Background(), []byte(`{"a":"a long enough value"}`)); err == nil {
		t.Fatal("Redact hid a detector error; the caller cannot fail closed without it")
	}
}

func TestParseFailureMode(t *testing.T) {
	for in, want := range map[string]FailureMode{"closed": Closed, "open": Open} {
		got, err := ParseFailureMode(in)
		if err != nil || got != want {
			t.Errorf("ParseFailureMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseFailureMode("maybe"); err == nil {
		t.Error("ParseFailureMode accepted an unknown mode; a typo in config must not silently pick one")
	}
}

func TestFilterSnapshotIsSafeUnderConcurrency(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	f := newTestFilter(t, det)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			f.Redact(context.Background(), []byte(`{"a":"SEKRIT here"}`))
		}
	}()
	for i := 0; i < 200; i++ {
		_ = f.Snapshot()
	}
	<-done
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run 'TestFilter|TestParseFailureMode'`
Expected: FAIL — `undefined: New`, `undefined: Options`.

- [ ] **Step 3: Write the stats**

Create `internal/privacy/stats.go`:

```go
package privacy

import "sync"

// Snapshot is the filter's counters at a point in time, for view.Status.
type Snapshot struct {
	// Redactions counts distinct values replaced, per label, since start.
	Redactions map[string]int64
	// Unresolved counts placeholders that reached a client because the table
	// could not resolve them. It is the one counter that means something went
	// WRONG rather than something worked: a non-zero value means the agent
	// received a placeholder where a real value belonged.
	Unresolved int64
	// CacheHits and CacheMisses are how the scan cache is doing. A hit rate that
	// collapses is the first symptom of a cache-key bug, and it shows up as a
	// latency problem long before anyone suspects the key.
	CacheHits, CacheMisses int64
}

// stats is the live counter set. One per Filter, shared by every request, so
// every increment is under the mutex — these are updated once per finding, not
// once per byte, so contention is not a concern.
type stats struct {
	mu          sync.Mutex
	redactions  map[string]int64
	unresolved  int64
	cacheHits   int64
	cacheMisses int64
}

func newStats() *stats { return &stats{redactions: map[string]int64{}} }

func (s *stats) redacted(label Label) {
	s.mu.Lock()
	s.redactions[string(label)]++
	s.mu.Unlock()
}

func (s *stats) unresolvedSeen() {
	s.mu.Lock()
	s.unresolved++
	s.mu.Unlock()
}

func (s *stats) cache(hit bool) {
	s.mu.Lock()
	if hit {
		s.cacheHits++
	} else {
		s.cacheMisses++
	}
	s.mu.Unlock()
}

func (s *stats) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Snapshot{
		Redactions:  make(map[string]int64, len(s.redactions)),
		Unresolved:  s.unresolved,
		CacheHits:   s.cacheHits,
		CacheMisses: s.cacheMisses,
	}
	for k, v := range s.redactions {
		out.Redactions[k] = v
	}
	return out
}
```

- [ ] **Step 4: Write the filter**

Create `internal/privacy/filter.go`:

```go
package privacy

import (
	"context"
	"fmt"
)

// FailureMode selects what happens when the filter cannot do its job on the
// request side.
type FailureMode int

const (
	// Closed refuses the request. This is the default because a privacy filter
	// that degrades silently is worse than no filter at all: the operator
	// believes they are protected exactly when they are not.
	Closed FailureMode = iota
	// Open sends the request unfiltered and records that it did.
	Open
)

func ParseFailureMode(s string) (FailureMode, error) {
	switch s {
	case "closed":
		return Closed, nil
	case "open":
		return Open, nil
	}
	return Closed, fmt.Errorf("privacy: unknown failure mode %q, want closed or open", s)
}

// Options configures a Filter.
type Options struct {
	// Detectors run in order, and that order is the tiebreak Resolve uses for
	// identical spans — so deterministic rules are registered before the model.
	Detectors []Detector
	// Cache may be nil, which disables caching.
	Cache findingsCache
	// Key is the install key placeholders are derived from.
	Key []byte
	// Unresolved selects what a restorer does with a placeholder it cannot
	// resolve.
	Unresolved UnresolvedMode
	// OnScanFailure selects what the CALLER should do when Redact fails; the
	// filter reports it rather than acting on it, because only the caller can
	// write an HTTP response.
	OnScanFailure FailureMode
}

// Filter is the one type internal/proxy needs. Everything else in this package
// is reachable through it, so the proxy never handles a detector, a table, or a
// rewriter directly.
//
// Safe for concurrent use: per-request state lives in the Table that Redact
// returns and the Restorer built from it.
type Filter struct {
	redactor      *Redactor
	key           []byte
	unresolved    UnresolvedMode
	onScanFailure FailureMode
	stats         *stats
}

func New(o Options) *Filter {
	f := &Filter{
		key:           o.Key,
		unresolved:    o.Unresolved,
		onScanFailure: o.OnScanFailure,
		stats:         newStats(),
	}
	cache := o.Cache
	if cache != nil {
		// Wrap so hits and misses are counted without the cache knowing about
		// stats, and without Redactor knowing about either.
		cache = &countingCache{inner: cache, stats: f.stats}
	}
	f.redactor = NewRedactor(o.Detectors, cache)
	return f
}

// OnScanFailure is what the caller should do if Redact returns an error.
func (f *Filter) OnScanFailure() FailureMode { return f.onScanFailure }

// Redact returns the body to send upstream and the table needed to restore the
// response. Both must be used together: the table is the only thing that can
// undo what the body now contains.
func (f *Filter) Redact(ctx context.Context, body []byte) ([]byte, *Table, error) {
	table := NewTable(f.key)
	out, err := f.redactor.Redact(ctx, body, table)
	if err != nil {
		return nil, nil, err
	}
	// Counted after a successful redaction only: a request that failed to scan
	// redacted nothing, and counting it would overstate protection.
	for _, label := range table.labels() {
		f.stats.redacted(label)
	}
	return out, table, nil
}

// Restorer builds the response-side transform for a table Redact returned.
func (f *Filter) Restorer(t *Table) *Restorer {
	return NewRestorer(t, f.unresolved, func(string) { f.stats.unresolvedSeen() })
}

// Snapshot is the counters view.Status reports.
func (f *Filter) Snapshot() Snapshot { return f.stats.snapshot() }

// countingCache records hit rate around any findingsCache.
type countingCache struct {
	inner findingsCache
	stats *stats
}

func (c *countingCache) Get(text string) ([]Finding, bool) {
	findings, ok := c.inner.Get(text)
	c.stats.cache(ok)
	return findings, ok
}

func (c *countingCache) Put(text string, findings []Finding) { c.inner.Put(text, findings) }
```

- [ ] **Step 5: Add `Table.labels`**

In `internal/privacy/table.go`:

```go
// labels is every label minted into this table, one entry per distinct value, so
// Filter can count redactions per label without exposing the mapping itself.
func (t *Table) labels() []Label {
	out := make([]Label, 0, len(t.byValue))
	for vk := range t.byValue {
		out = append(out, vk.label)
	}
	return out
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v -run 'TestFilter|TestParseFailureMode'`
Expected: PASS. `TestFilterSnapshotIsSafeUnderConcurrency` must be clean under `-race`.

- [ ] **Step 7: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): filter facade with per-session counters"
```

---

### Task 13: Wire the filter into the proxy

**Files:**
- Modify: `internal/proxy/attempt.go` (`Request.Restore`, pass into `RelayOptions`)
- Modify: `internal/proxy/relay.go` (`RelayOptions.Restore` and the stream transform)
- Modify: `internal/proxy/handler.go` (`HandlerOptions.Privacy`, redact in `proxyHandler`)
- Test: `internal/proxy/privacy_test.go`

**Interfaces:**
- Consumes: `privacy.Filter`, `privacy.Table`, `privacy.Restorer`, `privacy.FailureMode` (Task 12).
- Produces:
  - `Request.Restore *privacy.Table`
  - `RelayOptions.Restore *privacy.Restorer`
  - `HandlerOptions.Privacy *privacy.Filter`

**Two rules make this safe.** When `RelayOptions.Restore` is nil the relay's write path is **exactly what it is today** — `c.buf` straight to `w`, no accumulation — so a disabled filter costs nothing and property 3 holds trivially. And redaction happens once, in `proxyHandler`, after the blocked-model check and before `o.Attempter.Do`, so retries reuse one redacted body and one table.

Restoration does introduce **one event of buffering** when enabled: a JSON event cannot be rewritten before it has fully arrived. An Anthropic SSE event is a small object that arrives in a single segment, so the delay is sub-millisecond — but it is real, it applies only when the filter is on, and it is recorded here rather than discovered later.

- [ ] **Step 1: Write the failing test**

Create `internal/proxy/privacy_test.go`:

```go
package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/privacy/rules"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func testFilter(t *testing.T, mode privacy.FailureMode) *privacy.Filter {
	t.Helper()
	d, err := rules.New(rules.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return privacy.New(privacy.Options{
		Detectors:     []privacy.Detector{d},
		Key:           []byte("0123456789abcdef0123456789abcdef"),
		Unresolved:    privacy.Passthrough,
		OnScanFailure: mode,
	})
}

// The end-to-end property: a secret in the request never reaches upstream, and
// the agent still sees it in the response.
func TestPrivacyFilterRedactsUpstreamAndRestoresDownstream(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	// Upstream echoes whatever it was sent back inside a text block, so the test
	// observes both directions with one script.
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = testFilter(t, privacy.Closed)
	}, testutil.Script{Status: 200, Body: `{"id":"msg_1","content":[{"type":"text","text":"ECHO"}]}`})

	body := `{"model":"claude-opus-5","messages":[{"role":"user","content":"my key is ` + secret + `"}]}`
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	sent := string(h.upstream.Requests()[0].Body)
	if strings.Contains(sent, secret) {
		t.Fatalf("the secret reached upstream: %s", sent)
	}
	if !strings.Contains(sent, privacy.Sentinel) {
		t.Fatalf("no placeholder in the upstream body: %s", sent)
	}
	if !strings.Contains(sent, `"model":"claude-opus-5"`) {
		t.Errorf("the model was altered: %s", sent)
	}
}

// Fail-closed means closed: upstream must receive NOTHING.
func TestPrivacyFailClosedSendsNothingUpstream(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = privacy.New(privacy.Options{
			Detectors:     []privacy.Detector{errDetector{}},
			Key:           []byte("0123456789abcdef0123456789abcdef"),
			OnScanFailure: privacy.Closed,
		})
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"a long enough value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	if res.StatusCode == 200 {
		t.Errorf("status = 200; a failed scan must not succeed")
	}
	if n := len(h.upstream.Requests()); n != 0 {
		t.Fatalf("upstream received %d requests; fail-closed must send zero", n)
	}
}

func TestPrivacyFailOpenSendsUnfiltered(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = privacy.New(privacy.Options{
			Detectors:     []privacy.Detector{errDetector{}},
			Key:           []byte("0123456789abcdef0123456789abcdef"),
			OnScanFailure: privacy.Open,
		})
	}, testutil.Script{Status: 200, Body: `{}`})

	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[{"role":"user","content":"a long enough value"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	if len(h.upstream.Requests()) != 1 {
		t.Fatalf("fail-open must still send the request; upstream got %d", len(h.upstream.Requests()))
	}
}

// Passthrough paths carry the client's own credential. Filtering one breaks
// authentication, so they must be byte-identical upstream.
func TestPrivacyNeverFiltersPassthroughPaths(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE"
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.Privacy = testFilter(t, privacy.Closed)
	}, testutil.Script{Status: 200, Body: `{}`})

	body := `{"token":"` + secret + `"}`
	res, err := http.Post(h.srv.URL+"/v1/oauth/token", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)

	sent := string(h.upstream.Requests()[0].Body)
	if sent != body {
		t.Errorf("a passthrough body was filtered:\n got %s\nwant %s", sent, body)
	}
}

// With no filter configured, the relay's write path must be untouched.
func TestRelayWithoutARestorerIsUnchanged(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{"content":[{"type":"text","text":"plain"}]}`})
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if string(got) != `{"content":[{"type":"text","text":"plain"}]}` {
		t.Errorf("body = %s", got)
	}
}

// errDetector always fails, so the failure modes can be exercised.
type errDetector struct{}

func (errDetector) Name() string { return "err" }
func (errDetector) Scan(context.Context, string) ([]privacy.Finding, error) {
	return nil, io.ErrUnexpectedEOF
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/proxy -run TestPrivacy`
Expected: FAIL — `o.Privacy` undefined on `HandlerOptions`.

- [ ] **Step 3: Add the relay transform**

In `internal/proxy/relay.go`, add to `RelayOptions`:

```go
	// Restore rewrites the response stream, substituting plaintext back for the
	// placeholders the request carried. Nil — the default, and the only value
	// when the privacy filter is disabled — leaves the write path below exactly
	// as it was: chunks go straight to the client with no accumulation, which is
	// what keeps the filter free when it is off.
	Restore *privacy.Restorer
```

Replace the write section of the `case c, ok := <-chunks:` arm with a call to a
helper, so the transform lives in one place:

```go
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
```

Declare `var events []byte` along`pending` and `captured`, and on the EOF path
flush any trailing partial event before returning:

```go
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
```

Add the helpers at the bottom of the file:

```go
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
```

Add `"fmt"` and `"github.com/nicko170/aiproxy/internal/privacy"` to `relay.go`'s imports.

- [ ] **Step 4: Thread the table through the attempter**

In `internal/proxy/attempt.go`, add to `Request`:

```go
	// Restore carries the placeholders this request's body was redacted with, so
	// the relay can undo them in the response. Nil when the privacy filter is
	// disabled, which leaves the relay's write path untouched.
	Restore *privacy.Table
```

And in the `Relay` call, build the restorer from it:

```go
	ropts := RelayOptions{
		BodyIdle:   a.cfg.BodyIdle,
		Streaming:  streaming,
		ParseUsage: prov.ParseUsageEvent,
		OnUsage:    onUsage,
		ParseBody:  prov.ParseUsageBody,
	}
	if req.Restore != nil && a.privacy != nil {
		ropts.Restore = a.privacy.Restorer(req.Restore)
	}
	n, err := Relay(ctx, w, upstreamRes.Body, ropts)
```

Match the existing field names in that call site rather than the illustrative
`streaming`/`onUsage` above. Add a `privacy *privacy.Filter` field to
`Attempter` and set it from `HandlerOptions` where `NewAttempter` is
constructed, so the attempter can build a restorer without the handler passing
one down per request.

- [ ] **Step 5: Redact in the handler**

In `internal/proxy/handler.go`, add to `HandlerOptions`:

```go
	// Privacy redacts request bodies and restores responses. Nil disables the
	// whole path at zero cost.
	Privacy *privacy.Filter
```

In `proxyHandler`, immediately after the blocked-model loop and before
`o.Attempter.Do`:

```go
		// Redact ONCE, here. Not inside the attempt loop: Attempter replays
		// req.Body on every retry and RewriteBody rewrites it per attempt for
		// model mapping, so redacting there would mint different placeholders on
		// a retry — breaking the prompt cache and leaving the restore table
		// describing a body that was never sent.
		//
		// After the blocked-model check, so routing decisions are made on the
		// client's own values.
		if o.Privacy != nil {
			redacted, table, err := o.Privacy.Redact(r.Context(), req.Body)
			switch {
			case err == nil:
				req.Body = redacted
				req.Restore = table
			case o.Privacy.OnScanFailure() == privacy.Closed:
				o.Log.Error("privacy filter failed; refusing the request", "err", err)
				// 503 when the model is simply not installed or would not load,
				// 500 when a scan went wrong. The distinction is the difference
				// between "run aiproxy privacy install" and "file a bug", and
				// collapsing both into 500 sends the operator to the wrong one.
				status, msg := http.StatusInternalServerError,
					"aiproxy could not scan this request for sensitive data and is configured to fail closed."
				if errors.Is(err, privacy.ErrModelUnavailable) {
					status, msg = http.StatusServiceUnavailable,
						"aiproxy's privacy model is not available. Run \"aiproxy privacy install\", or disable privacy.ner in config."
				}
				writeError(w, status, "api_error", msg)
				return
			default:
				o.Log.Warn("privacy filter failed; sending unfiltered", "err", err)
			}
		}
```

Add `"errors"` and `"github.com/nicko170/aiproxy/internal/privacy"` to
`handler.go`'s imports.

Declare the sentinel in `internal/privacy/filter.go` so `internal/proxy` depends
on `privacy` rather than on `ner`:

```go
// ErrModelUnavailable reports that the NER model is not installed or failed to
// load, as distinct from a scan that went wrong. The control path maps it to 503
// and names the fix, because "install the model" and "this is a bug" are
// different instructions and a single 500 gives the operator neither.
var ErrModelUnavailable = errors.New("privacy: NER model unavailable")
```

and add `"errors"` to that file's imports. `internal/privacy/ner` wraps it around
every load failure, so the sentinel survives `Redact`'s error wrapping and
`errors.Is` finds it at the handler.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test ./internal/proxy -race -run TestPrivacy -v
go test ./... -race
```
Expected: PASS. The full suite must stay green — in particular the existing
relay and lockstep tests, which exercise the `Restore == nil` path and prove the
refactor did not disturb it.

- [ ] **Step 7: Commit**

```bash
git add internal/proxy
git commit -m "feat(proxy): redact requests and restore responses through the filter"
```

---

### Task 14: The scan cache

**Files:**
- Create: `internal/privacy/cache.go`
- Test: `internal/privacy/cache_test.go`

**Interfaces:**
- Consumes: `Finding` (Task 4); satisfies `findingsCache` (Task 8).
- Produces:
  - `type Cache struct{ ... }`, `func NewCache(maxEntries int, salt string) *Cache`
  - `func (c *Cache) Get(text string) ([]Finding, bool)`, `func (c *Cache) Put(text string, findings []Finding)`
  - `func (c *Cache) Len() int`
  - `func Salt(parts ...string) string`

**The key is a hash, and that is not an optimisation.** Using the text itself as a map key would hold every scanned string — secrets included — in a structure that outlives the request, breaking property 5. The cache stores `SHA256(salt ‖ text) → []Finding`: byte offsets and labels, and nothing that could reconstruct a value.

**Bounded LRU, not a TTL.** Content → findings is a pure function and never goes stale, so a TTL buys no correctness — only memory bounding, which an LRU does better. A 60-minute expiry would evict entries that are *still being resent every turn*, producing a rescan storm mid-conversation exactly when the context is largest and a model call hurts most. The salt handles freshness instead: change a rule toggle or the model version and every key changes with it.

- [ ] **Step 1: Write the failing test**

Create `internal/privacy/cache_test.go`:

```go
package privacy

import (
	"context"
	"strings"
	"testing"
)

func TestCacheHitsAvoidRescanning(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	c := NewCache(100, Salt("v1"))
	r := NewRedactor([]Detector{det}, c)

	body := []byte(`{"a":"the SEKRIT value","b":"unrelated but long enough"}`)
	if _, err := r.Redact(context.Background(), body, NewTable(testKey)); err != nil {
		t.Fatal(err)
	}
	firstCalls := det.calls
	if firstCalls == 0 {
		t.Fatal("the detector was never called")
	}

	// The same body again: every string is already cached, so the detector must
	// not be consulted at all. This is the property that makes the model
	// affordable — a coding agent resends its whole history every turn.
	if _, err := r.Redact(context.Background(), body, NewTable(testKey)); err != nil {
		t.Fatal(err)
	}
	if det.calls != firstCalls {
		t.Errorf("detector was called %d more times on an identical body", det.calls-firstCalls)
	}
}

// Only genuinely new content is scanned when a turn is appended.
func TestCacheScansOnlyNewContent(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	c := NewCache(100, Salt("v1"))
	r := NewRedactor([]Detector{det}, c)

	turn1 := []byte(`{"messages":[{"content":"first message here"}]}`)
	if _, err := r.Redact(context.Background(), turn1, NewTable(testKey)); err != nil {
		t.Fatal(err)
	}
	before := det.calls

	turn2 := []byte(`{"messages":[{"content":"first message here"},{"content":"second message here"}]}`)
	if _, err := r.Redact(context.Background(), turn2, NewTable(testKey)); err != nil {
		t.Fatal(err)
	}
	if got := det.calls - before; got != 1 {
		t.Errorf("scanned %d strings on the second turn, want 1 (only the new message)", got)
	}
}

// A different salt is a different cache. This is how a rule toggle or a model
// upgrade invalidates without any expiry logic.
func TestCacheSaltInvalidates(t *testing.T) {
	det := &fakeDetector{name: "fake", needle: "SEKRIT", label: LabelSecret}
	body := []byte(`{"a":"the SEKRIT value"}`)

	c1 := NewCache(100, Salt("rules=on", "entropy=on"))
	r1 := NewRedactor([]Detector{det}, c1)
	if _, err := r1.Redact(context.Background(), body, NewTable(testKey)); err != nil {
		t.Fatal(err)
	}
	before := det.calls

	// Same cache instance would hit; a differently-salted one must not.
	c2 := NewCache(100, Salt("rules=on", "entropy=off"))
	r2 := NewRedactor([]Detector{det}, c2)
	if _, err := r2.Redact(context.Background(), body, NewTable(testKey)); err != nil {
		t.Fatal(err)
	}
	if det.calls == before {
		t.Error("a changed salt reused cached findings; turning a rule off would leave its findings applied")
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewCache(2, Salt("v1"))
	c.Put("one", []Finding{{Start: 0, End: 1}})
	c.Put("two", nil)
	if _, ok := c.Get("one"); !ok {
		t.Fatal("one was evicted too early")
	}
	// "one" is now the most recently used, so "two" goes first.
	c.Put("three", nil)
	if _, ok := c.Get("two"); ok {
		t.Error("two survived; eviction is not least-recently-used")
	}
	if _, ok := c.Get("one"); !ok {
		t.Error("one was evicted despite being used most recently")
	}
	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2", c.Len())
	}
}

// Property 5: the cache must not hold the text it was asked about.
func TestCacheDoesNotRetainPlaintext(t *testing.T) {
	c := NewCache(10, Salt("v1"))
	const secret = "AKIAIOSFODNN7EXAMPLE-very-distinctive"
	c.Put(secret, []Finding{{Start: 0, End: 5, Label: LabelSecret}})

	for _, k := range c.keysForTest() {
		if strings.Contains(k, "AKIA") || strings.Contains(k, "distinctive") {
			t.Fatalf("a cache key contains the plaintext: %q", k)
		}
	}
}

func TestCacheStoresEmptyFindingsAsAHit(t *testing.T) {
	c := NewCache(10, Salt("v1"))
	c.Put("clean text", nil)
	findings, ok := c.Get("clean text")
	if !ok {
		t.Fatal("a clean string was not cached; the common case would rescan every turn")
	}
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
}

func TestCacheIsSafeUnderConcurrency(t *testing.T) {
	c := NewCache(64, Salt("v1"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			c.Put("key", []Finding{{Start: 0, End: 1}})
		}
	}()
	for i := 0; i < 1000; i++ {
		c.Get("key")
	}
	<-done
}

func TestNewCacheClampsANonPositiveBound(t *testing.T) {
	c := NewCache(0, Salt("v1"))
	c.Put("a", nil)
	if c.Len() == 0 {
		t.Error("a zero bound disabled the cache entirely; it should clamp to a usable default")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/privacy -run 'TestCache|TestNewCache'`
Expected: FAIL — `undefined: NewCache`, `undefined: Salt`.

- [ ] **Step 3: Write the implementation**

Create `internal/privacy/cache.go`:

```go
package privacy

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// defaultCacheEntries is the fallback bound. Findings are almost always empty
// and a key is 64 hex characters, so tens of thousands of entries is a few
// megabytes — cheap next to what it saves.
const defaultCacheEntries = 50_000

// Salt combines the inputs that change what a scan MEANS into one string, mixed
// into every cache key.
//
// It stands in for expiry. Content -> findings is a pure function, so a cached
// entry never goes stale on its own; what makes it wrong is a change to the
// rules, the model, or the enabled label set. Putting those in the key means such
// a change invalidates everything automatically, and nothing has to be timed out
// "just in case".
func Salt(parts ...string) string { return strings.Join(parts, "\x00") }

// Cache remembers findings per scanned string, bounded by an LRU.
//
// It stores SHA256(salt || text) -> []Finding. Hashing the key is not an
// optimisation: using the text itself would hold every scanned string —
// credentials included — in a structure outliving the request. Findings are byte
// offsets and labels, from which no value can be reconstructed.
//
// Safe for concurrent use: one cache is shared by every in-flight request.
type Cache struct {
	mu      sync.Mutex
	salt    string
	max     int
	lru     *list.List               // front = most recently used
	entries map[string]*list.Element // key -> element holding *cacheEntry
}

type cacheEntry struct {
	key      string
	findings []Finding
}

func NewCache(maxEntries int, salt string) *Cache {
	if maxEntries <= 0 {
		maxEntries = defaultCacheEntries
	}
	return &Cache{
		salt:    salt,
		max:     maxEntries,
		lru:     list.New(),
		entries: make(map[string]*list.Element, maxEntries/8+1),
	}
}

func (c *Cache) key(text string) string {
	h := sha256.New()
	h.Write([]byte(c.salt))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

// Get returns the cached findings for text. A cached EMPTY result is a hit, not a
// miss — most strings contain nothing sensitive, so caching "clean" is where most
// of the saving comes from.
func (c *Cache) Get(text string) ([]Finding, bool) {
	k := c.key(text)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return el.Value.(*cacheEntry).findings, true
}

// Put records findings for text, evicting the least recently used entry if the
// cache is full.
func (c *Cache) Put(text string, findings []Finding) {
	k := c.key(text)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		el.Value.(*cacheEntry).findings = findings
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&cacheEntry{key: k, findings: findings})
	c.entries[k] = el
	for c.lru.Len() > c.max {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*cacheEntry).key)
	}
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// keysForTest exposes the stored keys so a test can assert no plaintext is
// retained.
func (c *Cache) keysForTest() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.entries))
	for k := range c.entries {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/privacy -race -v -run 'TestCache|TestNewCache'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/privacy
git commit -m "feat(privacy): bounded LRU scan cache keyed on hashed content"
```

---

### Task 15: Configuration, the seam, and the TUI

**Files:**
- Modify: `internal/config/config.go`, `internal/config/store.go`
- Modify: `internal/view/types.go`, `internal/view/local.go`, `internal/view/settings.go`
- Modify: `internal/tui/app.go`, `internal/tui/settings.go`, `internal/tui/activity.go`, `internal/tui/frames_test.go`
- Modify: `cmd/aiproxy/main.go`
- Test: `internal/config/store_test.go`, `internal/view/local_test.go`, `internal/tui/behavior_test.go`

**Interfaces:**
- Consumes: `privacy.Filter`, `privacy.Snapshot`, `privacy.ParseFailureMode` (Task 12); `privacy.NewCache`, `privacy.Salt` (Task 14); `rules.New`, `rules.NewDenylist`, `rules.Builtin` (Tasks 5–7).
- Produces:
  - `config.Privacy` and `config.PrivacyNER`
  - `view.PrivacyStatus` on `view.Status.Privacy`
  - `view.Settings` gains `PrivacyEnabled`, `PrivacyOnScanFailure`, `PrivacyOnUnresolved`, `PrivacyDenylist`
  - `func buildPrivacy(cfg config.Config) (*privacy.Filter, error)` in `cmd/aiproxy`

**No new `view.Source` method, and therefore no new route.** Assets download lazily on enable and `aiproxy privacy install` is a CLI path, so `TestEveryViewSourceMethodHasAControlRoute` stays green without a `routeFor` entry.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/store_test.go`:

```go
func TestPrivacyDefaultsAreOffAndSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Privacy.Enabled {
		t.Error("the privacy filter must be off by default; it rewrites request bodies")
	}
	if cfg.Privacy.OnScanFailure != "closed" {
		t.Errorf("OnScanFailure = %q, want closed", cfg.Privacy.OnScanFailure)
	}
	if cfg.Privacy.OnUnresolvedPlaceholder != "passthrough" {
		t.Errorf("OnUnresolvedPlaceholder = %q, want passthrough", cfg.Privacy.OnUnresolvedPlaceholder)
	}
	if !cfg.Privacy.Rules.BuiltinSecrets || !cfg.Privacy.Rules.Entropy {
		t.Error("enabling the filter should bring the deterministic rules with it")
	}
	if cfg.Privacy.NER.Enabled {
		t.Error("the model must be off by default")
	}
	if len(cfg.Privacy.NER.Labels) != 0 {
		t.Errorf("NER labels = %v, want none by default", cfg.Privacy.NER.Labels)
	}
	if cfg.Privacy.CacheEntries != 50000 {
		t.Errorf("CacheEntries = %d, want 50000", cfg.Privacy.CacheEntries)
	}
	if cfg.Privacy.NER.MaxScanBytes != 262144 {
		t.Errorf("MaxScanBytes = %d, want 262144", cfg.Privacy.NER.MaxScanBytes)
	}
}

func TestPrivacyNonPositiveBoundsFallBackToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"privacy":{"cacheEntries":0,"ner":{"maxScanBytes":-1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Privacy.CacheEntries <= 0 || cfg.Privacy.NER.MaxScanBytes <= 0 {
		t.Errorf("bounds were not corrected on load: %+v", cfg.Privacy)
	}
}
```

Add to `internal/view/local_test.go`:

```go
func TestStatusReportsPrivacyDisabledWithoutAFilter(t *testing.T) {
	local := newHarness(t).local
	st, err := local.ServerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Privacy.Enabled {
		t.Error("Privacy.Enabled should be false with no filter attached")
	}
	if st.Privacy.ModelState != "off" {
		t.Errorf("ModelState = %q, want off", st.Privacy.ModelState)
	}
}

func TestPrivacySettingsRoundTrip(t *testing.T) {
	local := newHarness(t).local
	s, err := local.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.PrivacyEnabled {
		t.Fatal("default should be disabled")
	}
	s.PrivacyEnabled = true
	s.PrivacyDenylist = []string{"acme-prod.internal"}
	applied, err := local.UpdateSettings(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied.NeedsRestart, "privacyEnabled") {
		t.Errorf("NeedsRestart = %v, want privacyEnabled", applied.NeedsRestart)
	}
	back, err := local.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !back.PrivacyEnabled || len(back.PrivacyDenylist) != 1 {
		t.Errorf("settings did not persist: %+v", back)
	}
}

func TestValidateRejectsAnUnknownFailureMode(t *testing.T) {
	s := Settings{
		SwitchThreshold: 0.9, RetryBudgetMS: 1000, HeaderTimeoutMS: 1000, BodyIdleMS: 1000,
		QuotaProbeIntervalSeconds: 300, MetricsRetentionDays: 90,
		UpdateCheckEnabled: true, UpdateCheckIntervalHours: 24,
		PrivacyOnScanFailure: "maybe", PrivacyOnUnresolved: "passthrough",
	}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown scan-failure mode")
	}
}
```

Add to `internal/tui/behavior_test.go`:

```go
func TestHeaderShowsRedactionCount(t *testing.T) {
	m := fixtureModel(120, 28)
	if strings.Contains(m.viewHeader(), "redacted") {
		t.Fatal("header mentions redactions with the filter off")
	}
	m.status.Privacy = view.PrivacyStatus{
		Enabled: true, ModelState: "ready",
		Redactions: map[string]int64{"SECRET": 9, "EMAIL": 3},
	}
	if got := m.viewHeader(); !strings.Contains(got, "12 redacted") {
		t.Errorf("header = %q, want a total of 12 redacted", got)
	}
}

// Unresolved placeholders are the one privacy condition that means the agent
// received something wrong, so they must read as a warning rather than a count.
func TestHeaderWarnsOnUnresolvedPlaceholders(t *testing.T) {
	m := fixtureModel(120, 28)
	m.status.Privacy = view.PrivacyStatus{Enabled: true, ModelState: "ready", Unresolved: 2}
	if got := m.viewHeader(); !strings.Contains(got, "2 unresolved") {
		t.Errorf("header = %q, want it to surface unresolved placeholders", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config ./internal/view ./internal/tui -run 'Privacy|TestHeaderShowsRedaction|TestHeaderWarnsOnUnresolved|TestValidateRejectsAnUnknown'`
Expected: FAIL — `cfg.Privacy` undefined, `st.Privacy` undefined.

- [ ] **Step 3: Add the config block**

In `internal/config/config.go`, after `Update`:

```go
// PrivacyRules toggles the deterministic detectors. Both default on, because
// they are the reason to enable the filter at all.
type PrivacyRules struct {
	BuiltinSecrets bool `json:"builtinSecrets"`
	Entropy        bool `json:"entropy"`
}

// PrivacyNER configures the local NER model.
//
// Labels is empty by default and every entry is opt-in. private_url and
// private_date in particular would be destructive in source code, where import
// URLs, API endpoints, doc links, changelog dates, and licence years are
// everywhere — redacting them corrupts the agent's context for almost no privacy
// gain.
type PrivacyNER struct {
	Enabled      bool     `json:"enabled"`
	Labels       []string `json:"labels"`
	MaxScanBytes int      `json:"maxScanBytes"`
}

// Privacy configures the local privacy filter.
//
// Enabled defaults to FALSE. The filter rewrites request bodies, and that is not
// a behaviour to acquire silently on upgrade — but once it is on, the
// deterministic rules come with it.
type Privacy struct {
	Enabled                 bool         `json:"enabled"`
	OnScanFailure           string       `json:"onScanFailure"`
	OnUnresolvedPlaceholder string       `json:"onUnresolvedPlaceholder"`
	Rules                   PrivacyRules `json:"rules"`
	Denylist                []string     `json:"denylist"`
	AllowlistExtra          []string     `json:"allowlistExtra"`
	CacheEntries            int          `json:"cacheEntries"`
	NER                     PrivacyNER   `json:"ner"`
}
```

Add `Privacy Privacy \`json:"privacy"\`` to `Config`, and to `Default()`:

```go
		Privacy: Privacy{
			Enabled:                 false,
			OnScanFailure:           "closed",
			OnUnresolvedPlaceholder: "passthrough",
			Rules:                   PrivacyRules{BuiltinSecrets: true, Entropy: true},
			Denylist:                []string{},
			AllowlistExtra:          []string{},
			CacheEntries:            50000,
			NER:                     PrivacyNER{Enabled: false, Labels: []string{}, MaxScanBytes: 262144},
		},
```

In `internal/config/store.go`'s `loadLocked`, beside the update-interval guard:

```go
	// Bounds handed to an LRU and to a scan budget; a hand-edited 0 or negative
	// would disable the cache or the model silently.
	if cfg.Privacy.CacheEntries <= 0 {
		cfg.Privacy.CacheEntries = Default().Privacy.CacheEntries
	}
	if cfg.Privacy.NER.MaxScanBytes <= 0 {
		cfg.Privacy.NER.MaxScanBytes = Default().Privacy.NER.MaxScanBytes
	}
	if cfg.Privacy.OnScanFailure == "" {
		cfg.Privacy.OnScanFailure = Default().Privacy.OnScanFailure
	}
	if cfg.Privacy.OnUnresolvedPlaceholder == "" {
		cfg.Privacy.OnUnresolvedPlaceholder = Default().Privacy.OnUnresolvedPlaceholder
	}
```

- [ ] **Step 4: Add the seam types**

In `internal/view/types.go`, add to `Status` after `Update`:

```go
	// Privacy reports the local privacy filter, following the same precedent as
	// Probe and Update: a fact about the running instance, rendered by the poll
	// the TUI already makes.
	Privacy PrivacyStatus `json:"privacy"`
```

```go
// PrivacyStatus is the privacy filter's state and counters.
type PrivacyStatus struct {
	Enabled bool `json:"enabled"`
	// ModelState is off, absent, downloading, loading, ready, or error.
	ModelState    string `json:"modelState"`
	DownloadedPct int    `json:"downloadedPct,omitempty"`
	// Redactions counts distinct values replaced, per label, this session.
	Redactions map[string]int64 `json:"redactions"`
	// CacheHitRate is 0..1. A collapsed rate is the first symptom of a cache-key
	// bug, which otherwise shows up only as unexplained latency.
	CacheHitRate float64 `json:"cacheHitRate"`
	// Unresolved counts placeholders that reached a client unresolved. It is the
	// one counter here that means something went wrong rather than right.
	Unresolved int64  `json:"unresolved"`
	LastError  string `json:"lastError,omitempty"`
}
```

Add to `Settings`:

```go
	// PrivacyEnabled and the two failure modes configure the local privacy
	// filter. Enabled is restart-gated: the detector set, the cache salt, and the
	// model session are all built once at startup.
	PrivacyEnabled       bool     `json:"privacyEnabled"`
	PrivacyOnScanFailure string   `json:"privacyOnScanFailure"`
	PrivacyOnUnresolved  string   `json:"privacyOnUnresolved"`
	PrivacyDenylist      []string `json:"privacyDenylist"`
```

In `internal/view/settings.go`'s `Validate`:

```go
	if _, err := privacy.ParseFailureMode(s.PrivacyOnScanFailure); err != nil {
		return err
	}
	switch s.PrivacyOnUnresolved {
	case "passthrough", "error":
	default:
		return fmt.Errorf("privacyOnUnresolved must be \"passthrough\" or \"error\", got %q", s.PrivacyOnUnresolved)
	}
```

- [ ] **Step 5: Wire the seam**

In `internal/view/local.go`: add a `privacy *privacy.Filter` field, a
`NewLocal` parameter after `upd`, `Privacy: l.privacyStatus()` in
`ServerStatus`, the four `Settings` fields in `settingsFromConfig` and
`UpdateSettings`, `privacyEnabled`/`privacyOnScanFailure`/`privacyOnUnresolved`/`privacyDenylist`
in `diffSettings`, and all four in `restartSettingsFields`. Then:

```go
// privacyStatus converts the filter's counters into the view shape. Like
// probeStatus and updateStatus it performs no I/O — Snapshot reads counters
// behind a mutex — so a status poll costs the same whether the filter is busy or
// idle. A nil filter reports "off" rather than an empty "enabled" state.
func (l *Local) privacyStatus() PrivacyStatus {
	if l.privacy == nil {
		return PrivacyStatus{ModelState: "off", Redactions: map[string]int64{}}
	}
	snap := l.privacy.Snapshot()
	out := PrivacyStatus{
		Enabled:    true,
		ModelState: l.privacy.ModelState(),
		Redactions: snap.Redactions,
		Unresolved: snap.Unresolved,
	}
	if total := snap.CacheHits + snap.CacheMisses; total > 0 {
		out.CacheHitRate = float64(snap.CacheHits) / float64(total)
	}
	return out
}
```

Add `ModelState() string` to `privacy.Filter`, returning `"off"` until Task 18
gives it a model to report on:

```go
// ModelState describes the NER model's readiness. Until a model is configured it
// is "off" — a filter running deterministic rules only is fully functional, and
// reporting "absent" would imply something is missing that is not.
func (f *Filter) ModelState() string {
	if f.modelState == nil {
		return "off"
	}
	return f.modelState()
}
```

with a `modelState func() string` field on `Options` and `Filter`.

- [ ] **Step 6: Construct the filter in cmd/aiproxy**

In `cmd/aiproxy/main.go`, add and call from `buildHandler`:

```go
// buildPrivacy assembles the privacy filter from config, or returns nil when it
// is disabled — which is the default. Detector ORDER is significant: it is the
// tiebreak privacy.Resolve uses for identical spans, so the deterministic rules
// are registered before the model.
func buildPrivacy(cfg config.Config) (*privacy.Filter, error) {
	if !cfg.Privacy.Enabled {
		return nil, nil
	}
	key, err := privacy.LoadOrCreateKey(privacy.KeyPath())
	if err != nil {
		return nil, err
	}
	scanFail, err := privacy.ParseFailureMode(cfg.Privacy.OnScanFailure)
	if err != nil {
		return nil, err
	}
	unresolved := privacy.Passthrough
	if cfg.Privacy.OnUnresolvedPlaceholder == "error" {
		unresolved = privacy.ErrorOut
	}

	var dets []privacy.Detector
	if cfg.Privacy.Rules.BuiltinSecrets {
		rd, err := rules.New(rules.Builtin(), cfg.Privacy.AllowlistExtra)
		if err != nil {
			return nil, err
		}
		dets = append(dets, rd)
	}
	if len(cfg.Privacy.Denylist) > 0 {
		dl, err := rules.NewDenylist(cfg.Privacy.Denylist)
		if err != nil {
			return nil, err
		}
		dets = append(dets, dl)
	}

	// The salt carries everything that changes what a scan MEANS, so a toggle or
	// a denylist edit invalidates the cache without any expiry logic.
	salt := privacy.Salt(
		"rules=v1",
		fmt.Sprintf("builtin=%t", cfg.Privacy.Rules.BuiltinSecrets),
		fmt.Sprintf("entropy=%t", cfg.Privacy.Rules.Entropy),
		fmt.Sprintf("deny=%d", len(cfg.Privacy.Denylist)),
		strings.Join(cfg.Privacy.NER.Labels, ","),
	)
	return privacy.New(privacy.Options{
		Detectors:     dets,
		Cache:         privacy.NewCache(cfg.Privacy.CacheEntries, salt),
		Key:           key,
		Unresolved:    unresolved,
		OnScanFailure: scanFail,
	}), nil
}
```

Call it in `buildHandler`, pass the result to `view.NewLocal` and to
`proxy.HandlerOptions.Privacy`, and log once at startup when it is active so an
operator can see it took effect:

```go
	pf, err := buildPrivacy(cfg)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("privacy filter: %w", err)
	}
	if pf != nil {
		log.Info("privacy filter active",
			"onScanFailure", cfg.Privacy.OnScanFailure,
			"denylist", len(cfg.Privacy.Denylist),
			"nerLabels", len(cfg.Privacy.NER.Labels))
	}
```

Update the two other `view.NewLocal` call sites to pass `nil`.

- [ ] **Step 7: Add the TUI surfaces**

In `internal/tui/app.go`, extend `updateSegments`-style shedding with a privacy
segment placed **before** the update segment, so an update notice is shed first:

```go
// privacySegments returns the header's privacy wordings, longest first. Unresolved
// placeholders outrank the redaction count: a count means the filter is working,
// whereas an unresolved placeholder means the agent received something wrong,
// which is the more urgent of the two.
func (m Model) privacySegments() []string {
	th := m.th
	p := m.status.Privacy
	if !p.Enabled {
		return nil
	}
	glyph := "⊘ "
	if th.mode == modeNone {
		glyph = "[!] "
	}
	if p.Unresolved > 0 {
		return []string{
			th.bad(fmt.Sprintf("%s%d unresolved", glyph, p.Unresolved)),
			th.bad(fmt.Sprintf("%d unresolved", p.Unresolved)),
		}
	}
	var total int64
	for _, n := range p.Redactions {
		total += n
	}
	if total == 0 {
		return nil
	}
	return []string{
		th.dim(fmt.Sprintf("%s%d redacted", glyph, total)),
		th.dim(fmt.Sprintf("%d redacted", total)),
	}
}
```

Apply it in `viewHeader` with the same ladder mechanism the update segment uses,
trying each candidate until one fits. Add two settings rows in
`internal/tui/settings.go` (`privacyEnabled` boolean, `privacyDenylist`
comma-separated, matching `blockedModels`' handling), a per-request redaction
count column in `internal/tui/activity.go` only when
`m.status.Privacy.Enabled`, and the new `Settings` fields to `fixtureModel`.

- [ ] **Step 8: Run the tests and regenerate goldens**

```bash
go test ./internal/config ./internal/view ./internal/privacy/... -race
go test ./internal/tui -run TestGoldenFrames -update
git diff --stat internal/tui/testdata
git diff internal/tui/testdata
go test ./... -race
```
Expected: the settings goldens change (two new rows) and no others, because the
privacy header segment is empty with the filter off. **Read the diff.** If an
`overview_*` or `activity_*` golden moved, the segment or column is rendering
when it should not.

- [ ] **Step 9: Commit**

```bash
git add internal/config internal/view internal/tui cmd/aiproxy
git commit -m "feat(privacy): configuration, status, and TUI surfaces"
```

---

## Part 2 — Tier 2: the model

Everything above is a complete, shippable filter for credentials and internal
identifiers. Task 16 is a **gate**, not a task: if a Go tokenizer cannot
reproduce the reference implementation's offsets exactly, Part 2 stops there and
Part 1 ships alone. That is an acceptable outcome and a far better one than spans
that are one character out.

---

### Task 16 (GATE): A byte-level BPE tokenizer with exact offsets

**Files:**
- Create: `internal/privacy/tokenizer/tokenizer.go`, `internal/privacy/tokenizer/tokenizer_test.go`
- Create: `internal/privacy/tokenizer/testdata/offsets.json` (generated, committed)
- Create: `scripts/gen-tokenizer-fixtures.py` (one-time generator, committed for reproducibility)
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: nothing from Part 1.
- Produces:
  - `type Token struct { ID int; Start, End int }` — `Start`/`End` are **byte offsets into the input string**
  - `type Tokenizer struct{ ... }`, `func Load(path string) (*Tokenizer, error)`
  - `func (t *Tokenizer) Encode(s string) ([]Token, error)`

**What is already known** (from the model's own config, so it is fact rather than assumption): `vocab_size` is 200 064 with `<|endoftext|>` as both EOS and PAD, which is the o200k family; the architecture is `OpenAIPrivacyFilterForTokenClassification` with 640 hidden, 8 layers, 14 heads, 128 experts and 4 per token; and there are 33 labels — 8 categories × BIOES + `O`. Inputs are `input_ids` and `attention_mask`.

**Why offsets are tractable here.** Byte-level BPE has no whitespace normalisation and no lossy pre-processing: every token decodes to a definite byte sequence, so a token's span is simply the running total of the decoded lengths before it. That is a much better position than a WordPiece tokenizer, where offsets have to be reconstructed against a normalised string.

**Why `regexp2`.** The o200k pretokenizer pattern uses lookahead, which Go's RE2 does not support. `github.com/dlclark/regexp2` is pure Go and is what `tiktoken-go` uses for the same reason, so `CGO_ENABLED=0` is unaffected.

- [ ] **Step 1: Inspect the real tokenizer file and record what it is**

```bash
mkdir -p /tmp/pf && cd /tmp/pf
curl -fsSL -o tokenizer.json https://huggingface.co/openai/privacy-filter/resolve/main/tokenizer.json
python3 - <<'PY'
import json
t = json.load(open('tokenizer.json'))
print('model.type      =', t['model']['type'])
print('vocab size      =', len(t['model'].get('vocab', {})))
print('merges          =', len(t['model'].get('merges', [])))
print('byte_fallback   =', t['model'].get('byte_fallback'))
print('normalizer      =', json.dumps(t.get('normalizer'))[:200])
print('pre_tokenizer   =', json.dumps(t.get('pre_tokenizer'))[:600])
print('decoder         =', json.dumps(t.get('decoder'))[:200])
print('added_tokens    =', [a['content'] for a in t.get('added_tokens', [])][:10])
PY
```

Record the output in the commit message for this task. Two branches follow:

- **`model.type == "BPE"` with a `ByteLevel` pretokenizer or a `Split` on a
  regex** — the expected case. Proceed to Step 2.
- **Anything else** (Unigram, WordPiece, a normalizer that rewrites the input) —
  stop and report. Offsets under a normalizer that changes byte lengths are a
  different and much harder problem, and the gate has failed.

- [ ] **Step 2: Generate offset fixtures from the reference implementation**

Create `scripts/gen-tokenizer-fixtures.py`:

```python
#!/usr/bin/env python3
"""Generate tokenizer offset fixtures for internal/privacy/tokenizer.

The Go tokenizer must agree with this reference EXACTLY. A one-character
disagreement means the NER detector reports spans that redact the wrong bytes —
silently, in a component whose entire value is being trustworthy. So the fixtures
are generated once, committed, and asserted against.

Usage:  uv run --with tokenizers scripts/gen-tokenizer-fixtures.py <tokenizer.json> <out.json>
"""
import json
import sys

from tokenizers import Tokenizer

CASES = [
    "",
    "hello world",
    "Contact ada@example.com or call +44 20 7946 0958.",
    "AKIAIOSFODNN7EXAMPLE",
    "Ada Lovelace lived at 12 Rue de Rivoli, Paris.",
    # Multi-byte, combining characters, and emoji: where offset bugs live.
    "héllo wörld",
    "é vs é",
    "emoji 😀 then text",
    "日本語のテキストです",
    # Whitespace shapes, which byte-level BPE encodes into tokens of their own.
    "  leading and trailing  ",
    "tabs\tand\nnewlines\r\n",
    "  ",
    # Code, which is what this filter actually sees most of.
    'const key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz";',
    "func main() {\n\tfmt.Println(x[0])\n}",
    # A long input, to exercise merge behaviour at scale.
    "lorem ipsum dolor sit amet " * 40,
]


def main() -> None:
    tok = Tokenizer.from_file(sys.argv[1])
    out = []
    for text in CASES:
        enc = tok.encode(text, add_special_tokens=False)
        out.append({
            "text": text,
            "ids": enc.ids,
            # Byte offsets. The tokenizers library reports character offsets for
            # some configurations, so they are converted here rather than in Go:
            # the reference is the authority on what a span means.
            "offsets": [
                [len(text[:s].encode()), len(text[:e].encode())]
                for s, e in enc.offsets
            ],
        })
    json.dump(out, open(sys.argv[2], "w"), ensure_ascii=False, indent=1)
    print(f"wrote {len(out)} cases", file=sys.stderr)


if __name__ == "__main__":
    main()
```

```bash
uv run --with tokenizers scripts/gen-tokenizer-fixtures.py \
    /tmp/pf/tokenizer.json internal/privacy/tokenizer/testdata/offsets.json
```

Commit `offsets.json`. It is the contract; regenerating it to make a test pass is
the one thing that must never happen.

- [ ] **Step 3: Write the failing test**

Create `internal/privacy/tokenizer/tokenizer_test.go`:

```go
package tokenizer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	Text    string  `json:"text"`
	IDs     []int   `json:"ids"`
	Offsets [][2]int `json:"offsets"`
}

// modelPath is where Task 17's asset fetch puts tokenizer.json. The gate test
// skips when it is absent so `go test ./...` never depends on a download.
func modelPath(t *testing.T) string {
	t.Helper()
	p := os.Getenv("AIPROXY_TOKENIZER_JSON")
	if p == "" {
		t.Skip("set AIPROXY_TOKENIZER_JSON to the model's tokenizer.json to run the gate")
	}
	return p
}

// THE GATE. Exact agreement with the reference on ids and byte offsets, for
// every fixture. Nothing downstream of the tokenizer may be built until this
// passes, because a span that is one byte out redacts the wrong text.
func TestEncodeMatchesReferenceExactly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "offsets.json"))
	if err != nil {
		t.Fatalf("fixtures missing; run scripts/gen-tokenizer-fixtures.py: %v", err)
	}
	var fixtures []fixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}

	tok, err := Load(modelPath(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i, f := range fixtures {
		got, err := tok.Encode(f.Text)
		if err != nil {
			t.Errorf("case %d (%q): Encode: %v", i, trunc(f.Text), err)
			continue
		}
		if len(got) != len(f.IDs) {
			t.Errorf("case %d (%q): %d tokens, reference has %d", i, trunc(f.Text), len(got), len(f.IDs))
			continue
		}
		for j, tk := range got {
			if tk.ID != f.IDs[j] {
				t.Errorf("case %d token %d: id %d, want %d", i, j, tk.ID, f.IDs[j])
			}
			if tk.Start != f.Offsets[j][0] || tk.End != f.Offsets[j][1] {
				t.Errorf("case %d token %d: span [%d,%d), want [%d,%d)",
					i, j, tk.Start, tk.End, f.Offsets[j][0], f.Offsets[j][1])
			}
		}
	}
}

// Offsets must tile the input exactly: contiguous, in order, ending at len(text).
// This holds regardless of the fixtures and catches the whole class of bug where
// a decoded token's length is computed wrongly.
func TestOffsetsTileTheInput(t *testing.T) {
	tok, err := Load(modelPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"hello world", "héllo 😀 wörld", "  spaced  out  ",
		`{"key":"sk-ant-abcdefghijklmnop"}`, "日本語",
	} {
		got, err := tok.Encode(text)
		if err != nil {
			t.Fatalf("Encode(%q): %v", text, err)
		}
		prev := 0
		for j, tk := range got {
			if tk.Start != prev {
				t.Errorf("%q token %d starts at %d, previous ended at %d", text, j, tk.Start, prev)
			}
			if tk.End < tk.Start || tk.End > len(text) {
				t.Errorf("%q token %d span [%d,%d) is out of range for %d bytes",
					text, j, tk.Start, tk.End, len(text))
			}
			prev = tk.End
		}
		if len(got) > 0 && prev != len(text) {
			t.Errorf("%q: offsets end at %d, want %d", text, prev, len(text))
		}
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
```

- [ ] **Step 4: Add the dependency**

```bash
go get github.com/dlclark/regexp2@latest
go mod tidy
grep regexp2 go.mod
```

- [ ] **Step 5: Implement the tokenizer**

Create `internal/privacy/tokenizer/tokenizer.go`. The shape is fixed; fill the
pretokenizer pattern from what Step 1 reported:

```go
// Package tokenizer is a byte-level BPE tokenizer that reads the model's own
// tokenizer.json and reports byte offsets alongside token ids.
//
// It exists because the NER detector needs to map the model's per-token
// predictions back to spans in the original string, and a tokenizer that
// disagrees with the reference by one character produces spans that redact the
// wrong bytes — silently, in the component whose whole job is trustworthiness.
// TestEncodeMatchesReferenceExactly is the gate that keeps it honest.
//
// The vocabulary is read from the file shipped with the weights rather than from
// a baked-in o200k table: the file is the source of truth, and a table that
// drifted from it would produce ids the model was never trained on.
package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dlclark/regexp2"
)

// Token is one token and the byte range of the input it covers.
type Token struct {
	ID         int
	Start, End int
}

// Tokenizer holds a byte-level BPE vocabulary and its merge ranks.
type Tokenizer struct {
	// vocab maps a token's byte-level-encoded string to its id.
	vocab map[string]int
	// ranks maps a merge pair to its priority; lower merges first.
	ranks map[[2]string]int
	// split is the pretokenizer: it chops input into pieces that BPE is applied
	// to independently. Uses regexp2 because the o200k pattern needs lookahead,
	// which Go's RE2 does not provide.
	split *regexp2.Regexp
	// byteEncoder maps a raw byte to its byte-level representation rune, the
	// GPT-2 convention every byte-level BPE vocabulary is written in.
	byteEncoder [256]string
}

// tokenizerFile is the subset of tokenizer.json this needs.
type tokenizerFile struct {
	PreTokenizer json.RawMessage `json:"pre_tokenizer"`
	Model        struct {
		Type   string         `json:"type"`
		Vocab  map[string]int `json:"vocab"`
		Merges []any          `json:"merges"`
	} `json:"model"`
}

// Load reads a tokenizer.json.
func Load(path string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: %w", err)
	}
	var f tokenizerFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("tokenizer: parse %s: %w", path, err)
	}
	if f.Model.Type != "BPE" {
		return nil, fmt.Errorf("tokenizer: model type %q is not supported; only byte-level BPE is", f.Model.Type)
	}

	t := &Tokenizer{vocab: f.Model.Vocab, ranks: map[[2]string]int{}}
	buildByteEncoder(&t.byteEncoder)

	// merges is either ["a b", ...] or [["a","b"], ...] depending on the version
	// of the tokenizers library that wrote the file. Both are accepted rather
	// than assuming one, because guessing wrong yields a tokenizer that loads and
	// then produces subtly wrong ids.
	for i, m := range f.Model.Merges {
		var a, b string
		switch v := m.(type) {
		case string:
			parts := strings.Split(v, " ")
			if len(parts) != 2 {
				return nil, fmt.Errorf("tokenizer: merge %d is not a pair: %q", i, v)
			}
			a, b = parts[0], parts[1]
		case []any:
			if len(v) != 2 {
				return nil, fmt.Errorf("tokenizer: merge %d is not a pair", i)
			}
			a, _ = v[0].(string)
			b, _ = v[1].(string)
		default:
			return nil, fmt.Errorf("tokenizer: merge %d has unexpected type %T", i, m)
		}
		t.ranks[[2]string{a, b}] = i
	}

	pattern, err := splitPattern(f.PreTokenizer)
	if err != nil {
		return nil, err
	}
	// regexp2's None option matches .NET default semantics, which is what the
	// o200k pattern was written against.
	re, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: compile pretokenizer: %w", err)
	}
	t.split = re
	return t, nil
}

// splitPattern extracts the pretokenizer's regex. Sequence pretokenizers are
// walked for the first Split stage, which is where the pattern lives in every
// o200k-family file; anything else is an error rather than a guess.
func splitPattern(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("tokenizer: no pre_tokenizer in the file")
	}
	var node struct {
		Type          string            `json:"type"`
		Pattern       struct{ Regex string } `json:"pattern"`
		PreTokenizers []json.RawMessage `json:"pretokenizers"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return "", fmt.Errorf("tokenizer: parse pre_tokenizer: %w", err)
	}
	switch node.Type {
	case "Split":
		if node.Pattern.Regex == "" {
			return "", fmt.Errorf("tokenizer: Split pretokenizer has no regex")
		}
		return node.Pattern.Regex, nil
	case "Sequence":
		for _, sub := range node.PreTokenizers {
			if p, err := splitPattern(sub); err == nil {
				return p, nil
			}
		}
		return "", fmt.Errorf("tokenizer: no Split stage in the pretokenizer sequence")
	default:
		return "", fmt.Errorf("tokenizer: pretokenizer type %q is not supported", node.Type)
	}
}

// buildByteEncoder is the GPT-2 byte-to-unicode map every byte-level BPE
// vocabulary is written in: printable ASCII maps to itself, and the remaining
// bytes map into a private range so the vocabulary is valid UTF-8 text.
func buildByteEncoder(out *[256]string) {
	var bs []int
	for i := '!'; i <= '~'; i++ {
		bs = append(bs, int(i))
	}
	for i := '¡'; i <= '¬'; i++ {
		bs = append(bs, int(i))
	}
	for i := '®'; i <= 'ÿ'; i++ {
		bs = append(bs, int(i))
	}
	inSet := map[int]bool{}
	for _, b := range bs {
		inSet[b] = true
	}
	next := 0
	for b := 0; b < 256; b++ {
		if inSet[b] {
			out[b] = string(rune(b))
			continue
		}
		out[b] = string(rune(256 + next))
		next++
	}
}

// Encode tokenizes s and reports each token's byte span.
//
// Offsets come out of the pretokenizer's own match positions plus the byte
// lengths of the pieces BPE produces — never from re-decoding and searching,
// which is where offset bugs come from.
func (t *Tokenizer) Encode(s string) ([]Token, error) {
	if s == "" {
		return nil, nil
	}
	var out []Token
	m, err := t.split.FindStringMatch(s)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: pretokenize: %w", err)
	}
	for m != nil {
		// regexp2 reports rune indices; convert to bytes against the original
		// string so spans are byte offsets throughout.
		runes := []rune(s)
		start := len(string(runes[:m.Index]))
		piece := m.String()

		for _, part := range t.bpe(piece) {
			end := start + len(part)
			id, ok := t.vocab[t.encodeBytes(part)]
			if !ok {
				return nil, fmt.Errorf("tokenizer: piece %q is not in the vocabulary", part)
			}
			out = append(out, Token{ID: id, Start: start, End: end})
			start = end
		}
		if m, err = t.split.FindNextMatch(m); err != nil {
			return nil, fmt.Errorf("tokenizer: pretokenize: %w", err)
		}
	}
	return out, nil
}

// encodeBytes converts raw bytes to the byte-level representation the vocabulary
// is keyed on.
func (t *Tokenizer) encodeBytes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteString(t.byteEncoder[s[i]])
	}
	return b.String()
}

// bpe applies merges to one pretokenized piece and returns the resulting parts,
// as raw byte substrings of the input.
func (t *Tokenizer) bpe(piece string) []string {
	if len(piece) <= 1 {
		return []string{piece}
	}
	// Start from single bytes, so every part is a byte substring and offsets are
	// exact by construction.
	parts := make([]string, 0, len(piece))
	for i := 0; i < len(piece); i++ {
		parts = append(parts, piece[i:i+1])
	}
	for len(parts) > 1 {
		bestRank, bestAt := -1, -1
		for i := 0; i+1 < len(parts); i++ {
			key := [2]string{t.encodeBytes(parts[i]), t.encodeBytes(parts[i+1])}
			rank, ok := t.ranks[key]
			if !ok {
				continue
			}
			if bestAt < 0 || rank < bestRank {
				bestRank, bestAt = rank, i
			}
		}
		if bestAt < 0 {
			break
		}
		merged := parts[bestAt] + parts[bestAt+1]
		parts = append(parts[:bestAt], append([]string{merged}, parts[bestAt+2:]...)...)
	}
	return parts
}

// sortedVocab is a diagnostic used when the gate fails: it prints the vocabulary
// entries closest to an unmatched piece, which is almost always a byte-encoding
// mistake rather than a missing entry.
func (t *Tokenizer) sortedVocab(prefix string, n int) []string {
	var out []string
	for k := range t.vocab {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}
```

- [ ] **Step 6: Run the gate**

```bash
AIPROXY_TOKENIZER_JSON=/tmp/pf/tokenizer.json go test ./internal/privacy/tokenizer -race -v
```
Expected: PASS on both tests, for every fixture.

**If ids match but offsets do not**, the bug is in the rune-to-byte conversion in
`Encode` — `m.Index` is a rune index and the conversion above rebuilds `[]rune(s)`
per match, which is correct but O(n²); fix correctness first, then hoist the
conversion out of the loop.

**If ids do not match**, the likely causes in order are: the pretokenizer pattern
was extracted from the wrong stage; `byteEncoder` is wrong (compare against a
known o200k vocabulary entry for a space, `Ġ`); or the merge list parsed in the
wrong format. `sortedVocab` is there to help.

**If it cannot be made to pass**, stop. Report what disagrees, and ship Part 1
alone. Do not proceed to Task 17 with approximate offsets.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/privacy/tokenizer scripts/gen-tokenizer-fixtures.py
git commit -m "feat(privacy): byte-level BPE tokenizer with reference-exact offsets

Records the inspected tokenizer.json shape: <paste Step 1 output here>."
```

---

### Task 17: Vendored ONNX Runtime binding and verified assets

**Files:**
- Create: `internal/privacy/onnxrt/` (vendored), `internal/privacy/onnxrt/VENDOR.md`
- Create: `internal/privacy/ner/assets.go`, `internal/privacy/ner/assets_test.go`

**Interfaces:**
- Consumes: nothing from Part 1.
- Produces:
  - `type Asset struct { Name, URL, SHA256 string; Bytes int64 }`
  - `func Assets(goos, goarch string) ([]Asset, error)`
  - `func Dir() string` — `~/.config/aiproxy/models/privacy-filter`
  - `func Ensure(ctx context.Context, dir string, assets []Asset, progress func(name string, done, total int64)) error`
  - `func Present(dir string, assets []Asset) bool`

`Ensure` is the same download → verify → rename shape `internal/updater.Apply`
already uses, for the same reasons: nothing unverified is ever loaded, and a
partial download can never be mistaken for a complete one. It is a separate
implementation rather than a shared one because the updater's version comparison
and exec-path resolution have nothing to do with model files; what is shared is
the *sequence*, and that is worth restating in a comment rather than abstracting.

- [ ] **Step 1: Pin the revision and record the digests**

Both URLs must point at an immutable revision, so a digest mismatch always means
the download broke rather than that upstream moved a file.

```bash
# The model repo's current commit. Record it; it goes into the URL.
curl -fsSL https://huggingface.co/api/models/openai/privacy-filter | \
    python3 -c 'import json,sys; print(json.load(sys.stdin)["sha"])'

# Download once and record digests and sizes for the constants below.
REV=<the sha printed above>
mkdir -p /tmp/pf && cd /tmp/pf
for f in tokenizer.json config.json viterbi_calibration.json; do
  curl -fsSL -o "$f" "https://huggingface.co/openai/privacy-filter/resolve/$REV/$f"
done
curl -fsSL -o model_q4f16.onnx "https://huggingface.co/openai/privacy-filter/resolve/$REV/onnx/model_q4f16.onnx"
# The .onnx_data sidecar holds the weights for an external-data export; check
# whether the repo has one for this variant and fetch it too if so.
curl -fsSLI "https://huggingface.co/openai/privacy-filter/resolve/$REV/onnx/model_q4f16.onnx_data" | head -1
shasum -a 256 * && ls -l
```

Also pin an ONNX Runtime release. Record the version and the per-platform asset
digests:

```bash
gh release view --repo microsoft/onnxruntime --json tagName,assets \
  --jq '.tagName, (.assets[] | select(.name|test("osx|linux")) | .name)'
```

- [ ] **Step 2: Vendor the binding**

```bash
mkdir -p internal/privacy/onnxrt
git clone --depth 1 https://github.com/shota3506/onnxruntime-purego /tmp/ortpg
cp /tmp/ortpg/onnxruntime/*.go internal/privacy/onnxrt/
cp /tmp/ortpg/LICENSE internal/privacy/onnxrt/LICENSE
# Rewrite the package clause and drop anything not needed for a token classifier.
sed -i '' 's/^package onnxruntime$/package onnxrt/' internal/privacy/onnxrt/*.go
rm -f internal/privacy/onnxrt/*_test.go
go build ./internal/privacy/onnxrt
```

Create `internal/privacy/onnxrt/VENDOR.md`:

```markdown
# Vendored: onnxruntime-purego

Source: https://github.com/shota3506/onnxruntime-purego
Commit: <record the cloned commit>
License: MIT (see LICENSE)

Vendored rather than imported because upstream states its API may change without
notice, and this is load-bearing for a privacy filter: an unannounced signature
change should be a merge conflict we resolve deliberately, not a build break on
someone else's schedule.

It reaches ONNX Runtime through purego, so `CGO_ENABLED=0` still holds — which is
what keeps the release workflow cross-compiling four targets from one runner, and
keeps install.sh and the self-updater working.

## Local changes
- Package renamed to `onnxrt`.
- Tests and GenAI support removed; only session creation and Run are used.
- <record any further patch here, with the reason>
```

- [ ] **Step 3: Write the failing test**

Create `internal/privacy/ner/assets_test.go`:

```go
package ner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestEnsureDownloadsAndVerifies(t *testing.T) {
	body := []byte("pretend this is a model")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{Name: "model.onnx", URL: srv.URL + "/model.onnx", SHA256: digest(body)}}
	if Present(dir, assets) {
		t.Fatal("Present reported assets before any download")
	}
	if err := Ensure(context.Background(), dir, assets, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "model.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("content = %q", got)
	}
	if !Present(dir, assets) {
		t.Error("Present is false after a successful download")
	}
}

// A wrong digest must leave nothing behind that could later be loaded.
func TestEnsureRefusesAMismatchedDigestAndLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tampered"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{Name: "model.onnx", URL: srv.URL + "/m", SHA256: digest([]byte("expected"))}}
	err := Ensure(context.Background(), dir, assets, nil)
	if err == nil {
		t.Fatal("Ensure accepted a mismatched digest")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error should name the digest mismatch: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("left behind %s; nothing unverified may remain where it could be loaded", e.Name())
	}
}

// An already-present, correct asset is not re-downloaded — 800MB is not
// something to fetch twice.
func TestEnsureSkipsAVerifiedAsset(t *testing.T) {
	body := []byte("already here")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{Name: "m.onnx", URL: srv.URL + "/m", SHA256: digest(body)}}
	if err := Ensure(context.Background(), dir, assets, nil); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(context.Background(), dir, assets, nil); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("downloaded %d times, want 1", hits)
	}
}

func TestEnsureReportsProgress(t *testing.T) {
	body := make([]byte, 1<<16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "65536")
		w.Write(body)
	}))
	defer srv.Close()

	var lastDone int64
	err := Ensure(context.Background(), t.TempDir(),
		[]Asset{{Name: "m", URL: srv.URL + "/m", SHA256: digest(body), Bytes: int64(len(body))}},
		func(_ string, done, _ int64) { lastDone = done })
	if err != nil {
		t.Fatal(err)
	}
	if lastDone != int64(len(body)) {
		t.Errorf("final progress = %d, want %d", lastDone, len(body))
	}
}

func TestAssetsRejectAnUnsupportedPlatform(t *testing.T) {
	if _, err := Assets("windows", "amd64"); err == nil {
		t.Error("windows must be rejected; the release pipeline ships darwin and linux only")
	}
	for _, p := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}} {
		got, err := Assets(p[0], p[1])
		if err != nil {
			t.Errorf("Assets(%q,%q): %v", p[0], p[1], err)
		}
		if len(got) == 0 {
			t.Errorf("Assets(%q,%q) returned nothing", p[0], p[1])
		}
		for _, a := range got {
			if a.SHA256 == "" || a.URL == "" || a.Name == "" {
				t.Errorf("incomplete asset: %+v", a)
			}
		}
	}
}
```

- [ ] **Step 4: Implement**

Create `internal/privacy/ner/assets.go`:

```go
// Package ner is the model tier of the privacy filter: openai/privacy-filter run
// in-process through a vendored purego binding to ONNX Runtime.
package ner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
)

// modelRevision pins the Hugging Face commit every asset URL resolves against.
// Immutable by construction, so a digest mismatch always means the download
// broke — never that upstream replaced a file under us.
const modelRevision = "<record from Task 17 Step 1>"

// ortVersion pins the ONNX Runtime release. A native library is being loaded into
// this process; a segfault in it is not recoverable in Go, so the version is
// pinned to one that has been exercised rather than tracking latest.
const ortVersion = "<record from Task 17 Step 1>"

// Asset is one file to fetch and verify.
type Asset struct {
	Name   string
	URL    string
	SHA256 string
	// Bytes is the expected size, used only for progress reporting.
	Bytes int64
}

// Dir is where assets live: beside the config, not in the binary. Neither the
// runtime library nor the ~800MB of weights ships in the release tarball, which
// is what keeps it around 13MB.
func Dir() string { return filepath.Join(config.Dir(), "models", "privacy-filter") }

// Assets is the fetch list for one platform.
func Assets(goos, goarch string) ([]Asset, error) {
	hf := func(path string) string {
		return "https://huggingface.co/openai/privacy-filter/resolve/" + modelRevision + "/" + path
	}
	out := []Asset{
		{Name: "tokenizer.json", URL: hf("tokenizer.json"), SHA256: "<record>"},
		{Name: "config.json", URL: hf("config.json"), SHA256: "<record>"},
		{Name: "viterbi_calibration.json", URL: hf("viterbi_calibration.json"), SHA256: "<record>"},
		{Name: "model_q4f16.onnx", URL: hf("onnx/model_q4f16.onnx"), SHA256: "<record>", Bytes: 0},
	}
	// The external-data sidecar, if the q4f16 export has one (Task 17 Step 1
	// establishes this with a HEAD request).
	out = append(out, Asset{
		Name: "model_q4f16.onnx_data", URL: hf("onnx/model_q4f16.onnx_data"), SHA256: "<record>", Bytes: 0,
	})

	lib, err := runtimeAsset(goos, goarch)
	if err != nil {
		return nil, err
	}
	return append(out, lib), nil
}

// runtimeAsset is the ONNX Runtime shared library for a platform. Windows is
// absent deliberately: the release pipeline ships darwin and linux only, and
// replacing a running binary by rename — which the updater relies on — is not
// possible there anyway.
func runtimeAsset(goos, goarch string) (Asset, error) {
	key := goos + "/" + goarch
	switch key {
	case "darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64":
	default:
		return Asset{}, fmt.Errorf("ner: no ONNX Runtime build for %s", key)
	}
	// One entry per platform, each with its own digest. Filled from Task 17
	// Step 1.
	table := map[string]Asset{
		"darwin/arm64": {Name: "libonnxruntime.dylib", URL: "<record>", SHA256: "<record>"},
		"darwin/amd64": {Name: "libonnxruntime.dylib", URL: "<record>", SHA256: "<record>"},
		"linux/amd64":  {Name: "libonnxruntime.so", URL: "<record>", SHA256: "<record>"},
		"linux/arm64":  {Name: "libonnxruntime.so", URL: "<record>", SHA256: "<record>"},
	}
	return table[key], nil
}

// Present reports whether every asset is on disk with the right digest.
func Present(dir string, assets []Asset) bool {
	for _, a := range assets {
		if !verified(filepath.Join(dir, a.Name), a.SHA256) {
			return false
		}
	}
	return true
}

// Ensure downloads whatever is missing or wrong, verifying each file before it is
// put where it could be loaded.
//
// Same sequence as internal/updater.Apply, and for the same reasons: download to
// a temp file IN THE TARGET DIRECTORY so the rename is atomic on one filesystem,
// verify, then rename. Nothing unverified is ever moved into place, and a partial
// download cannot be mistaken for a complete one.
func Ensure(ctx context.Context, dir string, assets []Asset, progress func(name string, done, total int64)) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ner: create %s: %w", dir, err)
	}
	client := &http.Client{Timeout: 2 * time.Hour} // 800MB on a slow link
	for _, a := range assets {
		final := filepath.Join(dir, a.Name)
		if verified(final, a.SHA256) {
			continue
		}
		if err := fetch(ctx, client, dir, final, a, progress); err != nil {
			return err
		}
	}
	return nil
}

func fetch(ctx context.Context, client *http.Client, dir, final string, a Asset,
	progress func(string, int64, int64)) error {

	tmp, err := os.CreateTemp(dir, "."+a.Name+"-*")
	if err != nil {
		return fmt.Errorf("ner: %w", err)
	}
	defer os.Remove(tmp.Name())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("ner: fetch %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("ner: fetch %s: status %d", a.Name, resp.StatusCode)
	}

	total := a.Bytes
	if total == 0 {
		total = resp.ContentLength
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				return werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(a.Name, done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return fmt.Errorf("ner: download %s: %w", a.Name, rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return fmt.Errorf("ner: %s failed verification: expected sha256 %s, got %s",
			a.Name, a.SHA256, got)
	}
	return os.Rename(tmp.Name(), final)
}

func verified(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}
```

- [ ] **Step 5: Fill in every recorded value and prove none was missed**

The `<record>` markers above are outputs of Step 1, not decisions deferred to
later. Replace each with the real revision, URL, digest, and size, then verify
none survived:

```bash
! grep -rn '<record>' internal/privacy/ner/ || { echo "unfilled markers remain"; exit 1; }
```

That grep is the gate on this step: a `<record>` left in place compiles fine and
then fails verification at runtime, which is exactly the sort of thing to catch
here rather than on an operator's machine.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/privacy/ner -race -v && go build ./...`
Expected: PASS, including `TestAssetsRejectAnUnsupportedPlatform`.

- [ ] **Step 7: Commit**

```bash
git add internal/privacy/onnxrt internal/privacy/ner
git commit -m "feat(privacy): vendor the ONNX Runtime binding, fetch verified model assets"
```

---

### Task 18: The NER detector

**Files:**
- Create: `internal/privacy/ner/ner.go`, `internal/privacy/ner/viterbi.go`, `internal/privacy/ner/labels.go`
- Test: `internal/privacy/ner/viterbi_test.go`, `internal/privacy/ner/ner_test.go`

**Interfaces:**
- Consumes: `tokenizer.Load`, `tokenizer.Token` (Task 16); `Assets`, `Dir`, `Present`, `Ensure` (Task 17); `privacy.Detector`, `privacy.Finding`, `privacy.Label`, `privacy.MinScanBytes` (Part 1).
- Produces:
  - `func LoadLabels(configPath string) ([]string, error)` — `id2label` in model order
  - `type Span struct { Start, End int; Label string; Score float64 }`
  - `func Decode(logits [][]float32, labels []string, trans [][]float32) []Span` — BIOES-constrained Viterbi over token indices
  - `type Detector struct{ ... }`, `func New(o Options) (*Detector, error)`
  - `func (d *Detector) Name() string` → `"ner"`, `Scan`, `ModelState() string`

**Label mapping.** The model's `config.json` carries 33 labels — `O` plus 8
categories × BIOES. They map onto placeholder labels: `secret`→`SECRET`,
`private_email`→`EMAIL`, `private_phone`→`PHONE`, `private_address`→`ADDRESS`,
`private_person`→`PERSON`, `private_url`→`URL`, `private_date`→`DATE`,
`account_number`→`ACCOUNT`. Only categories listed in `privacy.ner.labels` produce
findings, and that list is empty by default.

- [ ] **Step 1: Establish the calibration file's schema**

```bash
python3 -c "import json;d=json.load(open('/tmp/pf/viterbi_calibration.json'));print(type(d));print(list(d)[:20] if isinstance(d,dict) else d[:5])"
python3 -c "import json;d=json.load(open('/tmp/pf/config.json'));print(json.dumps(d['id2label'],indent=0)[:800])"
```

Record both in the commit message. The calibration is expected to hold either a
33×33 transition matrix or a set of allowed-transition constraints. **If it holds
a matrix**, use it as the transition scores. **If it holds constraints only**,
build the matrix from BIOES legality: `B-x` may be followed only by `I-x` or
`E-x`; `I-x` only by `I-x` or `E-x`; `S-x`, `E-x`, and `O` only by `B-y`, `S-y`,
or `O`. Everything else is `-inf`. Either way the decode below is unchanged; only
where `trans` comes from differs.

- [ ] **Step 2: Write the Viterbi test**

Create `internal/privacy/ner/viterbi_test.go`:

```go
package ner

import "testing"

// tinyLabels is a two-category BIOES set, small enough to reason about by hand.
var tinyLabels = []string{
	"O",
	"B-secret", "I-secret", "E-secret", "S-secret",
	"B-private_email", "I-private_email", "E-private_email", "S-private_email",
}

// allow builds a transition matrix from BIOES legality, which is the fallback
// when the calibration file carries constraints rather than scores.
func allow(labels []string) [][]float32 {
	n := len(labels)
	out := make([][]float32, n)
	for i := range out {
		out[i] = make([]float32, n)
		for j := range out[i] {
			if legalTransition(labels[i], labels[j]) {
				out[i][j] = 0
			} else {
				out[i][j] = negInf
			}
		}
	}
	return out
}

func TestDecodeFindsASingleTokenSpan(t *testing.T) {
	// Three tokens: O, S-secret, O.
	logits := [][]float32{
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 5, 0, 0, 0, 0},
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	if len(got) != 1 {
		t.Fatalf("got %d spans, want 1: %+v", len(got), got)
	}
	if got[0].Start != 1 || got[0].End != 2 || got[0].Label != "secret" {
		t.Errorf("span = %+v", got[0])
	}
}

func TestDecodeFindsAMultiTokenSpan(t *testing.T) {
	// O, B-secret, I-secret, E-secret, O.
	logits := [][]float32{
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 5, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 5, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 5, 0, 0, 0, 0, 0},
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	if len(got) != 1 {
		t.Fatalf("got %d spans, want 1: %+v", len(got), got)
	}
	if got[0].Start != 1 || got[0].End != 4 {
		t.Errorf("span = %+v, want tokens [1,4)", got[0])
	}
}

// The constraint is the point of a constrained decode: a B not followed by I or E
// is not a valid tagging, and the decoder must find the best VALID path rather
// than the best per-token argmax.
func TestDecodeRejectsAnIllegalPath(t *testing.T) {
	// Per-token argmax would be B-secret then O, which BIOES forbids.
	logits := [][]float32{
		{0, 5, 0, 0, 4, 0, 0, 0, 0},
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	for _, s := range got {
		if s.Start == 0 && s.End == 1 && s.Label == "secret" {
			// S-secret at token 0 is legal and is the right answer; B-secret
			// alone would not be.
			return
		}
	}
	if len(got) != 0 {
		t.Fatalf("decoded an illegal tagging: %+v", got)
	}
}

func TestDecodeFindsTwoAdjacentSpans(t *testing.T) {
	logits := [][]float32{
		{0, 0, 0, 0, 5, 0, 0, 0, 0}, // S-secret
		{0, 0, 0, 0, 0, 0, 0, 0, 5}, // S-private_email
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	if len(got) != 2 {
		t.Fatalf("got %d spans, want 2: %+v", len(got), got)
	}
	if got[0].Label != "secret" || got[1].Label != "private_email" {
		t.Errorf("spans = %+v", got)
	}
}

func TestDecodeOnEmptyInput(t *testing.T) {
	if got := Decode(nil, tinyLabels, allow(tinyLabels)); len(got) != 0 {
		t.Errorf("Decode(nil) = %+v", got)
	}
}

func TestLegalTransition(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"O", "B-secret", true},
		{"O", "S-secret", true},
		{"O", "I-secret", false},
		{"O", "E-secret", false},
		{"B-secret", "I-secret", true},
		{"B-secret", "E-secret", true},
		{"B-secret", "O", false},
		{"B-secret", "I-private_email", false},
		{"I-secret", "E-secret", true},
		{"I-secret", "B-secret", false},
		{"E-secret", "O", true},
		{"E-secret", "B-private_email", true},
		{"S-secret", "O", true},
	} {
		if got := legalTransition(c.from, c.to); got != c.want {
			t.Errorf("legalTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}
```

- [ ] **Step 3: Implement labels and Viterbi**

Create `internal/privacy/ner/labels.go`:

```go
package ner

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/nicko170/aiproxy/internal/privacy"
)

// categoryLabels maps the model's category names to placeholder labels.
//
// Only categories the operator enabled are ever consulted, and the default set is
// empty. private_url and private_date are the two worth being deliberate about:
// in source code, import URLs, API endpoints, doc links, changelog dates and
// licence years are everywhere, so enabling them corrupts the agent's context for
// very little privacy gain.
var categoryLabels = map[string]privacy.Label{
	"secret":         privacy.LabelSecret,
	"private_email":  privacy.LabelEmail,
	"private_phone":  privacy.LabelPhone,
	"private_address": privacy.LabelAddress,
	"private_person": privacy.LabelPerson,
	"private_url":    privacy.LabelURL,
	"private_date":   privacy.LabelDate,
	"account_number": privacy.LabelAccount,
}

// LoadLabels reads id2label from the model's config.json, ordered by id, so index
// i of the returned slice is the label for logit column i.
func LoadLabels(configPath string) ([]string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("ner: %w", err)
	}
	var cfg struct {
		ID2Label map[string]string `json:"id2label"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("ner: parse %s: %w", configPath, err)
	}
	if len(cfg.ID2Label) == 0 {
		return nil, fmt.Errorf("ner: %s has no id2label", configPath)
	}
	ids := make([]int, 0, len(cfg.ID2Label))
	for k := range cfg.ID2Label {
		id, err := strconv.Atoi(k)
		if err != nil {
			return nil, fmt.Errorf("ner: id2label key %q is not an integer", k)
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]string, len(ids))
	for i, id := range ids {
		if id != i {
			return nil, fmt.Errorf("ner: id2label is not contiguous from 0 (saw %d at position %d)", id, i)
		}
		out[i] = cfg.ID2Label[strconv.Itoa(id)]
	}
	return out, nil
}

// splitTag divides "B-secret" into its BIOES prefix and category. "O" has no
// category.
func splitTag(tag string) (prefix, category string) {
	if tag == "O" {
		return "O", ""
	}
	if i := strings.IndexByte(tag, '-'); i > 0 {
		return tag[:i], tag[i+1:]
	}
	return tag, ""
}

// legalTransition encodes BIOES: a span opened with B must continue with I or end
// with E, and only E, S, or O may precede a new span. Without this, the best
// per-token guesses routinely form taggings that do not describe any set of
// spans at all.
func legalTransition(from, to string) bool {
	fp, fc := splitTag(from)
	tp, tc := splitTag(to)
	switch fp {
	case "O", "E", "S":
		return tp == "O" || tp == "B" || tp == "S"
	case "B", "I":
		return (tp == "I" || tp == "E") && tc == fc
	}
	return false
}
```

Create `internal/privacy/ner/viterbi.go`:

```go
package ner

import "math"

// negInf marks a forbidden transition. A large negative float32 rather than an
// actual infinity so that arithmetic on it stays finite and comparable.
const negInf = float32(-1e30)

// Span is a decoded entity over TOKEN indices; the caller maps them to byte
// offsets using the tokenizer's spans.
type Span struct {
	Start, End int // token indices, half-open
	Label      string
	Score      float64
}

// Decode runs a constrained Viterbi over per-token logits and returns the entity
// spans of the best legal path.
//
// Constrained is the operative word: taking each token's argmax independently
// produces taggings BIOES does not permit — a B with nothing after it, an I of one
// category following a B of another — which then decode into spans that do not
// exist. The transition matrix makes those paths unreachable rather than
// something to clean up afterwards.
//
// logits is [token][label]; trans is [from][to]; labels[i] names column i.
func Decode(logits [][]float32, labels []string, trans [][]float32) []Span {
	n := len(logits)
	if n == 0 || len(labels) == 0 {
		return nil
	}
	L := len(labels)

	score := make([][]float32, n)
	back := make([][]int, n)
	for i := range score {
		score[i] = make([]float32, L)
		back[i] = make([]int, L)
	}

	// A sequence may only open with O, B, or S.
	for j := 0; j < L; j++ {
		prefix, _ := splitTag(labels[j])
		if prefix == "I" || prefix == "E" {
			score[0][j] = negInf
			continue
		}
		score[0][j] = logits[0][j]
	}

	for i := 1; i < n; i++ {
		for j := 0; j < L; j++ {
			best, bestFrom := negInf, 0
			for k := 0; k < L; k++ {
				if score[i-1][k] <= negInf || trans[k][j] <= negInf {
					continue
				}
				if v := score[i-1][k] + trans[k][j]; v > best {
					best, bestFrom = v, k
				}
			}
			if best <= negInf {
				score[i][j] = negInf
				continue
			}
			score[i][j] = best + logits[i][j]
			back[i][j] = bestFrom
		}
	}

	// A sequence may only close on O, E, or S.
	bestLast, bestScore := -1, negInf
	for j := 0; j < L; j++ {
		prefix, _ := splitTag(labels[j])
		if prefix == "B" || prefix == "I" {
			continue
		}
		if score[n-1][j] > bestScore {
			bestScore, bestLast = score[n-1][j], j
		}
	}
	if bestLast < 0 {
		return nil
	}

	path := make([]int, n)
	path[n-1] = bestLast
	for i := n - 1; i > 0; i-- {
		path[i-1] = back[i][path[i]]
	}
	return spansFromPath(path, labels, logits)
}

// spansFromPath turns a tag path into spans. B..E and S both open and close a
// span; anything else is outside one.
func spansFromPath(path []int, labels []string, logits [][]float32) []Span {
	var out []Span
	start, category := -1, ""
	var sum float64
	for i, id := range path {
		prefix, cat := splitTag(labels[id])
		switch prefix {
		case "S":
			out = append(out, Span{Start: i, End: i + 1, Label: cat,
				Score: softmaxAt(logits[i], id)})
			start, category, sum = -1, "", 0
		case "B":
			start, category = i, cat
			sum = softmaxAt(logits[i], id)
		case "I":
			if start >= 0 {
				sum += softmaxAt(logits[i], id)
			}
		case "E":
			if start >= 0 {
				sum += softmaxAt(logits[i], id)
				out = append(out, Span{Start: start, End: i + 1, Label: category,
					Score: sum / float64(i+1-start)})
			}
			start, category, sum = -1, "", 0
		default: // O
			start, category, sum = -1, "", 0
		}
	}
	return out
}

// softmaxAt is the probability of column j, so Span.Score is a confidence rather
// than an unbounded logit. Nothing filters on it today; it exists so a threshold
// is a config change rather than an interface change.
func softmaxAt(row []float32, j int) float64 {
	max := row[0]
	for _, v := range row {
		if v > max {
			max = v
		}
	}
	var sum float64
	for _, v := range row {
		sum += math.Exp(float64(v - max))
	}
	if sum == 0 {
		return 0
	}
	return math.Exp(float64(row[j]-max)) / sum
}
```

- [ ] **Step 4: Run the Viterbi tests**

Run: `go test ./internal/privacy/ner -race -v -run 'TestDecode|TestLegalTransition'`
Expected: PASS. These need no model, no ONNX Runtime, and no download — the
decode is pure arithmetic and is tested as such.

- [ ] **Step 5: Implement the detector**

Create `internal/privacy/ner/ner.go` with:

```go
// Options configures the detector.
type Options struct {
	Dir          string   // where Assets were fetched
	Labels       []string // enabled categories; empty means the detector finds nothing
	MaxScanBytes int
	Log          *slog.Logger
}

// Detector is the model tier. It implements privacy.Detector, so the pipeline
// cannot tell it from the rule table.
//
// The session and the tokenizer are built ONCE, lazily, on the first scan that
// could produce a finding — so a proxy with the model configured but never
// exercised does not dlopen a native library or read 800MB from disk. loadOnce
// carries the outcome, including the failure, so a broken install is reported on
// every scan rather than retried on every scan.
type Detector struct {
	opts     Options
	loadOnce sync.Once
	loadErr  error
	tok      *tokenizer.Tokenizer
	labels   []string
	trans    [][]float32
	session  *onnxrt.Session
	enabled  map[string]privacy.Label
	state    atomic.Value // string: absent, loading, ready, error
}
```

All of `Scan` except the tensor call is ordinary code, and the tensor call is
isolated behind a one-method seam so the vendored binding's exact signatures live
in a single function — which also makes the detector testable with no model
present:

```go
// runner is the only thing Scan needs from ONNX Runtime: given a token window,
// return per-token logits. Keeping it an interface confines the vendored
// binding's API surface to newRunner, so a signature change upstream is a
// one-function fix rather than a rewrite — and lets the chunking, mapping, and
// de-duplication below be tested against a fake.
type runner interface {
	Run(inputIDs, attnMask []int64) ([][]float32, error)
	Close() error
}

// newRunner creates the session. This is the ONLY binding-dependent code in the
// package; match it to the signatures recorded in internal/privacy/onnxrt/VENDOR.md.
// The model has two inputs, input_ids and attention_mask, and one output of shape
// [1, seq, len(labels)].
func newRunner(modelPath string, nLabels int) (runner, error) { /* onnxrt.NewSession(...) */ }

// window is how many tokens go to the model at once, and overlap is how much two
// consecutive windows share.
//
// The model accepts 128K tokens, but a window that large would put a
// multi-second inference in front of a request. A quarter-window overlap means an
// entity straddling a boundary is seen whole inside at least one window, so
// nothing is missed at the seam; the duplicate findings that produces are removed
// by the (start, end, label) de-duplication below.
const (
	window  = 2048
	overlap = window / 4
)

func (d *Detector) Name() string { return "ner" }

func (d *Detector) Scan(ctx context.Context, text string) ([]privacy.Finding, error) {
	// No enabled labels means no work, and — importantly — no model load. A proxy
	// configured with the model but no labels never dlopens a native library.
	if len(d.enabled) == 0 || len(text) < privacy.MinScanBytes {
		return nil, nil
	}
	if err := d.load(); err != nil {
		return nil, err
	}

	scanned := text
	truncated := false
	if d.opts.MaxScanBytes > 0 && len(scanned) > d.opts.MaxScanBytes {
		// Cut on a rune boundary so the tokenizer is never handed a partial
		// character.
		cut := d.opts.MaxScanBytes
		for cut > 0 && !utf8.RuneStart(scanned[cut]) {
			cut--
		}
		scanned, truncated = scanned[:cut], true
	}

	toks, err := d.tok.Encode(scanned)
	if err != nil {
		return nil, fmt.Errorf("ner: tokenize: %w", err)
	}
	if len(toks) == 0 {
		return nil, nil
	}

	seen := map[[3]any]bool{}
	var out []privacy.Finding
	for base := 0; base < len(toks); base += window - overlap {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := base + window
		if end > len(toks) {
			end = len(toks)
		}
		chunk := toks[base:end]

		ids := make([]int64, len(chunk))
		mask := make([]int64, len(chunk))
		for i, tk := range chunk {
			ids[i], mask[i] = int64(tk.ID), 1
		}
		logits, err := d.session.Run(ids, mask)
		if err != nil {
			return nil, fmt.Errorf("ner: inference: %w", err)
		}

		for _, sp := range Decode(logits, d.labels, d.trans) {
			label, enabled := d.enabled[sp.Label]
			if !enabled {
				continue
			}
			if sp.Start < 0 || sp.End > len(chunk) || sp.End <= sp.Start {
				continue // a decode that disagrees with the window is not usable
			}
			// Token indices become byte offsets through the tokenizer's own
			// spans, never by re-decoding and searching — that is where offset
			// bugs come from.
			f := privacy.Finding{
				Start: chunk[sp.Start].Start, End: chunk[sp.End-1].End,
				Label: label, Rule: "ner:" + sp.Label, Confidence: sp.Score,
			}
			key := [3]any{f.Start, f.End, f.Label}
			if seen[key] {
				continue // the overlap region reported it twice
			}
			seen[key] = true
			out = append(out, f)
		}
		if end == len(toks) {
			break
		}
	}
	if truncated && d.opts.Log != nil {
		// Never silent: a truncated scan is a miss, and a miss the operator does
		// not know about is the failure this whole component exists to avoid.
		d.opts.Log.Warn("privacy: input truncated before scanning",
			"bytes", len(text), "scanned", len(scanned), "limit", d.opts.MaxScanBytes)
	}
	return out, nil
}

// load builds the tokenizer, labels, transitions, and session exactly once.
//
// The outcome is cached INCLUDING the failure: a broken or absent install is
// reported on every scan rather than retried on every scan, which would turn one
// missing file into a load attempt per request. Every failure wraps
// privacy.ErrModelUnavailable so the control path can answer 503 and name the fix.
func (d *Detector) load() error {
	d.loadOnce.Do(func() {
		d.state.Store("loading")
		defer func() {
			if d.loadErr != nil {
				d.state.Store("error")
			} else {
				d.state.Store("ready")
			}
		}()

		tok, err := tokenizer.Load(filepath.Join(d.opts.Dir, "tokenizer.json"))
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		labels, err := LoadLabels(filepath.Join(d.opts.Dir, "config.json"))
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		trans, err := loadTransitions(filepath.Join(d.opts.Dir, "viterbi_calibration.json"), labels)
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		session, err := newRunner(filepath.Join(d.opts.Dir, "model_q4f16.onnx"), len(labels))
		if err != nil {
			d.loadErr = fmt.Errorf("%w: %v", privacy.ErrModelUnavailable, err)
			return
		}
		d.tok, d.labels, d.trans, d.session = tok, labels, trans, session
	})
	return d.loadErr
}

// ModelState is what view.Status.Privacy reports: off, absent, loading, ready, or
// error.
func (d *Detector) ModelState() string {
	v, _ := d.state.Load().(string)
	if v == "" {
		return "absent"
	}
	return v
}
```

`New` validates that every entry of `Options.Labels` is a key of
`categoryLabels`, building `d.enabled`, and returns an error naming the unknown
label rather than ignoring it — a typo in config must not silently disable
protection. It sets `state` to `absent` when `Present(Dir(), …)` is false and
`ready` is deferred to `load`. `loadTransitions` reads the calibration file per the
schema established in Step 1, falling back to a matrix built from
`legalTransition` when the file carries constraints rather than scores.

- [ ] **Step 6: Write the detector's integration test**

Create `internal/privacy/ner/ner_test.go`. It **skips unless the assets are
present**, so `go test ./...` never depends on an 800MB download:

```go
func modelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("AIPROXY_MODEL_DIR")
	if dir == "" {
		t.Skip("set AIPROXY_MODEL_DIR to a fetched model directory to run the model tests")
	}
	return dir
}

func TestDetectorFindsPeopleAndEmails(t *testing.T) {
	d, err := New(Options{Dir: modelDir(t), Labels: []string{"private_person", "private_email"},
		MaxScanBytes: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	const text = "Please email Ada Lovelace at ada@example.org about the invoice."
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	var sawPerson, sawEmail bool
	for _, f := range got {
		span := text[f.Start:f.End]
		switch f.Label {
		case privacy.LabelPerson:
			sawPerson = strings.Contains(span, "Ada")
		case privacy.LabelEmail:
			sawEmail = strings.Contains(span, "ada@example.org")
		}
		if f.Start < 0 || f.End > len(text) {
			t.Errorf("span [%d,%d) out of range for %d bytes", f.Start, f.End, len(text))
		}
	}
	if !sawPerson || !sawEmail {
		t.Errorf("person=%v email=%v; findings=%+v", sawPerson, sawEmail, got)
	}
}

// A disabled category must produce nothing, even when the model is confident.
func TestDetectorHonoursTheEnabledLabelSet(t *testing.T) {
	d, err := New(Options{Dir: modelDir(t), Labels: []string{"private_person"}, MaxScanBytes: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Scan(context.Background(), "Email ada@example.org today please.")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.Label == privacy.LabelEmail {
			t.Errorf("returned a disabled category: %+v", f)
		}
	}
}

func TestDetectorWithNoLabelsNeverLoadsTheModel(t *testing.T) {
	d, err := New(Options{Dir: t.TempDir(), Labels: nil, MaxScanBytes: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	// No assets in that directory at all: if Scan tried to load, it would fail.
	got, err := d.Scan(context.Background(), "Ada Lovelace lives here somewhere.")
	if err != nil {
		t.Fatalf("Scan with no labels must not load the model: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("findings with no labels enabled: %+v", got)
	}
}

func TestNewRejectsAnUnknownLabel(t *testing.T) {
	if _, err := New(Options{Dir: t.TempDir(), Labels: []string{"private_shoesize"}}); err == nil {
		t.Fatal("an unknown label was accepted; a typo in config must be reported, not ignored")
	}
}

// fakeRunner returns canned logits, so the chunking, offset mapping, and
// de-duplication are tested with no model, no ONNX Runtime, and no download —
// which is where the bugs in this file will actually be.
type fakeRunner struct {
	calls  int
	widths []int
	// tag returns the label index for token i of a window.
	tag func(window, i int) int
	n   int // number of labels
}

func (f *fakeRunner) Run(ids, mask []int64) ([][]float32, error) {
	w := f.calls
	f.calls++
	f.widths = append(f.widths, len(ids))
	out := make([][]float32, len(ids))
	for i := range out {
		out[i] = make([]float32, f.n)
		out[i][f.tag(w, i)] = 10
	}
	return out, nil
}

func (f *fakeRunner) Close() error { return nil }

// An entity reported in two overlapping windows must yield ONE finding, or a
// value at a chunk seam would be replaced twice and the second replacement would
// land on already-substituted text.
func TestScanDeduplicatesAcrossOverlappingWindows(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	// Tag the same absolute token range in both windows.
	fake.tag = func(w, i int) int {
		if tokenIsInSharedRegion(w, i) {
			return labelIndex(t, d, "S-private_person")
		}
		return labelIndex(t, d, "O")
	}
	got, err := d.Scan(context.Background(), longTextSpanningTwoWindows())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[[2]int]int{}
	for _, f := range got {
		seen[[2]int{f.Start, f.End}]++
	}
	for span, n := range seen {
		if n > 1 {
			t.Errorf("span %v reported %d times; the overlap must be de-duplicated", span, n)
		}
	}
}

// Windows must overlap, or an entity straddling a boundary is seen by neither.
func TestScanWindowsOverlap(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	fake.tag = func(int, int) int { return labelIndex(t, d, "O") }
	if _, err := d.Scan(context.Background(), longTextSpanningTwoWindows()); err != nil {
		t.Fatal(err)
	}
	if fake.calls < 2 {
		t.Fatalf("input spanning two windows produced %d inference calls", fake.calls)
	}
	var total int
	for _, w := range fake.widths {
		total += w
	}
	if total <= tokenCount(t, d, longTextSpanningTwoWindows()) {
		t.Error("windows did not overlap; a span at the seam would be missed")
	}
}

// Truncation is reported, never silent.
func TestScanReportsTruncation(t *testing.T) {
	var buf bytes.Buffer
	d, fake := newFakeDetectorWithLog(t, []string{"private_person"},
		slog.New(slog.NewTextHandler(&buf, nil)))
	d.opts.MaxScanBytes = 64
	fake.tag = func(int, int) int { return labelIndex(t, d, "O") }
	if _, err := d.Scan(context.Background(), strings.Repeat("Ada Lovelace. ", 200)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "truncated") {
		t.Error("a truncated scan must say so; a silent miss is the failure this component exists to avoid")
	}
}
```

`newFakeDetector`, `labelIndex`, `tokenCount`, `longTextSpanningTwoWindows`, and
`tokenIsInSharedRegion` are small helpers in the same file: they build a
`Detector` with `loadOnce` already consumed and its fields set to a real
tokenizer, the label list, a `legalTransition`-derived transition matrix, and the
fake runner. Building the tokenizer needs `tokenizer.json`, so these tests use
`modelPath`-style skipping for that one file only — it is 2 MB rather than 800 MB
and can reasonably be committed to `testdata/` if the download is unwelcome in
CI.

- [ ] **Step 7: Verify against the real model**

```bash
go test ./internal/privacy/ner -race -v                     # decode tests only
AIPROXY_MODEL_DIR=~/.config/aiproxy/models/privacy-filter \
  go test ./internal/privacy/ner -race -v -run TestDetector # with assets present
go test ./... -race                                          # nothing else disturbed
```

- [ ] **Step 8: Register the detector in cmd/aiproxy**

In `buildPrivacy`, after the denylist, append the model **last** so the
deterministic rules win an identical-span tie:

```go
	if cfg.Privacy.NER.Enabled && len(cfg.Privacy.NER.Labels) > 0 {
		nd, err := ner.New(ner.Options{
			Dir:          ner.Dir(),
			Labels:       cfg.Privacy.NER.Labels,
			MaxScanBytes: cfg.Privacy.NER.MaxScanBytes,
			Log:          log,
		})
		if err != nil {
			return nil, err
		}
		dets = append(dets, nd)
		modelState = nd.ModelState
	}
```

and pass `modelState` through `privacy.Options` so `Filter.ModelState` reports it.

- [ ] **Step 9: Commit**

```bash
git add internal/privacy/ner cmd/aiproxy
git commit -m "feat(privacy): NER detector with constrained BIOES decoding

Records the calibration and id2label shapes: <paste Step 1 output here>."
```

---

### Task 19: `aiproxy privacy install`, and the README

**Files:**
- Create: `cmd/aiproxy/privacy.go`
- Modify: `cmd/aiproxy/update.go` (`dispatchSubcommand` gains a case)
- Modify: `README.md`
- Test: `cmd/aiproxy/privacy_test.go`

**Interfaces:**
- Consumes: `ner.Assets`, `ner.Dir`, `ner.Ensure`, `ner.Present` (Task 17); `dispatchSubcommand` (the self-update work).
- Produces: `func runPrivacy(args []string, out io.Writer, dir string, assets []ner.Asset) int` — exit 0 on success, 2 on error.

- [ ] **Step 1: Write the failing test**

Create `cmd/aiproxy/privacy_test.go`:

```go
func TestPrivacyInstallDownloadsAssets(t *testing.T) {
	body := []byte("model bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	sum := sha256.Sum256(body)
	assets := []ner.Asset{{Name: "m.onnx", URL: srv.URL + "/m", SHA256: hex.EncodeToString(sum[:])}}
	dir := t.TempDir()

	var out bytes.Buffer
	if code := runPrivacy([]string{"install"}, &out, dir, assets); code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "m.onnx")); err != nil {
		t.Errorf("asset not installed: %v", err)
	}
	if !strings.Contains(out.String(), dir) {
		t.Errorf("output should name where it installed: %s", out.String())
	}
}

func TestPrivacyStatusReportsMissingAssets(t *testing.T) {
	assets := []ner.Asset{{Name: "m.onnx", URL: "https://example.invalid/m", SHA256: "00"}}
	var out bytes.Buffer
	if code := runPrivacy([]string{"status"}, &out, t.TempDir(), assets); code != 2 {
		t.Errorf("exit code = %d, want 2 when assets are missing", code)
	}
	if !strings.Contains(out.String(), "install") {
		t.Errorf("output should name the fix: %s", out.String())
	}
}

func TestPrivacyRejectsAnUnknownAction(t *testing.T) {
	var out bytes.Buffer
	if code := runPrivacy([]string{"frobnicate"}, &out, t.TempDir(), nil); code != 2 {
		t.Error("an unknown action must be rejected")
	}
}

func TestPrivacySubcommandIsDispatched(t *testing.T) {
	var out bytes.Buffer
	// "privacy" with no action prints usage and exits 2, which proves dispatch
	// reached it rather than treating it as an unknown command.
	code := dispatchSubcommand([]string{"privacy"}, &out)
	if code != 2 || strings.Contains(out.String(), "unknown command") {
		t.Errorf("privacy was not dispatched: code=%d out=%s", code, out.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/aiproxy -run TestPrivacy`
Expected: FAIL — `undefined: runPrivacy`.

- [ ] **Step 3: Implement**

Create `cmd/aiproxy/privacy.go` with
`func runPrivacy(args []string, out io.Writer, dir string, assets []ner.Asset) int`
handling two actions:

- **`install`** — prints the total download size first, since ~840 MB is not
  something to start silently; calls `ner.Ensure` with a progress callback that
  reports per-asset percentage; prints the directory on success.
- **`status`** — prints each asset's name and whether it is present and verified;
  exits 0 when all are, 2 otherwise with the `aiproxy privacy install` hint.

Anything else prints usage and exits 2. Then in `dispatchSubcommand`:

```go
	case "privacy":
		return runPrivacy(args[1:], out, ner.Dir(), assetsForHost())
```

where `assetsForHost` calls `ner.Assets(runtime.GOOS, runtime.GOARCH)` and
reports an unsupported platform as an error rather than a panic.

- [ ] **Step 4: Update the README**

Add a **Privacy filter** section after Updating:

- What it does, in two sentences, and that it is **off by default**.
- The config block, with every field and its default, and the note that
  `ner.labels` is empty by default because `private_url` and `private_date` would
  be destructive in source code.
- `aiproxy privacy install` / `status`, and the ~840 MB footprint.
- The failure modes table from spec §12, in prose: fail-closed refuses the
  request; unresolved placeholders pass through and are counted.
- **What is and is not protected**, stated plainly: credentials and internal
  identifiers by deterministic rules, prose PII by the model, and *not* the source
  code itself — the agent needs that to work.
- The residual disclosure from spec §5.1: a stable placeholder tells the provider
  "this is the same value as before". A hash, not the value, and the price of
  prompt-cache stability.

Then verify every claim against the code, the way the Updating section was:

```bash
grep -n 'checkEnabled\|"privacy"' internal/config/config.go
grep -n 'case "privacy":' cmd/aiproxy/update.go
grep -n 'LabelURL\|LabelDate' internal/privacy/placeholder.go
go test ./... -race
```

- [ ] **Step 5: Commit**

```bash
git add cmd/aiproxy README.md
git commit -m "feat(privacy): add the privacy install subcommand and document the filter"
```

---

## Done when

```bash
gofmt -l .          # must print nothing
go vet ./...
staticcheck ./...
go test ./... -race
go test ./internal/privacy -run Fuzz -fuzz FuzzRewriterRoundTrip -fuzztime 60s
go test ./internal/privacy -run Fuzz -fuzz FuzzWalkStrings -fuzztime 30s
```

All green, and:

- A secret in a request never reaches upstream, and the agent still receives it in
  the response.
- Splitting an SSE stream at **every** byte offset restores identically.
- A file-write tool call whose content contains a secret with JSON metacharacters
  arrives at the agent byte-exact.
- With the filter disabled, `go test ./internal/proxy` behaves exactly as it did
  before Task 13 — the relay's write path is untouched.
- `onScanFailure: closed` with a failing detector sends **zero** requests upstream.
- The scan cache holds no plaintext, and a second identical request performs zero
  detector calls.
- `AIPROXY_TOKENIZER_JSON=… go test ./internal/privacy/tokenizer` matches the
  reference on ids **and** byte offsets for every fixture.

If Task 16's gate failed, everything above except the last two bullets still
holds, and Part 1 ships on its own.
