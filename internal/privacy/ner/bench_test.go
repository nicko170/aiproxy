package ner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/privacy"
	"github.com/nicko170/aiproxy/internal/privacy/tokenizer"
)

// These benchmarks measure the model tier on real hardware, because every
// latency number in this package's design was an estimate until one was taken.
// The cap on privacy.ner.maxScanBytes exists because of what they report, so
// re-run them on a new platform before trusting that default there.
//
//	AIPROXY_MODEL_DIR=~/.config/aiproxy/models/privacy-filter \
//	  go test ./internal/privacy/ner -run '^$' -bench . -benchtime 10x
//
// tok/s is the headline: this is a token classifier, so cost scales with tokens
// rather than bytes, and B/s varies with how dense the text is.

// benchModelDir skips rather than fails when the ~850MB asset set is absent, so
// `go test ./...` never depends on a download.
func benchModelDir(b *testing.B) string {
	b.Helper()
	dir := os.Getenv("AIPROXY_MODEL_DIR")
	if dir == "" {
		b.Skip("set AIPROXY_MODEL_DIR to a fetched model directory to run the model benchmarks")
	}
	return dir
}

// benchLabels is the full label set. Enabling every category is the worst case
// for the decode, and the honest one to quote.
var benchLabels = []string{
	"secret", "private_email", "private_phone", "private_address",
	"private_person", "private_url", "private_date", "account_number",
}

// prose is what the model tier actually exists for: PII in running text.
const prose = "Please email Ada Lovelace at ada@example.org or call +44 20 7946 0958 " +
	"about the invoice for 12 Rue de Rivoli, Paris, dated 3 March 2026. " +
	"Her account number is 4111 1111 1111 1111 and the API key is sk-ant-api03-abcdef. "

// code is what a coding agent actually sends, which is mostly NOT prose — worth
// measuring separately because token density differs sharply.
const code = "func (d *Detector) Scan(ctx context.Context, text string) ([]privacy.Finding, error) {\n" +
	"\tif len(d.enabled) == 0 || len(text) < privacy.MinScanBytes {\n\t\treturn nil, nil\n\t}\n" +
	"\tif err := d.load(); err != nil {\n\t\treturn nil, err\n\t}\n" +
	"\ttoks, err := d.tok.Encode(scanned)\n\tif err != nil {\n\t\treturn nil, fmt.Errorf(\"ner: tokenize: %w\", err)\n\t}\n"

// grow repeats seed until it is at least n bytes, then cuts on a rune boundary.
func grow(seed string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(seed)
	}
	s := b.String()
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(c byte) bool { return c&0xC0 != 0x80 }

// newBenchDetector builds one detector and loads the model once, so the load
// cost is not smeared across every measurement. MaxScanBytes is set high enough
// not to truncate the input under test; the shipping default is 4096 and is
// measured explicitly by BenchmarkScanAtShippingCap.
func newBenchDetector(b *testing.B, maxScan int) *Detector {
	b.Helper()
	d, err := New(Options{Dir: benchModelDir(b), Labels: benchLabels, MaxScanBytes: maxScan})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	// Force the load outside the timed region.
	if _, err := d.Scan(context.Background(), "warmup text for the session"); err != nil {
		b.Fatalf("warmup Scan: %v", err)
	}
	b.Cleanup(func() { d.Close() })
	return d
}

// countTokens reports how many tokens the input costs, so throughput can be
// quoted per token rather than only per byte.
func countTokens(b *testing.B, dir, text string) int {
	b.Helper()
	tok, err := tokenizer.Load(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		b.Fatalf("tokenizer.Load: %v", err)
	}
	toks, err := tok.Encode(text)
	if err != nil {
		b.Fatalf("Encode: %v", err)
	}
	return len(toks)
}

