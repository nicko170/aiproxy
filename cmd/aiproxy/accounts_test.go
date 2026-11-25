package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// I2: logging into an account that is already configured must update it in
// place, not append a duplicate that shares its AccountUUID and ends up both
// rotating and being probed. This drives the real hook onLoginSuccess builds
// (the same one buildHandler wires onto Anthropic.OnLoginSuccess) over a
// real config.Store and account.Manager, covering both branches: a genuinely
// new account is added, and a re-login for an existing one updates it in
// place and leaves the account count unchanged.
func TestOnLoginSuccessAddsANewAccountButDedupesARelogin(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	mgr := account.New(nil, map[string]provider.Provider{}, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})

	hook := onLoginSuccess(store, mgr, "anthropic", slog.Default())

	profile1 := provider.Profile{
		Email: "a@example.com", OrgName: "Acme", AccountUUID: "uuid-1", OrgUUID: "org-1", Plan: "pro",
	}
	cred1 := provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at-1", RefreshToken: "rt-1"}
	if err := hook(context.Background(), cred1, profile1); err != nil {
		t.Fatalf("first login: %v", err)
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts after first login = %d, want 1", len(cfg.Accounts))
	}
	if len(mgr.All()) != 1 {
		t.Fatalf("live accounts after first login = %d, want 1", len(mgr.All()))
	}
	firstID := cfg.Accounts[0].ID
	if cfg.Accounts[0].Credential.AccessToken != "at-1" {
		t.Errorf("persisted credential = %+v, want the first login's token", cfg.Accounts[0].Credential)
	}

	// A second, unrelated login (a different account entirely) must still
	// simply append.
	profile2 := provider.Profile{Email: "b@example.com", OrgName: "Beta Inc", AccountUUID: "uuid-2"}
	cred2 := provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at-2"}
	if err := hook(context.Background(), cred2, profile2); err != nil {
		t.Fatalf("second (different) login: %v", err)
	}
	cfg, err = store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts after second (different) login = %d, want 2", len(cfg.Accounts))
	}
	if len(mgr.All()) != 2 {
		t.Fatalf("live accounts after second (different) login = %d, want 2", len(mgr.All()))
	}

	// A re-login for the FIRST account (same AccountUUID, a fresh token —
	// e.g. its refresh token had expired) must update that entry in place:
	// same ID, new credential, no third entry, no second mgr.Add for it.
	reloginCred := provider.Credential{Type: provider.CredentialOAuth, AccessToken: "at-1-refreshed", RefreshToken: "rt-1-refreshed"}
	reloginProfile := provider.Profile{Email: "a@example.com", OrgName: "Acme", AccountUUID: "uuid-1", OrgUUID: "org-1", Plan: "pro"}
	if err := hook(context.Background(), reloginCred, reloginProfile); err != nil {
		t.Fatalf("relogin: %v", err)
	}

	cfg, err = store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts after relogin = %d, want still 2 (deduped, not appended)", len(cfg.Accounts))
	}
	if len(mgr.All()) != 2 {
		t.Fatalf("live accounts after relogin = %d, want still 2 (no second mgr.Add)", len(mgr.All()))
	}

	var got *config.Account
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Identity.AccountUUID == "uuid-1" {
			got = &cfg.Accounts[i]
		}
	}
	if got == nil {
		t.Fatal("account with uuid-1 not found after relogin")
	}
	if got.ID != firstID {
		t.Errorf("relogin changed the account's ID from %q to %q, want it stable", firstID, got.ID)
	}
	if got.Credential.AccessToken != "at-1-refreshed" {
		t.Errorf("relogin credential = %+v, want the fresh token persisted in place", got.Credential)
	}

	// The live Manager entry must carry the fresh credential too, not just
	// the on-disk copy: a re-login exists specifically to fix an account
	// whose in-memory credential has stopped working, and if only the store
	// were updated the fix would silently not take effect until a restart.
	live, ok := mgr.Get(firstID)
	if !ok {
		t.Fatalf("live account %q not found after relogin", firstID)
	}
	if live.Credential.AccessToken != "at-1-refreshed" {
		t.Errorf("live credential = %+v, want the fresh token applied to the live Manager entry", live.Credential)
	}
}

