package rules

import (
	"context"
	"regexp"
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

// The guard is only observable through a rule that WOULD match below the
// minimum, so this builds one rather than relying on Builtin() — every builtin
// rule needs at least 8 bytes to match anyway, so a test using them passes
// whether or not the guard exists.
func TestRulesSkipStringsBelowTheMinimum(t *testing.T) {
	short := []Rule{{
		Name: "test-short", Label: privacy.LabelSecret,
		Re: regexp.MustCompile(`(abc)`), Group: 1,
	}}
	d, err := New(short, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 3 bytes, under privacy.MinScanBytes: the guard must suppress it.
	got, err := d.Scan(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("scanned a %d-byte string: %+v", len("abc"), got)
	}
	// The same rule on a long-enough string MUST fire, or the test above would
	// pass for the wrong reason — a rule that never matches anything.
	got, err = d.Scan(context.Background(), "xxxxx abc xxxxx")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the rule does not match even above the minimum: %+v", got)
	}
}

func TestNewDenylistRejectsABadPattern(t *testing.T) {
	if _, err := NewDenylist([]string{"/(unclosed/"}); err == nil {
		t.Fatal("NewDenylist accepted an invalid regex; it must fail at construction")
	}
}

func TestNewDenylistRejectsEmptyPattern(t *testing.T) {
	if _, err := NewDenylist([]string{"//"}); err == nil {
		t.Fatal("NewDenylist accepted an empty regex pattern; it must fail at construction")
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
