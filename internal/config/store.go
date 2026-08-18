package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store is the only writer of the config file.
//
// Mutations go through Update, which re-reads from disk, applies the change, and
// writes atomically while holding a mutex. Serializing matters more than it
// looks: several accounts refreshing OAuth tokens at once is an ordinary event,
// and two concurrent read-modify-write cycles would drop one rotated refresh
// token, invalidating that account on the next start.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{path: path} }

// Load reads the config, filling in defaults for a missing file or absent
// fields. A freshly defaulted config is not written; the first Update does that.
func (s *Store) Load() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	// Unmarshal over the defaults so absent keys keep their default value.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", s.path, err)
	}
	if cfg.Accounts == nil {
		cfg.Accounts = []Account{}
	}
	if cfg.Listen.APIKey == "" {
		cfg.Listen.APIKey = newAPIKey()
	}
	// A hand-edited 0 (or a negative) would be handed to a ticker; correct it
	// here rather than making every consumer defend against it.
	if cfg.Update.CheckIntervalHours <= 0 {
		cfg.Update.CheckIntervalHours = Default().Update.CheckIntervalHours
	}
	return cfg, nil
}

// Update re-reads, applies fn, and persists. When fn returns an error nothing is
// written and that error propagates.
func (s *Store) Update(fn func(*Config) error) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg, err := s.loadLocked()
	if err != nil {
		return cfg, err
	}
	if err := fn(&cfg); err != nil {
		return cfg, err
	}
	if err := s.writeLocked(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// writeLocked writes to a temp file and renames, so a crash mid-write cannot
// truncate a config holding the only copy of a refresh token. Permissions are
// re-applied every write, not only on create: mode is honoured at creation time
// only, so a pre-existing file could otherwise stay world-readable.
func (s *Store) writeLocked(cfg Config) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("chmod config dir: %w", err)
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return os.Chmod(s.path, 0o600)
}
