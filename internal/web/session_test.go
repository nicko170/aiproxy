package web

import (
	"testing"
	"time"
)

func TestMintedSessionIsValid(t *testing.T) {
	s := NewSessions(time.Hour)
	tok, err := s.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok == "" {
		t.Fatal("Mint returned an empty token")
	}
	if !s.Valid(tok) {
		t.Error("a freshly minted token is not valid")
	}
}

// Tokens must be unguessable and must not collide. A predictable session token
// is the whole authentication system defeated, so this asserts both randomness
// (no two mints agree) and length (enough entropy to be worth having).
func TestTokensAreUniqueAndLongEnough(t *testing.T) {
	s := NewSessions(time.Hour)
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		tok, err := s.Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if seen[tok] {
			t.Fatalf("Mint produced a duplicate token on iteration %d", i)
		}
		seen[tok] = true
		if len(tok) < 32 {
			t.Fatalf("token %q is %d chars, want at least 32", tok, len(tok))
		}
	}
}

func TestUnknownTokenIsNotValid(t *testing.T) {
	s := NewSessions(time.Hour)
	if s.Valid("not-a-token") {
		t.Error("an unminted token was accepted")
	}
	if s.Valid("") {
		t.Error("the empty token was accepted")
	}
}

func TestDeleteRevokesImmediately(t *testing.T) {
	s := NewSessions(time.Hour)
	tok, _ := s.Mint()
	s.Delete(tok)
	if s.Valid(tok) {
		t.Error("a deleted token is still valid")
	}
	if n := s.Count(); n != 0 {
		t.Errorf("Count() = %d after delete, want 0", n)
	}
}

// Expiry is driven by an injectable clock, not by sleeping: a test that sleeps
// for the real TTL cannot assert a 30-day expiry, and one that sleeps for a
// short TTL is a flake waiting for a loaded CI box.
func TestExpiredSessionIsRejectedAndReaped(t *testing.T) {
	s := NewSessions(time.Hour)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }

	tok, _ := s.Mint()
	now = now.Add(59 * time.Minute)
	if !s.Valid(tok) {
		t.Fatal("token expired early")
	}
	now = now.Add(2 * time.Hour)
	if s.Valid(tok) {
		t.Error("expired token was accepted")
	}
	if n := s.Count(); n != 0 {
		t.Errorf("Count() = %d, want the expired entry reaped", n)
	}
}

// The expiry slides on use, so an operator with the dashboard open all week is
// not logged out mid-session.
func TestValidSlidesTheExpiry(t *testing.T) {
	s := NewSessions(time.Hour)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }

	tok, _ := s.Mint()
	for i := 0; i < 10; i++ {
		now = now.Add(50 * time.Minute)
		if !s.Valid(tok) {
			t.Fatalf("token expired after %d slides despite continuous use", i)
		}
	}
}
