package privacy

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// feed pushes s through a fresh rewriter in the given chunk sizes and returns
// the complete output. Any split must produce the same bytes — that is the
// property this whole file exists to establish.
func feed(t *testing.T, tab *Table, s string, chunks []int) string {
	t.Helper()
	w := newRewriter(tab, Passthrough, nil)
	var b strings.Builder
	i := 0
	for _, n := range chunks {
		if i >= len(s) {
			break
		}
		j := i + n
		if j > len(s) {
			j = len(s)
		}
		out, err := w.Write(s[i:j])
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		b.WriteString(out)
		i = j
	}
	if i < len(s) {
		out, err := w.Write(s[i:])
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		b.WriteString(out)
	}
	tail, err := w.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	b.WriteString(tail)
	return b.String()
}

func tableWith(t *testing.T, label Label, value string) (*Table, string) {
	t.Helper()
	tab := NewTable(testKey)
	p, err := tab.Add(label, value)
	if err != nil {
		t.Fatal(err)
	}
	return tab, p
}

func TestRewriterSubstitutesInOneWrite(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	in := "your key " + p + " is stale"
	if got := feed(t, tab, in, []int{len(in)}); got != "your key AKIAIOSFODNN7EXAMPLE is stale" {
		t.Errorf("got %q", got)
	}
}

// Property 2, exhaustively: split the stream at EVERY byte offset and assert the
// output never changes. A placeholder straddling the split is the normal case,
// not the exotic one — token boundaries do not respect placeholders.
func TestRewriterIsInvariantAcrossEverySingleSplit(t *testing.T) {
	tab, p := tableWith(t, LabelSecret, "AKIAIOSFODNN7EXAMPLE")
	in := "prefix " + p + " middle " + p + " suffix"
	want := strings.ReplaceAll(in, p, "AKIAIOSFODNN7EXAMPLE")

	for split := 0; split <= len(in); split++ {
		got := feed(t, tab, in, []int{split})
		if got != want {
			t.Fatalf("split at %d produced %q, want %q", split, got, want)
		}
	}
}

// One byte at a time is the worst case for the holdback and the cheapest way to
// catch an off-by-one in the pending buffer.
func TestRewriterIsInvariantByteByByte(t *testing.T) {
	tab, p := tableWith(t, LabelPerson, "Ada Lovelace")
	in := "hello " + p + ", welcome"
	want := "hello Ada Lovelace, welcome"

	ones := make([]int, len(in))
	for i := range ones {
		ones[i] = 1
	}
	if got := feed(t, tab, in, ones); got != want {
		t.Errorf("byte-by-byte gave %q, want %q", got, want)
	}
}

func TestRewriterIsInvariantUnderRandomSplits(t *testing.T) {
	tab, p := tableWith(t, LabelEmail, "ada@example.com")
	in := "a " + p + " b " + p + " c " + p + " d"
	want := strings.ReplaceAll(in, p, "ada@example.com")

	rng := rand.New(rand.NewSource(1)) // fixed seed: a failure must reproduce
	for iter := 0; iter < 500; iter++ {
		var chunks []int
		for total := 0; total < len(in); {
			n := 1 + rng.Intn(7)
			chunks = append(chunks, n)
			total += n
		}
		if got := feed(t, tab, in, chunks); got != want {
			t.Fatalf("chunks %v produced %q, want %q", chunks, got, want)
		}
	}
}

// Property 3: text with no sentinel comes out byte-for-byte, and nothing is
// withheld unless a chunk actually ends mid-sentinel.
func TestRewriterWithholdsNothingWithoutASentinel(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	const in = "ordinary prose and some code: arr[0] = f(x); // fine"
	out, err := w.Write(in)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("output %q != input %q", out, in)
	}
	if n := w.Pending(); n != 0 {
		t.Errorf("withheld %d bytes with no sentinel present; per-chunk flushing must be untouched", n)
	}
}

// A chunk ending in "[" holds back exactly one byte — the minimum needed to
// stay correct, and the only case where ordinary text is delayed at all.
func TestRewriterHoldsBackOnlyAPartialSentinel(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	out, err := w.Write("index[")
	if err != nil {
		t.Fatal(err)
	}
	if out != "index" {
		t.Errorf("emitted %q, want index", out)
	}
	if w.Pending() != 1 {
		t.Errorf("withheld %d bytes, want 1", w.Pending())
	}
	// The next write resolves it: "[0]" cannot begin a placeholder.
	out, err = w.Write("0] = 1")
	if err != nil {
		t.Fatal(err)
	}
	if out != "[0] = 1" {
		t.Errorf("emitted %q, want [0] = 1", out)
	}
	if w.Pending() != 0 {
		t.Errorf("still withholding %d bytes", w.Pending())
	}
}

// Holdback is bounded, so a stream of open brackets cannot grow the buffer.
func TestRewriterHoldbackIsBounded(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	for i := 0; i < 1000; i++ {
		if _, err := w.Write("[[AIPROXY_"); err != nil {
			t.Fatal(err)
		}
		if n := w.Pending(); n >= MaxPlaceholderBytes {
			t.Fatalf("pending grew to %d bytes, budget is %d", n, MaxPlaceholderBytes)
		}
	}
}

