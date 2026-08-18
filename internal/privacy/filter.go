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
