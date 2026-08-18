package rules

import (
	"context"
	"strings"
	"testing"
)

// The deterministic tier's cost, measured so it can be compared against the
// model tier's in internal/privacy/ner. The comparison is the whole point: it
// is what tells an operator what enabling privacy.ner actually buys and costs.
//
//	go test ./internal/privacy/rules -run '^$' -bench . -benchtime 1000x
//
// Unlike the model, these detectors have no byte cap — they scan the whole
// string however long it is — so this also establishes that removing the cap
// from Tier 1 was affordable.

const benchProse = "Please email Ada Lovelace at ada@example.org or call +44 20 7946 0958 " +
	"about the invoice for 12 Rue de Rivoli, Paris, dated 3 March 2026. " +
	"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE and DATABASE_PASSWORD=s3cr3t-hunter2-correct-horse. "

func benchGrow(seed string, n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(seed)
	}
	return b.String()[:n]
}

func BenchmarkRulesScan(b *testing.B) {
	d, err := New(Builtin(), nil)
	if err != nil {
		b.Fatal(err)
	}
	for _, size := range []int{512, 4096, 65536} {
		text := benchGrow(benchProse, size)
		b.Run(sizeName(size), func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := d.Scan(context.Background(), text); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N), "us/op")
		})
	}
}

// BenchmarkDenylistScan measures the operator denylist separately, since its
// cost scales with the number of entries an operator configures rather than
// with anything shipped.
func BenchmarkDenylistScan(b *testing.B) {
	entries := []string{
		"acme-prod.internal", "acme-staging.internal", "acme-data-eu",
		"/acme-[a-z]+-(eu|us|ap)/", "project-nightingale",
	}
	d, err := NewDenylist(entries)
	if err != nil {
		b.Fatal(err)
	}
	text := benchGrow(benchProse+"deploying to acme-prod.internal now. ", 4096)
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Scan(context.Background(), text); err != nil {
			b.Fatal(err)
		}
	}
}

func sizeName(n int) string {
	switch {
	case n >= 1024:
		return itoa(n/1024) + "KB"
	default:
		return itoa(n) + "B"
	}
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
