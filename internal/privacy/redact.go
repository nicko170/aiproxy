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
