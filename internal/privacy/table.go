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
			// Unreachable through the public API, and kept as a guard rather
			// than an expectation. The only way to reach a taken placeholder
			// already holding OUR value would be a different (label, value) pair
			// minting the same string — impossible, because the label is written
			// literally into the placeholder text, so a differing label differs
			// in the placeholder too, and an identical (label, value) pair is
			// already served by byValue above. Only forceForTest can get here.
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

// Len is the number of distinct placeholders minted.
//
// Nil-safe, and deliberately so: the relay uses it to decide whether to arm a
// restorer at all, and "no filter configured" (nil) and "filter ran, found
// nothing" (empty) must take the same branch — neither can resolve anything, and
// a table is complete by construction once redaction finishes.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byPlaceholder)
}

// forceForTest seeds a mapping directly, so the collision path can be exercised
// without searching for a 12-hex collision that does not naturally occur.
func (t *Table) forceForTest(placeholder, value string) {
	t.byPlaceholder[placeholder] = value
}

// labels is every label minted into this table, one entry per distinct value, so
// Filter can count redactions per label without exposing the mapping itself.
func (t *Table) labels() []Label {
	out := make([]Label, 0, len(t.byValue))
	for vk := range t.byValue {
		out = append(out, vk.label)
	}
	return out
}
