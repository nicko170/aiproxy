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