// An unresolvable placeholder passes through verbatim and is reported. Guessing
// a value would write something wrong into the operator's files; a visibly
// wrong string is recoverable.
func TestRewriterPassesThroughAnUnresolvedPlaceholder(t *testing.T) {
	tab := NewTable(testKey)
	var seen []string
	w := newRewriter(tab, Passthrough, func(p string) { seen = append(seen, p) })

	const orphan = "[[AIPROXY_SECRET_deadbeef]]"
	out, err := w.Write("before " + orphan + " after")
	if err != nil {
		t.Fatal(err)
	}
	tail, err := w.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if got := out + tail; got != "before "+orphan+" after" {
		t.Errorf("got %q, want the placeholder verbatim", got)
	}
	if len(seen) != 1 || seen[0] != orphan {
		t.Errorf("onUnresolved saw %v, want one report of %q", seen, orphan)
	}
}

func TestRewriterErrorsOnUnresolvedWhenConfigured(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, ErrorOut, nil)
	_, err := w.Write("before [[AIPROXY_SECRET_deadbeef]] after")
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
}

// A partial placeholder at end of stream is emitted, not swallowed. Losing it
// would silently truncate the model's answer.
func TestRewriterFlushEmitsAPartialTail(t *testing.T) {
	tab := NewTable(testKey)
	w := newRewriter(tab, Passthrough, nil)
	out, err := w.Write("ends with [[AIPROXY_SEC")
	if err != nil {
		t.Fatal(err)
	}
	tail, err := w.Flush()
	if err != nil {
		t.Fatal(err)
	}
	if got := out + tail; got != "ends with [[AIPROXY_SEC" {
		t.Errorf("got %q; a partial placeholder must survive Flush", got)
	}
}

// Restoring a value that itself contains a sentinel-looking prefix must not
// re-enter the matcher: substituted text is emitted, never rescanned.
func TestRewriterDoesNotRescanSubstitutedText(t *testing.T) {
	tab := NewTable(testKey)
	// A pathological plaintext that looks like another placeholder.
	p, err := tab.Add(LabelSecret, "[[AIPROXY_SECRET_00000000]]")
	if err != nil {
		t.Fatal(err)
	}
	got := feed(t, tab, "x "+p+" y", []int{3, 5, 100})
	if got != "x [[AIPROXY_SECRET_00000000]] y" {
		t.Errorf("got %q; substituted text must be emitted as-is", got)
	}
}

// Multi-byte text must not be split mid-rune by the holdback logic. The
// rewriter works on bytes, so this asserts the bytes reassemble.
func TestRewriterPreservesMultiByteText(t *testing.T) {
	tab, p := tableWith(t, LabelPerson, "Ada")
	in := "héllo 😀 " + p + " wörld"
	want := "héllo 😀 Ada wörld"
	ones := make([]int, len(in))
	for i := range ones {
		ones[i] = 1
	}
	if got := feed(t, tab, in, ones); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// FuzzRewriterRoundTrip is property 1 at the rewriter level: for arbitrary
// surrounding text and an arbitrary chunking, redact-then-restore is the
// identity. The fuzzer supplies both the text and the split sizes, so it
// explores boundary placements no hand-written case would think of.
func FuzzRewriterRoundTrip(f *testing.F) {
	f.Add("hello world", "secret-value", uint8(3))
	f.Add("", "x", uint8(1))
	f.Add("[[AIPROXY_", "v", uint8(2))
	f.Add("a[[b[[AIPROXY_SEC", "v", uint8(1))
	f.Fuzz(func(t *testing.T, surround, secret string, chunk uint8) {
		if secret == "" || chunk == 0 {
			return
		}
		tab := NewTable(testKey)
		p, err := tab.Add(LabelSecret, secret)
		if err != nil {
			return
		}
		in := surround + p + surround
		want := surround + secret + surround

		w := newRewriter(tab, Passthrough, nil)
		var b strings.Builder
		for i := 0; i < len(in); i += int(chunk) {
			j := i + int(chunk)
			if j > len(in) {
				j = len(in)
			}
			out, err := w.Write(in[i:j])
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			b.WriteString(out)
		}
		tail, err := w.Flush()
		if err != nil {
			t.Fatalf("Flush: %v", err)
		}
		b.WriteString(tail)

		// surround may itself contain a placeholder-looking run that resolves,
		// so compare against the same transformation applied whole.
		ref := newRewriter(tab, Passthrough, nil)
		whole, err := ref.Write(in)
		if err != nil {
			t.Fatal(err)
		}
		refTail, _ := ref.Flush()
		if got := b.String(); got != whole+refTail {
			t.Fatalf("chunked output differs from whole-input output:\n chunked %q\n whole   %q", got, whole+refTail)
		}
		if !strings.Contains(whole+refTail, secret) && strings.Contains(want, secret) {
			t.Fatalf("the secret was not restored: %q", whole+refTail)
		}
	})
}
