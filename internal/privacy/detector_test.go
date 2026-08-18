package privacy

import (
	"reflect"
	"testing"
)

func TestResolveSortsByPosition(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 20, End: 30, Label: LabelEmail, Rule: "b"},
		{Start: 0, End: 10, Label: LabelSecret, Rule: "a"},
	}})
	if len(got) != 2 || got[0].Start != 0 || got[1].Start != 20 {
		t.Fatalf("Resolve did not order by position: %+v", got)
	}
}

// The longer span wins: a connection string is more useful to redact whole than
// to redact the password inside it and leave the host and user behind.
func TestResolvePrefersTheLongerOverlappingSpan(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 10, End: 20, Label: LabelSecret, Rule: "password"},
		{Start: 0, End: 40, Label: LabelSecret, Rule: "connection-string"},
	}})
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if got[0].Rule != "connection-string" {
		t.Errorf("kept %q, want connection-string", got[0].Rule)
	}
}

// Two detectors reporting the identical span must always resolve the same way,
// or the same body redacts differently on different runs and the prompt cache
// stops hitting.
func TestResolveBreaksIdenticalSpansByRegistrationOrder(t *testing.T) {
	first := []Finding{{Start: 5, End: 15, Label: LabelSecret, Rule: "rules"}}
	second := []Finding{{Start: 5, End: 15, Label: LabelPerson, Rule: "ner"}}

	got := Resolve([][]Finding{first, second})
	if len(got) != 1 || got[0].Rule != "rules" {
		t.Fatalf("expected the first-registered detector to win, got %+v", got)
	}
	// Registering them the other way round must flip the winner — proving the
	// tiebreak is registration order and not something incidental like the
	// label's or rule's spelling.
	got = Resolve([][]Finding{second, first})
	if len(got) != 1 || got[0].Rule != "ner" {
		t.Fatalf("registration order is not the tiebreak, got %+v", got)
	}
}

func TestResolveKeepsAdjacentNonOverlappingSpans(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 0, End: 10, Label: LabelSecret, Rule: "a"},
		{Start: 10, End: 20, Label: LabelEmail, Rule: "b"},
	}})
	if len(got) != 2 {
		t.Fatalf("adjacent spans must both survive: %+v", got)
	}
}

func TestResolveDropsEmptyAndInvertedSpans(t *testing.T) {
	got := Resolve([][]Finding{{
		{Start: 5, End: 5, Rule: "empty"},
		{Start: 9, End: 4, Rule: "inverted"},
		{Start: 0, End: 3, Rule: "good"},
	}})
	want := []Finding{{Start: 0, End: 3, Rule: "good"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %+v, want %+v", got, want)
	}
}

func TestResolveOnNothing(t *testing.T) {
	if got := Resolve(nil); len(got) != 0 {
		t.Errorf("Resolve(nil) = %+v", got)
	}
	if got := Resolve([][]Finding{{}, {}}); len(got) != 0 {
		t.Errorf("Resolve of empty groups = %+v", got)
	}
}

// TestResolveDeterminismViaRuleTiebreak verifies that when two findings in the
// same detector group have identical span (Start/End) but different Rule, the
// comparator's Rule tiebreak produces a deterministic result independent of input
// order. One detector CAN report the same span twice — e.g., openai-key and
// anthropic-key both match "sk-ant-..." — and the kept finding must be
// deterministic by rule name, not by input position.
func TestResolveDeterminismViaRuleTiebreak(t *testing.T) {
	// Two findings with identical span, supplied in reverse alphabetical order.
	// The comparator must pick by Rule, not by input order.
	sameGroup := []Finding{
		{Start: 5, End: 15, Label: LabelSecret, Rule: "b-rule"},
		{Start: 5, End: 15, Label: LabelSecret, Rule: "a-rule"},
	}

	got := Resolve([][]Finding{sameGroup})
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	// The comparator's Rule tiebreak orders by rule name, so "a-rule" < "b-rule".
	if got[0].Rule != "a-rule" {
		t.Errorf("expected Rule 'a-rule' (deterministic tiebreak), got %q", got[0].Rule)
	}
}
