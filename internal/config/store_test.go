package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "config.json"))

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retry.BudgetMS != 10000 {
		t.Errorf("BudgetMS = %d, want 10000", cfg.Retry.BudgetMS)
	}
	if cfg.Retry.InlineAbsorbMaxMS != 5000 {
		t.Errorf("InlineAbsorbMaxMS = %d, want 5000", cfg.Retry.InlineAbsorbMaxMS)
	}
	// The second clock. It is separate from the budget on purpose: it bounds the
	// upstream's own time-to-first-token, which the budget must not.
	if cfg.Retry.HeaderTimeoutMS != 60000 {
		t.Errorf("HeaderTimeoutMS = %d, want 60000", cfg.Retry.HeaderTimeoutMS)
	}
	if cfg.QuotaProbe.IntervalSeconds != 300 {
		t.Errorf("probe interval = %d, want 300", cfg.QuotaProbe.IntervalSeconds)
	}
	if cfg.Listen.APIKey == "" {
		t.Error("a proxy API key should be generated on first load")
	}
}

func TestUpdatePersistsAndEnforcesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := NewStore(path)

	if _, err := s.Update(func(c *Config) error {
		c.Accounts = append(c.Accounts, Account{
			ID:         "acct-1",
			Provider:   "anthropic",
			Label:      "a@example.com",
			Credential: provider.Credential{Type: provider.CredentialOAuth, AccessToken: "tok"},
		})
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reloaded, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Accounts) != 1 || reloaded.Accounts[0].ID != "acct-1" {
		t.Fatalf("accounts not persisted: %+v", reloaded.Accounts)
	}
	if reloaded.Accounts[0].Credential.AccessToken != "tok" {
		t.Error("credential not persisted")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perm = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 700", perm)
	}
}

// A config written before retry.headerTimeoutMs existed must pick up the default
// rather than zero. Zero would mean "no per-attempt header timeout at all" to any
// reader that took the number literally, and the whole point of the field is that
// it is a chosen, finite bound.
func TestLoadFillsHeaderTimeoutForAPreExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// The retry block exactly as it was serialized before this field was added.
	old := `{"retry":{"budgetMs":3000,"inlineAbsorbMaxMs":5000,"bodyIdleMs":120000}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retry.BudgetMS != 3000 {
		t.Errorf("BudgetMS = %d, want the on-disk 3000", cfg.Retry.BudgetMS)
	}
	if cfg.Retry.HeaderTimeoutMS != 60000 {
		t.Errorf("HeaderTimeoutMS = %d, want the 60000 default", cfg.Retry.HeaderTimeoutMS)
	}
}

// Concurrent read-modify-write must not lose writes. Losing one here means
// losing a rotated refresh token, which invalidates an account on next start.
func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "config.json"))

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Update(func(c *Config) error {
				c.Accounts = append(c.Accounts, Account{ID: string(rune('a' + i))})
				return nil
			})
		}(i)
	}
	wg.Wait()

	cfg, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != n {
		t.Errorf("got %d accounts, want %d — a concurrent write was lost", len(cfg.Accounts), n)
	}
}

func TestUpdateRollsBackOnCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	s := NewStore(path)
	s.Update(func(c *Config) error { return nil }) // materialize the file

	wantErr := os.ErrInvalid
	if _, err := s.Update(func(c *Config) error {
		c.Accounts = append(c.Accounts, Account{ID: "ghost"})
		return wantErr
	}); err == nil {
		t.Fatal("expected the callback error to propagate")
	}

	cfg, _ := s.Load()
	if len(cfg.Accounts) != 0 {
		t.Error("a failed update must not be written")
	}
}
