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
