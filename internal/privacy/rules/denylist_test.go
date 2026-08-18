package rules

import (
	"context"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/privacy"
)

func denyScan(t *testing.T, entries []string, text string) []privacy.Finding {
	t.Helper()
	d, err := NewDenylist(entries)
	if err != nil {
		t.Fatalf("NewDenylist: %v", err)
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return got
}

func TestDenylistMatchesLiterals(t *testing.T) {
	text := "deploying to acme-prod.internal from ci"
	got := denyScan(t, []string{"acme-prod.internal"}, text)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if span := text[got[0].Start:got[0].End]; span != "acme-prod.internal" {
		t.Errorf("span = %q", span)
	}
	if got[0].Label != privacy.LabelID {
		t.Errorf("Label = %q, want ID", got[0].Label)
	}
}

func TestDenylistMatchesEveryOccurrence(t *testing.T) {
	text := "acme-prod talks to acme-prod over tls"
	if got := denyScan(t, []string{"acme-prod"}, text); len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
}

func TestDenylistSupportsRegexEntries(t *testing.T) {
	text := "buckets: acme-data-eu, acme-data-us, other-data"
	got := denyScan(t, []string{`/acme-data-[a-z]{2}/`}, text)
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if !strings.HasPrefix(text[f.Start:f.End], "acme-data-") {
			t.Errorf("span = %q", text[f.Start:f.End])
		}
	}
}

// The denylist is deliberately exempt from MinScanBytes: an operator asking for
// "acme" to be redacted must get it, and four bytes is a plausible codename.
func TestDenylistMatchesShortEntries(t *testing.T) {
	text := "project acme ships friday"
	if got := denyScan(t, []string{"acme"}, text); len(got) != 1 {
		t.Fatalf("a 4-byte denylist entry did not match: %+v", got)
	}
}

// ...whereas the rule detector skips short strings entirely, because no
// credential format fits in one and most strings in a request are protocol
// values.
func TestRulesSkipStringsBelowTheMinimum(t *testing.T) {
	d, err := New(Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Shorter than privacy.MinScanBytes, and contrived to match nothing anyway;
	// the assertion is that Scan returns early rather than that it finds nothing.
	got, err := d.Scan(context.Background(), "sk-ant-")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("findings on a sub-minimum string: %+v", got)
	}
}

func TestNewDenylistRejectsABadPattern(t *testing.T) {
	if _, err := NewDenylist([]string{"/(unclosed/"}); err == nil {
		t.Fatal("NewDenylist accepted an invalid regex; it must fail at construction")
	}
}

func TestEmptyDenylistFindsNothing(t *testing.T) {
	if got := denyScan(t, nil, "anything at all"); len(got) != 0 {
		t.Errorf("empty denylist produced findings: %+v", got)
	}
}

func TestDenylistNameIsStable(t *testing.T) {
	d, _ := NewDenylist(nil)
	if d.Name() != "denylist" {
		t.Errorf("Name = %q, want denylist", d.Name())
	}
}
