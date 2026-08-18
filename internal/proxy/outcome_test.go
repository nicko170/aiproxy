package proxy

import (
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

// The enum is persisted. Renumbering an existing value silently rewrites the
// meaning of every row already written.
func TestOutcomeKindNumberingIsStable(t *testing.T) {
	want := map[provider.OutcomeKind]int{
		provider.OutcomeOK:                0,
		provider.OutcomeQuotaRejected:     1,
		provider.OutcomeThrottledWithHint: 2,
		provider.OutcomeThrottledNoHint:   3,
		provider.OutcomeCredentialStale:   4,
		provider.OutcomeCredentialRefused: 5,
		provider.OutcomeClientError:       6,
		provider.OutcomeServerError:       7,
		provider.OutcomeNoAccountReady:    8,
	}
	for kind, n := range want {
		if int(kind) != n {
			t.Errorf("%v = %d, want %d — existing values must never be renumbered", kind, int(kind), n)
		}
	}
	// New kinds append after the existing ones.
	if int(provider.OutcomeUpstreamError) <= 8 || int(provider.OutcomeAdmissionError) <= 8 {
		t.Error("new outcome kinds must be appended, not inserted")
	}
	if int(provider.OutcomeClientDisconnected) <= int(provider.OutcomeAdmissionError) {
		t.Error("OutcomeClientDisconnected must be appended after OutcomeAdmissionError, not inserted")
	}
	if int(provider.OutcomeBlocked) <= int(provider.OutcomeClientDisconnected) {
		t.Error("OutcomeBlocked must be appended after OutcomeClientDisconnected, not inserted")
	}
}

func TestOutcomeKindStringsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for k := provider.OutcomeOK; k <= provider.OutcomeBlocked; k++ {
		s := k.String()
		if s == "unknown" {
			t.Errorf("%d has no String() case", int(k))
		}
		if seen[s] {
			t.Errorf("duplicate outcome string %q", s)
		}
		seen[s] = true
	}
}
