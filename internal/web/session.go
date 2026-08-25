// Package web serves the browser dashboard: embedded assets, the session
// exchange that lets a browser authenticate without an x-api-key header, and
// nothing else.
//
// It imports internal/view and nothing below it — the same rule internal/tui
// follows — so presentation never reaches past the seam into account, metrics
// or proxy state.
package web

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// tokenBytes is the entropy behind one session token. 32 bytes is well past
// what is guessable and costs nothing.
const tokenBytes = 32

// Sessions is an in-memory session store with a sliding expiry.
//
// In memory, deliberately: a restart logs the operator out, which is the right
// trade for a single-operator tool and avoids persisting anything
// credential-adjacent to disk. Persisting sessions would mean a file that
// grants full control-plane access sitting beside the config.
type Sessions struct {
	mu  sync.Mutex
	ttl time.Duration
	// expires maps token to the instant it dies. Valid slides the value
	// forward, so continuous use never expires.
	expires map[string]time.Time
	// now is injectable so expiry is testable without sleeping. Production
	// never reassigns it; a test that sleeps for a 30-day TTL cannot exist,
	// and one that shortens the TTL to sleep through is a flake on a loaded
	// machine.
	now func() time.Time
}

func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{ttl: ttl, expires: map[string]time.Time{}, now: time.Now}
}

// Mint creates a session and returns its token. The error is from the system
// random source and is never ignored: falling back to a weaker token would
// turn an unreadable failure into a silent one.
func (s *Sessions) Mint() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires[tok] = s.now().Add(s.ttl)
	return tok, nil
}

// Valid reports whether a token is live, and slides its expiry when it is.
// An expired entry is reaped here rather than by a background goroutine: the
// store is small, every lookup already holds the lock, and a reaper goroutine
// would be a lifecycle to own for no benefit.
func (s *Sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.expires[token]
	if !ok {
		return false
	}
	now := s.now()
	if !now.Before(exp) {
		delete(s.expires, token)
		return false
	}
	s.expires[token] = now.Add(s.ttl)
	return true
}

func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expires, token)
}

func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.expires)
}
