package ner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/privacy/tokenizer"
)

// configLabels is id2label from the shipped config.json, in model order. Kept
// here as a fixture so the fake-runner tests need no download;
// TestLoadLabelsMatchesTheShippedConfig checks it against the real file when the
// assets are present.
var configLabels = []string{
	"O",
	"B-account_number", "I-account_number", "E-account_number", "S-account_number",
	"B-private_address", "I-private_address", "E-private_address", "S-private_address",
	"B-private_date", "I-private_date", "E-private_date", "S-private_date",
	"B-private_email", "I-private_email", "E-private_email", "S-private_email",
	"B-private_person", "I-private_person", "E-private_person", "S-private_person",
	"B-private_phone", "I-private_phone", "E-private_phone", "S-private_phone",
	"B-private_url", "I-private_url", "E-private_url", "S-private_url",
	"B-secret", "I-secret", "E-secret", "S-secret",
}

func modelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("AIPROXY_MODEL_DIR")
	if dir == "" {
		t.Skip("set AIPROXY_MODEL_DIR to a fetched model directory to run the model tests")
	}
	return dir
}

// ---------------------------------------------------------------------------
// Tests that need no model, no ONNX Runtime, and no download.
// ---------------------------------------------------------------------------

// byteTokenizer emits one token per byte, which is enough for every property the
// chunking and mapping code has: contiguous, ordered, non-empty spans.
type byteTokenizer struct{}

func (byteTokenizer) Encode(s string) ([]tokenizer.Token, error) {
	out := make([]tokenizer.Token, len(s))
	for i := range s {
		out[i] = tokenizer.Token{ID: int(s[i]), Start: i, End: i + 1}
	}
	return out, nil
}

// repeatSpanTokenizer reproduces the one tokenizer property that breaks naive
// offset mapping: when a character's bytes do not merge into one token, EVERY
// resulting token carries the whole character's span, so consecutive tokens
// repeat a span and the spans do not tile. Here token pairs 2k and 2k+1 both
// cover bytes [k, k+1).
type repeatSpanTokenizer struct{}

