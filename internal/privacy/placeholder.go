package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/nicko170/aiproxy/internal/config"
)

// Sentinel opens every placeholder. The streaming rewriter withholds bytes only
// when a chunk ends part-way through this prefix, so its rarity in ordinary
// prose and code is what keeps per-chunk flushing intact.
const Sentinel = "[[AIPROXY_"

// MaxPlaceholderBytes bounds every placeholder, and therefore bounds the
// streaming rewriter's holdback at MaxPlaceholderBytes-1. The longest actual
// form is 33 bytes — Sentinel (10) + label (<=8) + "_" (1) + 12 hex + "]]" (2)
// — and the slack is deliberate headroom so that adding a longer label is a
// test failure (TestLongestPlaceholderFitsTheBudget) rather than a silent
// stream bug.
const MaxPlaceholderBytes = 40

// keyBytes is the install key's length. 32 bytes of HMAC-SHA256 key is far more
// than the 32-48 bits of output actually used; the cost is nothing and it
// removes key length as a thing to think about.
const keyBytes = 32

// Label selects a placeholder's middle segment. It is included so the model can
// still reason about what was removed — "update the email address" is possible
// with [[AIPROXY_EMAIL_...]] and guesswork with an opaque blob.
type Label string

const (
	LabelSecret  Label = "SECRET"
	LabelEmail   Label = "EMAIL"
	LabelPhone   Label = "PHONE"
	LabelAddress Label = "ADDRESS"
	LabelPerson  Label = "PERSON"
	LabelURL     Label = "URL"
	LabelDate    Label = "DATE"
	LabelAccount Label = "ACCOUNT"
	LabelID      Label = "ID"
)

// AllLabels is every label, so a test can assert the longest placeholder fits
// MaxPlaceholderBytes without that list being maintained in two places.
func AllLabels() []Label {
	return []Label{
		LabelSecret, LabelEmail, LabelPhone, LabelAddress, LabelPerson,
		LabelURL, LabelDate, LabelAccount, LabelID,
	}
}

// placeholderRe matches a complete placeholder. The hex run is 8 to 12
// characters: 8 normally, widened to 12 on the collision path (see Table.Add).
var placeholderRe = regexp.MustCompile(`\[\[AIPROXY_[A-Z]+_[0-9a-f]{8,12}\]\]`)

// IsPlaceholder reports whether s is exactly one placeholder and nothing else.
func IsPlaceholder(s string) bool {
	loc := placeholderRe.FindStringIndex(s)
	return loc != nil && loc[0] == 0 && loc[1] == len(s)
}

// FindPlaceholder returns the leftmost placeholder's bounds in s.
func FindPlaceholder(s string) (int, int, bool) {
	loc := placeholderRe.FindStringIndex(s)
	if loc == nil {
		return 0, 0, false
	}
	return loc[0], loc[1], true
}

// Mint derives the placeholder for value under label, using hexLen hex
// characters of HMAC-SHA256(key, value).
//
// Two properties are load-bearing. The same value always yields the same
// placeholder, so a redacted prefix is byte-identical across turns and the
// provider's prompt cache still hits — a positional counter would renumber as
// content shifts and quietly multiply cost. And the hash is KEYED, because an
// AWS access key ID carries only ~20 bits of entropy after its fixed prefix and
// an unkeyed digest of one is brute-forceable from the placeholder alone.
//
// hexLen is 8 or 12; anything else is clamped into that range rather than
// producing a placeholder the recogniser would reject.
func Mint(key []byte, label Label, value string, hexLen int) string {
	if hexLen < 8 {
		hexLen = 8
	}
	if hexLen > 12 {
		hexLen = 12
	}
	mac := hmac.New(sha256.New, key)
	// The label is part of the input, so the same bytes classified two ways
	// cannot collide into one placeholder.
	mac.Write([]byte(label))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	sum := hex.EncodeToString(mac.Sum(nil))
	return Sentinel + string(label) + "_" + sum[:hexLen] + "]]"
}

// KeyPath is where the install key lives: beside the config, not inside it.
// config.json is rendered on the Settings screen and rewritten on every
// mutation, and a key belongs in neither.
func KeyPath() string { return filepath.Join(config.Dir(), "privacy.key") }

// LoadOrCreateKey reads the install key, generating it on first use.
//
// A rotated key changes every placeholder, which invalidates every cached
// prefix at the provider. That is survivable but expensive, so the key is
// written once and never touched again — and a file that exists but is the
// wrong length is an error rather than something to silently replace, because
// replacing it is the expensive outcome.
func LoadOrCreateKey(path string) ([]byte, error) {
	switch b, err := os.ReadFile(path); {
	case err == nil:
		if len(b) != keyBytes {
			return nil, fmt.Errorf("privacy: %s holds %d bytes, want %d; "+
				"delete it to regenerate, accepting that every placeholder changes", path, len(b), keyBytes)
		}
		return b, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("privacy: read install key: %w", err)
	}

	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("privacy: generate install key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("privacy: create config dir: %w", err)
	}
	// Written through a temp file and renamed, for the same reason
	// config.Store does it: a crash mid-write must not leave a truncated key
	// that the check above would then refuse on every subsequent start.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".privacy.key-*")
	if err != nil {
		return nil, fmt.Errorf("privacy: create install key: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, fmt.Errorf("privacy: install key: %w", err)
	}
	return key, nil
}
