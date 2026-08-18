package rules

import "testing"

func TestShannonBitsOrdersInputsSensibly(t *testing.T) {
	// A repeated character carries no information.
	if got := ShannonBits("aaaaaaaaaaaa"); got != 0 {
		t.Errorf("ShannonBits(repeated) = %v, want 0", got)
	}
	// An English word is low; a random base64 run is high. The exact values
	// matter less than the ordering and the thresholds around them.
	word := ShannonBits("changeme")
	random := ShannonBits("Zm9vYmFyYmF6cXV1eDEyMzQ1Njc4OTA")
	if !(word < random) {
		t.Errorf("expected changeme (%v) below a random run (%v)", word, random)
	}
	if random < 4.0 {
		t.Errorf("a random base64 run scored %v; the assigned-credential rule's 3.0 floor would never fire", random)
	}
	if word >= 3.0 {
		t.Errorf("changeme scored %v, at or above the 3.0 floor — it would be redacted", word)
	}
}

func TestShannonBitsOnEmptyAndSingle(t *testing.T) {
	if got := ShannonBits(""); got != 0 {
		t.Errorf("ShannonBits(\"\") = %v, want 0", got)
	}
	if got := ShannonBits("a"); got != 0 {
		t.Errorf("ShannonBits(\"a\") = %v, want 0", got)
	}
}

// Entropy is computed over bytes, so multi-byte input must not panic or produce
// a nonsensical value.
func TestShannonBitsHandlesMultiByteInput(t *testing.T) {
	if got := ShannonBits("héllo wörld 😀"); got <= 0 {
		t.Errorf("ShannonBits(multibyte) = %v, want > 0", got)
	}
}