// BenchmarkScan is the headline: warm inference across the input sizes a real
// request actually carries, for both prose and code.
func BenchmarkScan(b *testing.B) {
	dir := benchModelDir(b)
	d := newBenchDetector(b, 1<<20)

	for _, shape := range []struct {
		name string
		seed string
	}{{"prose", prose}, {"code", code}} {
		for _, size := range []int{64, 512, 2048, 4096, 16384} {
			text := grow(shape.seed, size)
			tokens := countTokens(b, dir, text)

			b.Run(fmt.Sprintf("%s/%dB", shape.name, size), func(b *testing.B) {
				b.SetBytes(int64(len(text)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := d.Scan(context.Background(), text); err != nil {
						b.Fatalf("Scan: %v", err)
					}
				}
				b.StopTimer()
				secs := b.Elapsed().Seconds()
				b.ReportMetric(float64(tokens*b.N)/secs, "tok/s")
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/op")
				b.ReportMetric(float64(tokens), "tokens")
			})
		}
	}
}

// BenchmarkScanAtShippingCap measures what an operator actually gets with the
// default privacy.ner.maxScanBytes=4096: anything longer is truncated, so the
// per-request cost stops growing. This is the number the default was chosen on.
func BenchmarkScanAtShippingCap(b *testing.B) {
	dir := benchModelDir(b)
	d := newBenchDetector(b, 4096)
	text := grow(prose, 64<<10) // far over the cap
	capped := grow(prose, 4096)
	tokens := countTokens(b, dir, capped)

	b.SetBytes(4096) // what is actually scanned, not what was submitted
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Scan(context.Background(), text); err != nil {
			b.Fatalf("Scan: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(tokens*b.N)/b.Elapsed().Seconds(), "tok/s")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/op")
}

// BenchmarkScanParallel measures aggregate throughput with concurrent callers.
// onnxSession.Run is serialised behind a semaphore by deliberate choice, so this
// should show total throughput flat against GOMAXPROCS — that flatness is the
// head-of-line blocking the design accepted, quantified.
func BenchmarkScanParallel(b *testing.B) {
	dir := benchModelDir(b)
	d := newBenchDetector(b, 1<<20)
	text := grow(prose, 2048)
	tokens := countTokens(b, dir, text)

	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := d.Scan(context.Background(), text); err != nil {
				b.Fatalf("Scan: %v", err)
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(tokens*b.N)/b.Elapsed().Seconds(), "tok/s")
}

// BenchmarkLoad measures the one-off cost of dlopening ONNX Runtime, reading
// the tokenizer, and creating the session — what the first request after a
// restart pays. Weights are mmapped, so this is far below what ~850MB suggests.
func BenchmarkLoad(b *testing.B) {
	dir := benchModelDir(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d, err := New(Options{Dir: dir, Labels: benchLabels, MaxScanBytes: 4096})
		if err != nil {
			b.Fatalf("New: %v", err)
		}
		if _, err := d.Scan(context.Background(), "one short sentence to force the load"); err != nil {
			b.Fatalf("Scan: %v", err)
		}
		b.StopTimer()
		d.Close()
		b.StartTimer()
	}
}

// BenchmarkTokenize isolates the tokenizer, so a slow Scan can be attributed to
// inference rather than to the BPE it was blamed on.
func BenchmarkTokenize(b *testing.B) {
	dir := benchModelDir(b)
	tok, err := tokenizer.Load(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		b.Fatalf("tokenizer.Load: %v", err)
	}
	text := grow(prose, 4096)
	tokens := countTokens(b, dir, text)

	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tok.Encode(text); err != nil {
			b.Fatalf("Encode: %v", err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(tokens*b.N)/b.Elapsed().Seconds(), "tok/s")
}

// BenchmarkNoLabels pins the cost of a detector that is configured but has no
// enabled labels — it must not load the model or tokenize, so this should be
// nanoseconds. If it is not, the early return has regressed.
func BenchmarkNoLabels(b *testing.B) {
	d, err := New(Options{Dir: b.TempDir(), Labels: nil, MaxScanBytes: 4096})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	text := grow(prose, 4096)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Scan(context.Background(), text); err != nil {
			b.Fatalf("Scan: %v", err)
		}
	}
	_ = privacy.MinScanBytes
}