func (repeatSpanTokenizer) Encode(s string) ([]tokenizer.Token, error) {
	out := make([]tokenizer.Token, 0, 2*len(s))
	for i := range s {
		out = append(out,
			tokenizer.Token{ID: int(s[i]), Start: i, End: i + 1},
			tokenizer.Token{ID: int(s[i]), Start: i, End: i + 1})
	}
	return out, nil
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

func (f *fakeRunner) Run(_ context.Context, ids, mask []int64) ([][]float32, error) {
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

func newFakeDetector(t *testing.T, cats []string) (*Detector, *fakeRunner) {
	t.Helper()
	return newFakeDetectorWithLog(t, cats, nil)
}

// newFakeDetectorWithLog builds a Detector with loadOnce already consumed and
// its fields set by hand, so Scan runs its real chunking, decoding, mapping and
// de-duplication against canned logits.
func newFakeDetectorWithLog(t *testing.T, cats []string, log *slog.Logger) (*Detector, *fakeRunner) {
	t.Helper()
	d, err := New(Options{Dir: t.TempDir(), Labels: cats, MaxScanBytes: 1 << 20, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeRunner{n: len(configLabels), tag: func(int, int) int { return 0 }}
	d.tok = byteTokenizer{}
	d.labels = configLabels
	d.trans = transitionMatrix(configLabels, nil)
	d.session = fake
	d.loadOnce.Do(func() {}) // consume it: load must never run for these
	return d, fake
}

func labelIndex(t *testing.T, d *Detector, tag string) int {
	t.Helper()
	for i, l := range d.labels {
		if l == tag {
			return i
		}
	}
	t.Fatalf("no label %q", tag)
	return -1
}

func tokenCount(t *testing.T, d *Detector, text string) int {
	t.Helper()
	toks, err := d.tok.Encode(text)
	if err != nil {
		t.Fatal(err)
	}
	return len(toks)
}

// longTextSpanningTwoWindows is one byte per token under byteTokenizer, so its
// length is its token count: comfortably more than one window, comfortably less
// than three.
func longTextSpanningTwoWindows() string {
	return strings.Repeat("a", window+overlap+100)
}

// tokenIsInSharedRegion marks ONE absolute token that both windows contain, so
// the same entity is reported twice and the de-duplication has something to do.
func tokenIsInSharedRegion(w, i int) bool {
	const sharedAbsolute = window - overlap + 64 // inside [window-overlap, window)
	return w*(window-overlap)+i == sharedAbsolute
}

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
	if len(got) == 0 {
		t.Fatal("no findings; the fixture must place an entity inside the overlap")
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

// The cut must land on a rune boundary, or the tokenizer is handed half a
// character and the offsets that come back describe bytes that do not exist.
func TestScanTruncatesOnARuneBoundary(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	fake.tag = func(int, int) int { return labelIndex(t, d, "O") }
	// "é" is two bytes, so a 3-byte-per-char string has cuts that land mid-rune.
	text := strings.Repeat("aéb", 200)
	for limit := 60; limit < 70; limit++ {
		d.opts.MaxScanBytes = limit
		if _, err := d.Scan(context.Background(), text); err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		w := fake.widths[len(fake.widths)-1]
		if !isValidUTF8Prefix(text, w) {
			t.Errorf("limit %d scanned %d bytes, which is not a rune boundary", limit, w)
		}
	}
}

func isValidUTF8Prefix(s string, n int) bool {
	return n <= len(s) && utf8.ValidString(s[:n])
}

// Offsets must come from min/max over the run's tokens, because token spans do
// not strictly tile: repeated spans are normal, and a run's first token is not
// guaranteed to hold the smallest offset.
func TestScanMapsOffsetsAcrossNonTilingTokens(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	d.tok = repeatSpanTokenizer{}
	// Tokens 2..5 -> bytes [1,3): a B/I/I/E run over two doubled characters.
	fake.tag = func(_, i int) int {
		switch i {
		case 2:
			return labelIndex(t, d, "B-private_person")
		case 3, 4:
			return labelIndex(t, d, "I-private_person")
		case 5:
			return labelIndex(t, d, "E-private_person")
		}
		return labelIndex(t, d, "O")
	}
	const text = "abcdefgh"
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if got[0].Start != 1 || got[0].End != 3 {
		t.Errorf("span = [%d,%d), want [1,3) (%q)", got[0].Start, got[0].End, text[1:3])
	}
}

func TestScanSkipsShortStringsWithoutLoading(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	fake.tag = func(int, int) int { return labelIndex(t, d, "S-private_person") }
	got, err := d.Scan(context.Background(), "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || fake.calls != 0 {
		t.Errorf("a string shorter than MinScanBytes (%d) was scanned: findings=%+v calls=%d",
			privacy.MinScanBytes, got, fake.calls)
	}
}

func TestScanHonoursContextCancellation(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	fake.tag = func(int, int) int { return labelIndex(t, d, "O") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Scan(ctx, longTextSpanningTwoWindows()); !errors.Is(err, context.Canceled) {
		t.Errorf("Scan on a cancelled context = %v, want context.Canceled", err)
	}
}

// A disabled category must produce nothing, even when the model is confident.
func TestScanDropsDisabledCategories(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	fake.tag = func(_, i int) int {
		if i == 3 {
			return labelIndex(t, d, "S-private_email")
		}
		return labelIndex(t, d, "O")
	}
	got, err := d.Scan(context.Background(), "an email lives here")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("returned a disabled category: %+v", got)
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
	if d.ModelState() != stateOff {
		t.Errorf("ModelState = %q, want %q", d.ModelState(), stateOff)
	}
}

func TestNewRejectsAnUnknownLabel(t *testing.T) {
	if _, err := New(Options{Dir: t.TempDir(), Labels: []string{"private_shoesize"}}); err == nil {
		t.Fatal("an unknown label was accepted; a typo in config must be reported, not ignored")
	}
}

// A missing install must wrap ErrModelUnavailable — that is what the control
// path matches on to answer 503 — and must be reported per scan rather than
// retried per scan.
func TestLoadFailureIsCachedAndWrapsErrModelUnavailable(t *testing.T) {
	d, err := New(Options{Dir: filepath.Join(t.TempDir(), "nothing-here"),
		Labels: []string{"private_person"}, MaxScanBytes: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	if d.ModelState() != stateAbsent {
		t.Errorf("ModelState before load = %q, want %q", d.ModelState(), stateAbsent)
	}
	_, e1 := d.Scan(context.Background(), "Ada Lovelace lives here somewhere.")
	_, e2 := d.Scan(context.Background(), "Ada Lovelace lives here somewhere.")
	for _, e := range []error{e1, e2} {
		if !errors.Is(e, privacy.ErrModelUnavailable) {
			t.Fatalf("error = %v, want it to wrap ErrModelUnavailable", e)
		}
	}
	if e1.Error() != e2.Error() {
		t.Errorf("the failure was not cached: %v then %v", e1, e2)
	}
	if d.ModelState() != stateError {
		t.Errorf("ModelState after a failed load = %q, want %q", d.ModelState(), stateError)
	}
}

func TestCategoriesCoverEveryModelCategory(t *testing.T) {
	inConfig := map[string]bool{}
	for _, l := range configLabels {
		if _, cat := splitTag(l); cat != "" {
			inConfig[cat] = true
		}
	}
	for _, c := range Categories() {
		if !inConfig[c] {
			t.Errorf("category %q is not in the model's label set", c)
		}
		delete(inConfig, c)
	}
	if len(inConfig) != 0 {
		t.Errorf("model categories with no placeholder label: %v", inConfig)
	}
}

func TestLibraryNameMatchesTheAssetTable(t *testing.T) {
	for _, c := range []struct{ goos, goarch string }{
		{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"},
	} {
		a, err := runtimeAsset(c.goos, c.goarch)
		if err != nil {
			t.Fatal(err)
		}
		if got := LibraryName(c.goos); got != a.Name {
			t.Errorf("LibraryName(%q) = %q, but the asset is named %q", c.goos, got, a.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests that need the fetched assets. Skipped unless AIPROXY_MODEL_DIR is set,
// so `go test ./...` never depends on an 800MB download.
// ---------------------------------------------------------------------------

func TestLoadLabelsMatchesTheShippedConfig(t *testing.T) {
	got, err := LoadLabels(filepath.Join(modelDir(t), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(configLabels) {
		t.Fatalf("got %d labels, want %d", len(got), len(configLabels))
	}
	for i := range got {
		if got[i] != configLabels[i] {
			t.Errorf("label %d = %q, want %q", i, got[i], configLabels[i])
		}
	}
}

// The risk this whole task exists to retire: that nothing had yet loaded a real
// ONNX Runtime session through the vendored binding. This asserts the session
// opens, names the IO the detector expects, and returns logits of the shape the
// decoder assumes.
func TestRealSessionProducesLogitsOfTheExpectedShape(t *testing.T) {
	dir := modelDir(t)
	labels, err := LoadLabels(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRunner(filepath.Join(dir, LibraryName(runtime.GOOS)),
		filepath.Join(dir, "model_q4f16.onnx"), len(labels))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	s := r.(*onnxSession)
	t.Logf("onnxruntime %s, api v%d", s.rt.GetVersionString(), s.rt.GetAPIVersion())
	t.Logf("inputs=%v outputs=%v", s.sess.InputNames(), s.sess.OutputNames())

	tok, err := tokenizer.Load(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	toks, err := tok.Encode(adaSentence)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(toks))
	mask := make([]int64, len(toks))
	for i, tk := range toks {
		ids[i], mask[i] = int64(tk.ID), 1
	}
	logits, err := r.Run(context.Background(), ids, mask)
	if err != nil {
		t.Fatal(err)
	}
	if len(logits) != len(ids) {
		t.Fatalf("got %d logit rows for %d tokens", len(logits), len(ids))
	}
	for i, row := range logits {
		if len(row) != 33 {
			t.Fatalf("row %d has %d columns, want 33 (8 categories x BIOES + O)", i, len(row))
		}
	}
	t.Logf("logits shape = [1 %d %d]", len(logits), len(logits[0]))
}

const adaSentence = "Please email Ada Lovelace at ada@example.org about the invoice."

func TestDetectorFindsPeopleAndEmails(t *testing.T) {
	d, err := New(Options{Dir: modelDir(t), Labels: []string{"private_person", "private_email"},
		MaxScanBytes: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const text = adaSentence
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	var sawPerson, sawEmail bool
	for _, f := range got {
		if f.Start < 0 || f.End > len(text) || f.End <= f.Start {
			t.Fatalf("span [%d,%d) out of range for %d bytes", f.Start, f.End, len(text))
		}
		span := text[f.Start:f.End]
		t.Logf("finding: label=%s rule=%s bytes=[%d,%d) conf=%.4f text=%q",
			f.Label, f.Rule, f.Start, f.End, f.Confidence, span)
		switch f.Label {
		case privacy.LabelPerson:
			sawPerson = sawPerson || strings.Contains(span, "Ada")
		case privacy.LabelEmail:
			sawEmail = sawEmail || strings.Contains(span, "ada@example.org")
		}
	}
	if !sawPerson || !sawEmail {
		t.Errorf("person=%v email=%v; findings=%+v", sawPerson, sawEmail, got)
	}
	if d.ModelState() != stateReady {
		t.Errorf("ModelState = %q, want %q", d.ModelState(), stateReady)
	}
}

// A disabled category must produce nothing, even when the model is confident.
func TestDetectorHonoursTheEnabledLabelSet(t *testing.T) {
	d, err := New(Options{Dir: modelDir(t), Labels: []string{"private_person"}, MaxScanBytes: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
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

// TestModelLatency is the measurement the caching design rests on. It asserts
// nothing about wall-clock — that would be a flaky test on shared CI — and only
// reports, so run it with -v.
func TestModelLatency(t *testing.T) {
	dir := modelDir(t)
	d, err := New(Options{Dir: dir, Labels: []string{"private_person", "private_email"},
		MaxScanBytes: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	start := time.Now()
	if _, err := d.Scan(context.Background(), adaSentence); err != nil {
		t.Fatal(err)
	}
	t.Logf("cold  short (%d bytes, includes load): %v", len(adaSentence), time.Since(start))

	for _, c := range []struct {
		name string
		text string
	}{
		{"short", adaSentence},
		{"code4k", sourceCodeSample(4 << 10)},
		{"code16k", sourceCodeSample(16 << 10)},
	} {
		// One warm-up, then the median of five.
		if _, err := d.Scan(context.Background(), c.text); err != nil {
			t.Fatal(err)
		}
		var times []time.Duration
		var found []privacy.Finding
		for range 5 {
			t0 := time.Now()
			got, err := d.Scan(context.Background(), c.text)
			if err != nil {
				t.Fatal(err)
			}
			times = append(times, time.Since(t0))
			found = got
		}
		toks, err := d.tok.Encode(c.text)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("warm  %s (%d bytes, %d tokens, %d findings): %v",
			c.name, len(c.text), len(toks), len(found), times)
		for _, f := range found {
			t.Logf("    %s [%d,%d) %q", f.Label, f.Start, f.End, c.text[f.Start:f.End])
		}
	}
}

// sourceCodeSample is a realistic agent payload: Go source with the kinds of
// URLs, identifiers and names that make source code the hard case.
func sourceCodeSample(n int) string {
	const unit = `package billing

// Invoice is one billed period for a customer account.
type Invoice struct {
	ID        string    // e.g. "inv_8813"
	AccountID string    // maps to accounts.id
	Email     string    // billing contact, e.g. grace.hopper@example.com
	Total     int64     // minor units
	IssuedAt  time.Time
}

// Send posts the invoice to the billing service. See
// https://docs.example.com/billing/v2/invoices for the payload contract.
func (c *Client) Send(ctx context.Context, inv Invoice) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v2/invoices", body(inv))
	if err != nil {
		return fmt.Errorf("billing: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("billing: send %s: %w", inv.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("billing: send %s: status %d", inv.ID, resp.StatusCode)
	}
	return nil
}

`
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "// block %d\n%s", i, unit)
	}
	return b.String()[:n]
}

// A byte-level BPE folds the preceding space into a word's token, so a raw span
// is " Ada Lovelace". Redacting that eats the separator, so the whitespace must
// be handed back before the finding leaves Scan.
func TestScanTrimsWhitespaceFromSpans(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	const text = "email Ada Lovelace now"
	// byteTokenizer is one token per byte, so tag bytes [5,18): " Ada Lovelace".
	fake.tag = func(_, i int) int {
		switch {
		case i == 5:
			return labelIndex(t, d, "B-private_person")
		case i > 5 && i < 17:
			return labelIndex(t, d, "I-private_person")
		case i == 17:
			return labelIndex(t, d, "E-private_person")
		}
		return labelIndex(t, d, "O")
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if span := text[got[0].Start:got[0].End]; span != "Ada Lovelace" {
		t.Errorf("span = %q, want %q", span, "Ada Lovelace")
	}
}

// A span that is nothing but whitespace must be dropped rather than reported as
// an empty range.
func TestScanDropsAnAllWhitespaceSpan(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	const text = "email    now please"
	fake.tag = func(_, i int) int {
		if i == 6 {
			return labelIndex(t, d, "S-private_person")
		}
		return labelIndex(t, d, "O")
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("an all-whitespace span was reported: %+v", got)
	}
}

// The default cap is now 4 KB rather than 256 KB, so truncation stops being the
// rare case and becomes the ordinary one: a 16 KB tool result gets scanned to
// 4 KB. That MUST still be logged. This test reads the cap from config.Default()
// rather than hardcoding it, so lowering the default further cannot quietly
// break the log, and raising it cannot quietly reintroduce a 45-second scan
// without TestPrivacyNERDefaultBoundsWorstCaseScanLatency also failing.
func TestScanReportsTruncationAtTheConfiguredDefault(t *testing.T) {
	limit := config.Default().Privacy.NER.MaxScanBytes
	if limit <= 0 {
		t.Fatalf("config default MaxScanBytes = %d", limit)
	}

	var buf bytes.Buffer
	d, fake := newFakeDetectorWithLog(t, []string{"private_person"},
		slog.New(slog.NewTextHandler(&buf, nil)))
	d.opts.MaxScanBytes = limit
	fake.tag = func(int, int) int { return labelIndex(t, d, "O") }

	// A realistic oversized payload: a 16 KB tool result.
	text := sourceCodeSample(16 << 10)
	if _, err := d.Scan(context.Background(), text); err != nil {
		t.Fatal(err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "truncated") {
		t.Fatalf("a %d-byte input under a %d-byte cap logged nothing: %q", len(text), limit, logged)
	}
	// The operator needs the numbers, not just the word: how much arrived, how
	// much was looked at, and what the cap was.
	for _, want := range []string{
		fmt.Sprintf("bytes=%d", len(text)),
		fmt.Sprintf("scanned=%d", limit),
		fmt.Sprintf("limit=%d", limit),
		"level=WARN",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("truncation log is missing %q: %s", want, logged)
		}
	}
	// And it must actually have stopped at the cap, not merely said so.
	var total int
	for _, w := range fake.widths {
		total += w
	}
	if total == 0 || fake.widths[0] > limit {
		t.Errorf("first window was %d tokens for a %d-byte cap", fake.widths[0], limit)
	}
}

// A string at or under the cap must NOT log truncation, or the warning becomes
// noise the operator learns to ignore.
func TestScanIsSilentWhenNothingIsTruncated(t *testing.T) {
	limit := config.Default().Privacy.NER.MaxScanBytes
	var buf bytes.Buffer
	d, fake := newFakeDetectorWithLog(t, []string{"private_person"},
		slog.New(slog.NewTextHandler(&buf, nil)))
	d.opts.MaxScanBytes = limit
	fake.tag = func(int, int) int { return labelIndex(t, d, "O") }
	if _, err := d.Scan(context.Background(), sourceCodeSample(limit)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "truncated") {
		t.Errorf("a %d-byte input under a %d-byte cap logged truncation: %s", limit, limit, buf.String())
	}
}

// The same thing against the real model, so the new default is confirmed
// end-to-end rather than only against a fake: a 16 KB payload under the 4 KB
// default must be scanned to 4 KB, must say so, and must come back in the ~520 ms
// the cap was chosen to buy.
func TestRealDetectorTruncatesAndReportsAtTheConfiguredDefault(t *testing.T) {
	dir := modelDir(t)
	limit := config.Default().Privacy.NER.MaxScanBytes
	var buf bytes.Buffer
	d, err := New(Options{Dir: dir, Labels: []string{"private_person", "private_email"},
		MaxScanBytes: limit, Log: slog.New(slog.NewTextHandler(&buf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	text := sourceCodeSample(16 << 10)
	if _, err := d.Scan(context.Background(), text); err != nil { // warm the session
		t.Fatal(err)
	}
	buf.Reset()
	t0 := time.Now()
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(t0)
	t.Logf("16KB input under a %d-byte cap: %v, %d findings", limit, elapsed, len(got))
	t.Logf("log: %s", strings.TrimSpace(buf.String()))

	if !strings.Contains(buf.String(), "truncated") {
		t.Errorf("the real detector truncated %d bytes to %d silently", len(text), limit)
	}
	for _, f := range got {
		if f.End > limit {
			t.Errorf("finding [%d,%d) is past the %d-byte cap", f.Start, f.End, limit)
		}
	}
}

// A MULTI-TOKEN entity straddling a window boundary is the case the local
// de-duplication cannot handle, and the case TestScanDeduplicatesAcrossOverlapping-
// Windows cannot reach: that test tags a single S- token, which is boundary-immune
// and can only ever decode identically in both windows.
//
// Here window i sees only the entity's first tokens, and Decode's "close on O, E,
// or S" terminal constraint forces a legal tag onto its last token — yielding a
// TRUNCATED span. Window i+1 sees the entity whole and yields the full span.
// Different (Start, End) pairs, so Scan returns BOTH. The guarantee that only the
// full span is redacted lives in privacy.Resolve, so that is what this asserts.
func TestBoundaryEntityResolvesToOneFullSpan(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	// Shrunk from 2048/512: forcing this at the real window size would need
	// megabytes of fixture for no extra coverage.
	d.window, d.overlap = 8, 4

	// 20 bytes, one token per byte under byteTokenizer. Windows are [0,8),
	// [4,12), [8,16), [12,20). The entity is absolute tokens [6,10) — inside
	// window 1 whole, and cut by the boundaries of windows 0 and 2.
	const text = "0123456789abcdefghij"
	const entStart, entEnd = 6, 10

	// Tag by ABSOLUTE token, which is what a real model does: the same token
	// gets the same tag whichever window it is seen in.
	fake.tag = func(w, i int) int {
		abs := w*(d.window-d.overlap) + i
		switch {
		case abs == entStart:
			return labelIndex(t, d, "B-private_person")
		case abs > entStart && abs < entEnd-1:
			return labelIndex(t, d, "I-private_person")
		case abs == entEnd-1:
			return labelIndex(t, d, "E-private_person")
		}
		return labelIndex(t, d, "O")
	}

	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Logf("Scan returned [%d,%d) %q", f.Start, f.End, text[f.Start:f.End])
	}

	// The premise: Scan itself emits differently-clipped spans for one entity.
	// If this stops being true the test below stops proving anything, so it is
	// asserted rather than assumed.
	var sawFull, sawClipped bool
	for _, f := range got {
		switch {
		case f.Start == entStart && f.End == entEnd:
			sawFull = true
		case f.Start >= entStart && f.End <= entEnd:
			sawClipped = true
		}
	}
	if !sawFull {
		t.Fatalf("no window decoded the whole entity [%d,%d): %+v", entStart, entEnd, got)
	}
	if !sawClipped {
		t.Fatalf("the fixture no longer produces a clipped span, so it cannot "+
			"exercise the case it exists for: %+v", got)
	}

	// The guarantee, where it actually lives: Resolve drops a span overlapping
	// one already kept and keeps the longer, so the clipped spans disappear and
	// the whole entity is redacted exactly once.
	resolved := privacy.Resolve([][]privacy.Finding{got})
	if len(resolved) != 1 {
		t.Fatalf("Resolve returned %d findings, want 1 covering the whole entity: %+v",
			len(resolved), resolved)
	}
	if resolved[0].Start != entStart || resolved[0].End != entEnd {
		t.Errorf("resolved span = [%d,%d) %q, want [%d,%d) %q",
			resolved[0].Start, resolved[0].End, text[resolved[0].Start:resolved[0].End],
			entStart, entEnd, text[entStart:entEnd])
	}
	// And nothing is redacted twice: a byte covered by two findings would be
	// substituted, then substituted again inside already-placeholder text.
	covered := map[int]int{}
	for _, f := range resolved {
		for b := f.Start; b < f.End; b++ {
			covered[b]++
		}
	}
	for b, n := range covered {
		if n > 1 {
			t.Errorf("byte %d is covered by %d findings", b, n)
		}
	}
}

// Exact duplicates from the overlap must still collapse inside Scan, so the
// best-effort dedup is not dead code that Resolve merely papers over.
func TestScanStillCollapsesExactDuplicatesAtASmallWindow(t *testing.T) {
	d, fake := newFakeDetector(t, []string{"private_person"})
	d.window, d.overlap = 8, 4
	const text = "0123456789abcdefghij"
	// Absolute token 5 is inside both window 0 [0,8) and window 1 [4,12), and a
	// single-token S- entity decodes identically in both.
	fake.tag = func(w, i int) int {
		if w*(d.window-d.overlap)+i == 5 {
			return labelIndex(t, d, "S-private_person")
		}
		return labelIndex(t, d, "O")
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, f := range got {
		if f.Start == 5 && f.End == 6 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the identical span [5,6) was returned %d times: %+v", n, got)
	}
}

// ModelState must distinguish "not installed" from "installed, not yet used":
// they call for different messages in the TUI — run the install, versus nothing
// is wrong.
func TestModelStateReportsAbsentWhenTheAssetsAreMissing(t *testing.T) {
	d, err := New(Options{Dir: filepath.Join(t.TempDir(), "nothing-here"),
		Labels: []string{"private_person"}, MaxScanBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ModelState(); got != stateAbsent {
		t.Errorf("ModelState with no assets on disk = %q, want %q", got, stateAbsent)
	}
}

// A directory with the right filenames but wrong contents is NOT installed:
// Present verifies digests, so a partial or corrupted fetch reports absent
// rather than pretending to be ready.
func TestModelStateReportsAbsentForAnUnverifiedInstall(t *testing.T) {
	dir := t.TempDir()
	assets, err := Assets(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no ONNX Runtime build for this platform: %v", err)
	}
	for _, a := range assets {
		if err := os.WriteFile(filepath.Join(dir, a.Name), []byte("not the real file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d, err := New(Options{Dir: dir, Labels: []string{"private_person"}, MaxScanBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.ModelState(); got != stateAbsent {
		t.Errorf("ModelState with wrong-digest files = %q, want %q", got, stateAbsent)
	}
}

// The other half, against the real install: present but not yet exercised must
// report "installed", and only become "ready" once a scan has built the session.
func TestModelStateReportsInstalledBeforeTheFirstScan(t *testing.T) {
	dir := modelDir(t)
	d, err := New(Options{Dir: dir, Labels: []string{"private_person"}, MaxScanBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := d.ModelState(); got != stateInstalled {
		t.Fatalf("ModelState before the first scan = %q, want %q", got, stateInstalled)
	}
	if _, err := d.Scan(context.Background(), adaSentence); err != nil {
		t.Fatal(err)
	}
	if got := d.ModelState(); got != stateReady {
		t.Errorf("ModelState after a successful scan = %q, want %q", got, stateReady)
	}
}

// detectorWithRunner builds a Detector wired to an arbitrary runner, with
// loadOnce already consumed so nothing is ever loaded from disk.
func detectorWithRunner(t *testing.T, r runner) *Detector {
	t.Helper()
	d, err := New(Options{Dir: t.TempDir(), Labels: []string{"private_person"}, MaxScanBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	d.tok = byteTokenizer{}
	d.labels = configLabels
	d.trans = transitionMatrix(configLabels, nil)
	d.session = r
	d.loadOnce.Do(func() {})
	return d
}

// blockingRunner holds the model for as long as it is asked to, so a caller's
// deadline can be observed against a busy session.
type blockingRunner struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (b *blockingRunner) Run(ctx context.Context, ids, _ []int64) ([][]float32, error) {
	b.calls.Add(1)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(ids))
	for i := range out {
		out[i] = make([]float32, len(configLabels))
	}
	return out, nil
}

func (b *blockingRunner) Close() error { return nil }

// The session is process-wide and serialised, so one slow inference is a
// head-of-line block on every other request that needs the model. A request
// whose own scan deadline has already passed must leave the queue rather than
// wait for work it will discard: sync.Mutex has no cancellable acquire, which is
// why Run gates on a channel.
func TestScanLeavesTheModelQueueWhenTheContextIsDone(t *testing.T) {
	r := &blockingRunner{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(r.release)

	// One scan takes the session and holds it.
	holder := detectorWithRunner(t, r)
	go holder.Scan(context.Background(), strings.Repeat("a b ", 40))
	select {
	case <-r.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first scan never reached the runner")
	}

	// A second scan whose deadline expires while it waits must return promptly.
	waiter := detectorWithRunner(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := waiter.Scan(ctx, strings.Repeat("a b ", 40))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Scan = %v, want the deadline to be observed while queued", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Scan waited %s on a busy session; the queue is not cancellable", elapsed)
	}
}
