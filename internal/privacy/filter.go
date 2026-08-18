package privacy

import (
	"context"
	"errors"
	"fmt"
	"time"
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

// ErrModelUnavailable reports that the NER model is not installed or failed to
// load, as distinct from a scan that went wrong. The control path maps it to 503
// and names the fix, because "install the model" and "this is a bug" are
// different instructions and a single 500 gives the operator neither.
var ErrModelUnavailable = errors.New("privacy: NER model unavailable")

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
	// ScanTimeout bounds the WHOLE request's scan. Zero leaves it unbounded,
	// which is only safe when no detector can block — the deterministic rules
	// are microseconds, the model is not.
	ScanTimeout time.Duration
	// ModelState reports the NER model's readiness, e.g. "loading" or "ready".
	// Nil until a model is wired in (Task 18), in which case Filter.ModelState
	// reports "off" — a filter running deterministic rules only is fully
	// functional, and reporting "absent" would imply something is missing
	// that is not.
	ModelState func() string
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
	scanTimeout   time.Duration
	stats         *stats
	modelState    func() string
}

func New(o Options) *Filter {
	f := &Filter{
		key:           o.Key,
		unresolved:    o.Unresolved,
		onScanFailure: o.OnScanFailure,
		scanTimeout:   o.ScanTimeout,
		stats:         newStats(),
		modelState:    o.ModelState,
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
//
// A scan is bounded by ScanTimeout when one is configured. That bound is on the
// AGGREGATE scan for the whole request, not on one string: maxScanBytes caps a
// single string's model time, but a fresh conversation carries dozens of unseen
// strings and the proxy has no other ceiling on the latency it adds here.
// Expiry surfaces as an ordinary scan failure and therefore obeys OnScanFailure,
// which is the point — a timeout is exactly the "could not scan" case.
//
// Two shapes are NOT failures and never fail closed:
//
//   - An empty body. proxyHandler is the router's catch-all, so every GET and
//     every unrecognised path reaches this with no body. There is provably
//     nothing to scan.
//   - A body that is not JSON: multipart uploads, form encoding, anything the
//     provider accepts that a JSON string walker cannot read. This is passed
//     through UNFILTERED rather than refused, following the precedent
//     anthropic.RewriteBody already sets for a body that is not a JSON object.
//     Refusing would mean the filter's default mode breaks file uploads
//     outright, and "not JSON" is a shape, not a malfunction. It is not free of
//     risk — a JSON body truncated by maxBodyBytes also lands here, and a
//     multipart part can carry a secret — so it is COUNTED: SentUnfiltered
//     rises and the UI shows it, which is what property 7 asks for.
func (f *Filter) Redact(ctx context.Context, body []byte) ([]byte, *Table, error) {
	if f.scanTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.scanTimeout)
		defer cancel()
	}
	table := NewTable(f.key)
	out, err := f.redactor.Redact(ctx, body, table)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotJSON):
		f.stats.sentUnfiltered()
		return body, NewTable(f.key), nil
	default:
		// Recorded on BOTH failure modes. Under closed the refusal is visible as
		// a 5xx, but under open the request goes upstream with no filtering at
		// all and this counter plus LastError are the only trace it leaves.
		f.stats.scanFailed(err)
		if f.onScanFailure == Open {
			f.stats.sentUnfiltered()
		}
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

// ModelState describes the NER model's readiness. Until a model is configured it
// is "off" — a filter running deterministic rules only is fully functional, and
// reporting "absent" would imply something is missing that is not.
func (f *Filter) ModelState() string {
	if f.modelState == nil {
		return "off"
	}
	return f.modelState()
}

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
