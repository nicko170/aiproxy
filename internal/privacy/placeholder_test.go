package privacy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMintIsStableAndKeyed(t *testing.T) {
	k1 := []byte("0123456789abcdef0123456789abcdef")
	k2 := []byte("fedcba9876543210fedcba9876543210")

	a := Mint(k1, LabelSecret, "AKIAIOSFODNN7EXAMPLE", 8)
	b := Mint(k1, LabelSecret, "AKIAIOSFODNN7EXAMPLE", 8)
	if a != b {
		t.Errorf("minting is not stable: %q vs %q — a prompt cache prefix depends on this", a, b)
	}
	if c := Mint(k2, LabelSecret, "AKIAIOSFODNN7EXAMPLE", 8); c == a {
		t.Error("the install key does not affect the placeholder; it must, or a known-format value is brute-forceable")
	}
	if d := Mint(k1, LabelSecret, "AKIAIOSFODNN7DIFFERENT", 8); d == a {
		t.Error("two different values minted the same placeholder")
	}
}

func TestMintFormat(t *testing.T) {
	got := Mint([]byte("0123456789abcdef0123456789abcdef"), LabelEmail, "a@b.example", 8)
	if !strings.HasPrefix(got, Sentinel) {
		t.Errorf("%q does not start with the sentinel %q", got, Sentinel)
	}
	if !strings.HasPrefix(got, "[[AIPROXY_EMAIL_") || !strings.HasSuffix(got, "]]") {
		t.Errorf("format = %q", got)
	}
	if !IsPlaceholder(got) {
		t.Errorf("%q is not recognised by IsPlaceholder", got)
	}
}

// The longest form must fit the constant the streaming rewriter budgets its
// holdback against. If this ever fails, a placeholder can cross more bytes than
// the rewriter withholds and restoration silently misses it.
func TestLongestPlaceholderFitsTheBudget(t *testing.T) {
	longest := 0
	for _, l := range AllLabels() {
		got := Mint([]byte("0123456789abcdef0123456789abcdef"), l, "x", 12)
		if len(got) > longest {
			longest = len(got)
		}
	}
	if longest > MaxPlaceholderBytes {
		t.Fatalf("longest placeholder is %d bytes, budget is %d", longest, MaxPlaceholderBytes)
	}
	t.Logf("longest placeholder is %d bytes of a %d budget", longest, MaxPlaceholderBytes)
}

func TestIsPlaceholderRejectsNearMisses(t *testing.T) {
	for _, s := range []string{
		"", "[[AIPROXY_]]", "[[AIPROXY_SECRET_]]",
		"[[AIPROXY_SECRET_XYZ12345]]",       // hex only
		"[[AIPROXY_secret_a1b2c3d4]]",       // label is uppercase
		"[[AIPROXY_SECRET_a1b2c3]]",         // too short
		"[[AIPROXY_SECRET_a1b2c3d4e5f6a7]]", // too long
		"[AIPROXY_SECRET_a1b2c3d4]",         // single brackets
		"[[AIPROXY_SECRET_a1b2c3d4]",        // unbalanced
	} {
		if IsPlaceholder(s) {
			t.Errorf("IsPlaceholder(%q) = true, want false", s)
		}
	}
}

func TestFindPlaceholderReturnsLeftmostMatch(t *testing.T) {
	s := "before [[AIPROXY_SECRET_a1b2c3d4]] middle [[AIPROXY_EMAIL_00112233]] after"
	start, end, ok := FindPlaceholder(s)
	if !ok {
		t.Fatal("FindPlaceholder found nothing")
	}
	if got := s[start:end]; got != "[[AIPROXY_SECRET_a1b2c3d4]]" {
		t.Errorf("found %q, want the leftmost placeholder", got)
	}
	if _, _, ok := FindPlaceholder("nothing here at all"); ok {
		t.Error("FindPlaceholder matched a string with no placeholder")
	}
}

func TestLoadOrCreateKeyIsStableAndPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.key")

	first, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if len(first) != 32 {
		t.Errorf("key is %d bytes, want 32", len(first))
	}

	second, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Error("the key changed between calls; every placeholder in every cached prefix would change with it")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode is %v, want 0600", perm)
	}
}

func TestLoadOrCreateKeyRejectsAShortKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "privacy.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("a truncated key file was accepted; a weak key defeats the point of keying the hash")
	}
}
