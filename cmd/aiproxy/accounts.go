package main

import (
	"context"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// onLoginSuccess builds the anthropic.Anthropic.OnLoginSuccess hook: the
// only place a PKCE login's exchanged credential exists after the exchange
// (provider.LoginResult never carries one — see its doc comment), so this is
// where it is persisted through store and applied to mgr, "persist, then
// apply" like every other mutation.
//
// A re-login for an account already configured (its refresh token expired
// and the user re-ran the login flow, or they simply logged in again by
// habit) must update that account in place rather than append a second
// entry sharing its AccountUUID: config.ImportFile's dedupe
// (importDedupeKey in internal/view) already exists for exactly this reason
// on the import path, and the login path ignored it, leaving two entries
// that both rotate and both get probed. mergeLoginAccount reuses the same
// dedupe key here — and unlike ImportCredentials' dedupe (which only ever
// skips a duplicate), a re-login's whole point is a fresh credential, so the
// matched entry's Credential/Identity/Label are replaced in the store AND
// pushed into the live Manager via UpdateCredential; mgr.Add would instead
// create the exact duplicate this fix exists to avoid.
func onLoginSuccess(store *config.Store, mgr *account.Manager) func(context.Context, provider.Credential, provider.Profile) error {
	return func(_ context.Context, cred provider.Credential, profile provider.Profile) error {
		acc := config.Account{
			ID: config.NewID(), Provider: "anthropic", Label: loginLabel(profile),
			Credential: cred,
			Identity: config.Identity{
				AccountUUID: profile.AccountUUID, OrgUUID: profile.OrgUUID,
				OrgName: profile.OrgName, Plan: profile.Plan,
			},
		}

		var id string
		isNew := false
		if _, err := store.Update(func(c *config.Config) error {
			id, isNew = mergeLoginAccount(c, acc)
			return nil
		}); err != nil {
			return err
		}
		if !isNew {
			return mgr.UpdateCredential(id, acc.Credential, acc.Identity, acc.Label)
		}
		return mgr.Add(acc)
	}
}

// accountDedupeKey identifies an account for login-dedupe purposes: its
// credential's account uuid when known, else its label — the same two
// fields config.ImportFile's dedupe (view's importDedupeKey) keys on, so a
// re-login lands on the same account an import would already have deduped
// onto. Empty for an account with neither, which is treated as never a
// duplicate: there is nothing to key on.
func accountDedupeKey(a config.Account) string {
	if a.Identity.AccountUUID != "" {
		return "uuid:" + a.Identity.AccountUUID
	}
	if a.Label != "" {
		return "label:" + a.Label
	}
	return ""
}

// mergeLoginAccount folds acc into cfg: if an account sharing its dedupe key
// already exists, acc's Credential/Identity/Label replace that entry's in
// place — its ID is left untouched — and mergeLoginAccount returns that
// existing ID and reports it as not new, so the caller updates the live
// Manager entry under its original id (via UpdateCredential) rather than
// adding a second live copy through mgr.Add. Otherwise acc is appended under
// its own (already-fresh) id and reported as new.
func mergeLoginAccount(cfg *config.Config, acc config.Account) (id string, isNew bool) {
	if key := accountDedupeKey(acc); key != "" {
		for i := range cfg.Accounts {
			if accountDedupeKey(cfg.Accounts[i]) == key {
				cfg.Accounts[i].Credential = acc.Credential
				cfg.Accounts[i].Identity = acc.Identity
				cfg.Accounts[i].Label = acc.Label
				return cfg.Accounts[i].ID, false
			}
		}
	}
	cfg.Accounts = append(cfg.Accounts, acc)
	return acc.ID, true
}
