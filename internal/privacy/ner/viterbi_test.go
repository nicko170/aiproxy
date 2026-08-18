package ner

import (
	"os"
	"path/filepath"
	"testing"
)

// tinyLabels is a two-category BIOES set, small enough to reason about by hand.
var tinyLabels = []string{
	"O",
	"B-secret", "I-secret", "E-secret", "S-secret",
	"B-private_email", "I-private_email", "E-private_email", "S-private_email",
}

// allow builds a transition matrix from BIOES legality, which is what the
// calibration file amounts to at its shipped operating point: every bias is 0.0,
// so the matrix is legality alone.
func allow(labels []string) [][]float32 {
	return transitionMatrix(labels, nil)
}

func TestDecodeFindsASingleTokenSpan(t *testing.T) {
	// Three tokens: O, S-secret, O.
	logits := [][]float32{
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 5, 0, 0, 0, 0},
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	if len(got) != 1 {
		t.Fatalf("got %d spans, want 1: %+v", len(got), got)
	}
	if got[0].Start != 1 || got[0].End != 2 || got[0].Label != "secret" {
		t.Errorf("span = %+v", got[0])
	}
}

func TestDecodeFindsAMultiTokenSpan(t *testing.T) {
	// O, B-secret, I-secret, E-secret, O.
	logits := [][]float32{
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 5, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 5, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 5, 0, 0, 0, 0, 0},
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	if len(got) != 1 {
		t.Fatalf("got %d spans, want 1: %+v", len(got), got)
	}
	if got[0].Start != 1 || got[0].End != 4 {
		t.Errorf("span = %+v, want tokens [1,4)", got[0])
	}
}

// The constraint is the point of a constrained decode: a B not followed by I or E
// is not a valid tagging, and the decoder must find the best VALID path rather
// than the best per-token argmax.
func TestDecodeRejectsAnIllegalPath(t *testing.T) {
	// Per-token argmax would be B-secret then O, which BIOES forbids.
	logits := [][]float32{
		{0, 5, 0, 0, 4, 0, 0, 0, 0},
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	for _, s := range got {
		if s.Start == 0 && s.End == 1 && s.Label == "secret" {
			// S-secret at token 0 is legal and is the right answer; B-secret
			// alone would not be.
			return
		}
	}
	if len(got) != 0 {
		t.Fatalf("decoded an illegal tagging: %+v", got)
	}
}

func TestDecodeFindsTwoAdjacentSpans(t *testing.T) {
	logits := [][]float32{
		{0, 0, 0, 0, 5, 0, 0, 0, 0}, // S-secret
		{0, 0, 0, 0, 0, 0, 0, 0, 5}, // S-private_email
	}
	got := Decode(logits, tinyLabels, allow(tinyLabels))
	if len(got) != 2 {
		t.Fatalf("got %d spans, want 2: %+v", len(got), got)
	}
	if got[0].Label != "secret" || got[1].Label != "private_email" {
		t.Errorf("spans = %+v", got)
	}
}

func TestDecodeOnEmptyInput(t *testing.T) {
	if got := Decode(nil, tinyLabels, allow(tinyLabels)); len(got) != 0 {
		t.Errorf("Decode(nil) = %+v", got)
	}
}

func TestLegalTransition(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"O", "B-secret", true},
		{"O", "S-secret", true},
		{"O", "I-secret", false},
		{"O", "E-secret", false},
		{"B-secret", "I-secret", true},
		{"B-secret", "E-secret", true},
		{"B-secret", "O", false},
		{"B-secret", "I-private_email", false},
		{"I-secret", "E-secret", true},
		{"I-secret", "B-secret", false},
		{"E-secret", "O", true},
		{"E-secret", "B-private_email", true},
		{"S-secret", "O", true},
	} {
		if got := legalTransition(c.from, c.to); got != c.want {
			t.Errorf("legalTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// Every legal transition must fall into exactly one of the six bias classes the
// calibration file names; a legal pair with no class would silently ignore a
// tuned bias.
func TestEveryLegalTransitionHasABiasClass(t *testing.T) {
	for _, from := range tinyLabels {
		for _, to := range tinyLabels {
			if !legalTransition(from, to) {
				continue
			}
			if transitionClass(from, to) == "" {
				t.Errorf("legal transition %q -> %q has no bias class", from, to)
			}
		}
	}
}

// A bias must actually move the decode, or reading the file would be decoration.
func TestBiasesShiftTheDecode(t *testing.T) {
	// Token 1 slightly prefers S-secret over O. The span starts at token 1
	// rather than token 0 because token 0 has no predecessor and so feels no
	// transition bias at all.
	logits := [][]float32{
		{5, 0, 0, 0, 0, 0, 0, 0, 0},
		{4, 0, 0, 0, 5, 0, 0, 0, 0},
	}
	biased := transitionMatrix(tinyLabels, map[string]float64{
		"transition_bias_background_to_start": -50,
	})
	if got := Decode(logits, tinyLabels, allow(tinyLabels)); len(got) != 1 {
		t.Fatalf("unbiased decode found %d spans, want 1: %+v", len(got), got)
	}
	if got := Decode(logits, tinyLabels, biased); len(got) != 0 {
		t.Errorf("a -50 background_to_start bias did not suppress the span: %+v", got)
	}
}

func TestLoadTransitionsRejectsAMissingFile(t *testing.T) {
	if _, err := loadTransitions(filepath.Join(t.TempDir(), "nope.json"), tinyLabels); err == nil {
		t.Fatal("a missing calibration file must be an error, not silent zeros")
	}
}

func TestLoadTransitionsRejectsAFileWithNoDefaultOperatingPoint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cal.json")
	if err := os.WriteFile(p, []byte(`{"operating_points":{"strict":{"biases":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTransitions(p, tinyLabels); err == nil {
		t.Fatal("a calibration file without the default operating point must be an error")
	}
}

func TestLoadTransitionsReadsTheShippedSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cal.json")
	const shipped = `{"operating_points":{"default":{"biases":{
      "transition_bias_background_stay": 0.0,
      "transition_bias_background_to_start": 0.0,
      "transition_bias_end_to_background": 0.0,
      "transition_bias_end_to_start": 0.0,
      "transition_bias_inside_to_continue": 0.0,
      "transition_bias_inside_to_end": 0.0}}}}`
	if err := os.WriteFile(p, []byte(shipped), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadTransitions(p, tinyLabels)
	if err != nil {
		t.Fatal(err)
	}
	want := allow(tinyLabels)
	for i := range want {
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("trans[%d][%d] = %v, want %v", i, j, got[i][j], want[i][j])
			}
		}
	}
}