func TestMergeLoginAccountDedupesOnUUIDThenLabel(t *testing.T) {
	t.Run("dedupes on account uuid", func(t *testing.T) {
		cfg := &config.Config{Accounts: []config.Account{
			{ID: "existing", Identity: config.Identity{AccountUUID: "u1"}, Label: "old label"},
		}}
		acc := config.Account{ID: "fresh", Identity: config.Identity{AccountUUID: "u1"}, Label: "new label",
			Credential: provider.Credential{AccessToken: "new-token"}}

		_, isNew := mergeLoginAccount(cfg, acc)
		if isNew {
			t.Error("want isNew = false: the uuid already exists")
		}
		if len(cfg.Accounts) != 1 {
			t.Fatalf("accounts = %d, want 1 (updated in place)", len(cfg.Accounts))
		}
		if cfg.Accounts[0].ID != "existing" {
			t.Errorf("ID = %q, want the existing account's ID preserved", cfg.Accounts[0].ID)
		}
		if cfg.Accounts[0].Label != "new label" || cfg.Accounts[0].Credential.AccessToken != "new-token" {
			t.Errorf("account = %+v, want label/credential replaced in place", cfg.Accounts[0])
		}
	})

	t.Run("falls back to label when no uuid", func(t *testing.T) {
		cfg := &config.Config{Accounts: []config.Account{
			{ID: "existing", Label: "same-label"},
		}}
		acc := config.Account{ID: "fresh", Label: "same-label", Credential: provider.Credential{AccessToken: "new-token"}}

		_, isNew := mergeLoginAccount(cfg, acc)
		if isNew {
			t.Error("want isNew = false: the label already exists")
		}
		if len(cfg.Accounts) != 1 {
			t.Fatalf("accounts = %d, want 1", len(cfg.Accounts))
		}
	})

	t.Run("same account uuid in a different org is a separate account", func(t *testing.T) {
		// One email in both a personal org and a Team org: Anthropic returns
		// the same account uuid for both. Logging into the second must not
		// overwrite the first.
		cfg := &config.Config{Accounts: []config.Account{
			{ID: "personal", Identity: config.Identity{AccountUUID: "u1", OrgUUID: "org-personal"},
				Credential: provider.Credential{AccessToken: "personal-token"}},
		}}
		acc := config.Account{ID: "team", Identity: config.Identity{AccountUUID: "u1", OrgUUID: "org-team"},
			Credential: provider.Credential{AccessToken: "team-token"}}

		id, isNew := mergeLoginAccount(cfg, acc)
		if !isNew || id != "team" {
			t.Errorf("got (%q, %v), want (\"team\", true): a different org is a different account", id, isNew)
		}
		if len(cfg.Accounts) != 2 {
			t.Fatalf("accounts = %d, want 2", len(cfg.Accounts))
		}
		if cfg.Accounts[0].Credential.AccessToken != "personal-token" {
			t.Errorf("personal account credential overwritten: %q", cfg.Accounts[0].Credential.AccessToken)
		}

		// A re-login into the same org still dedupes.
		again := config.Account{ID: "fresh", Identity: config.Identity{AccountUUID: "u1", OrgUUID: "org-team"},
			Credential: provider.Credential{AccessToken: "team-token-2"}}
		id, isNew = mergeLoginAccount(cfg, again)
		if isNew || id != "team" || len(cfg.Accounts) != 2 {
			t.Errorf("re-login into same org: got (%q, %v, %d accounts), want (\"team\", false, 2)", id, isNew, len(cfg.Accounts))
		}
	})

	t.Run("appends when nothing matches", func(t *testing.T) {
		cfg := &config.Config{Accounts: []config.Account{
			{ID: "existing", Identity: config.Identity{AccountUUID: "u1"}},
		}}
		acc := config.Account{ID: "fresh", Identity: config.Identity{AccountUUID: "u2"}}

		_, isNew := mergeLoginAccount(cfg, acc)
		if !isNew {
			t.Error("want isNew = true: a different uuid is a genuinely new account")
		}
		if len(cfg.Accounts) != 2 {
			t.Fatalf("accounts = %d, want 2", len(cfg.Accounts))
		}
	})
}

// A long-lived login that comes back materially shorter than a year means the
// server ignored expires_in. The login still works; the operator must be told
// now rather than discovering it when the account dies in an hour.
func TestOnLoginSuccessWarnsWhenLongLivedTokenIsShort(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	mgr := account.New(nil, map[string]provider.Provider{}, account.Options{
		Persist: func(string, provider.Credential) error { return nil },
	})
	var buf bytes.Buffer
	hook := onLoginSuccess(store, mgr, "anthropic", slog.New(slog.NewTextHandler(&buf, nil)))

	err := hook(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at", LongLived: true,
		ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
	}, provider.Profile{Email: "a@example.com", AccountUUID: "u1"})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	log := buf.String()
	if !strings.Contains(log, "shorter") {
		t.Errorf("log = %q, want a downgrade warning", log)
	}
	if !strings.Contains(log, "expiresIn=") {
		t.Errorf("log = %q, want the actual lifetime named", log)
	}
}

func TestOnLoginSuccessDoesNotWarnForAFullYearLongLivedToken(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	mgr := account.New(nil, map[string]provider.Provider{}, account.Options{
		Persist: func(string, provider.Credential) error { return nil },
	})
	var buf bytes.Buffer
	hook := onLoginSuccess(store, mgr, "anthropic", slog.New(slog.NewTextHandler(&buf, nil)))

	err := hook(context.Background(), provider.Credential{
		Type: provider.CredentialOAuth, AccessToken: "at", LongLived: true,
		ExpiresAt: time.Now().Add(365 * 24 * time.Hour).UnixMilli(),
	}, provider.Profile{Email: "b@example.com", AccountUUID: "u2"})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if strings.Contains(buf.String(), "shorter") {
		t.Errorf("log = %q, want no downgrade warning", buf.String())
	}
}
