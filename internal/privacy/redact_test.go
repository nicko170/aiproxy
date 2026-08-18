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
		{"data", "", false}, // only under a source block
		{"content", "", false},
		{"text", "", false},
	} {
		if got := SkipKey(c.key, c.parent); got != c.want {
			t.Errorf("SkipKey(%q, %q) = %v, want %v", c.key, c.parent, got, c.want)
		}
	}
}
