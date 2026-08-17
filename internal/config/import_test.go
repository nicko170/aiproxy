package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nicko170/aiproxy/internal/provider"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportLegacyAccounts(t *testing.T) {
	p := writeFile(t, t.TempDir(), "legacy.json", `{
	  "accounts": [
	    {"name": "a@example.com (Acme)", "type": "oauth",
	     "accessToken": "at-1", "refreshToken": "rt-1", "expiresAt": 1786986000000,
	     "accountUuid": "acct-uuid", "orgUuid": "org-uuid", "orgName": "Acme"},
	    {"name": "fallback", "type": "apikey", "apiKey": "sk-test", "priority": 10,
	     "disabled": true, "upstream": "https://api.example.com",
	     "modelMap": {"claude-x": "model-y"}}
	  ]
	}`)

	got, err := ImportFile(p, ImportSourceLegacy)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2", len(got))
	}

	a := got[0]
	if a.Label != "a@example.com (Acme)" || a.Provider != "anthropic" {
		t.Errorf("account 0 = %+v", a)
	}
	if a.Credential.Type != provider.CredentialOAuth || a.Credential.AccessToken != "at-1" ||
		a.Credential.RefreshToken != "rt-1" || a.Credential.ExpiresAt != 1786986000000 {
		t.Errorf("credential 0 = %+v", a.Credential)
	}
	if a.Identity.OrgName != "Acme" || a.Identity.AccountUUID != "acct-uuid" {
		t.Errorf("identity 0 = %+v", a.Identity)
	}
	if a.ID == "" {
		t.Error("imported account must be assigned an id")
	}

	b := got[1]
	if b.Credential.Type != provider.CredentialAPIKey || b.Credential.APIKey != "sk-test" {
		t.Errorf("credential 1 = %+v", b.Credential)
	}
	if !b.Disabled || b.Priority != 10 || b.Upstream != "https://api.example.com" {
		t.Errorf("account 1 = %+v", b)
	}
	if b.ModelMap["claude-x"] != "model-y" {
		t.Errorf("model map = %+v", b.ModelMap)
	}
	if a.ID == b.ID {
		t.Error("each imported account needs a distinct id")
	}
}

func TestImportClaudeCodeCredentials(t *testing.T) {
	p := writeFile(t, t.TempDir(), "creds.json", `{
	  "claudeAiOauth": {"accessToken": "at-9", "refreshToken": "rt-9",
	                    "expiresAt": 1786986000000, "subscriptionType": "max"}
	}`)

	got, err := ImportFile(p, ImportSourceClaudeCode)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if got[0].Credential.AccessToken != "at-9" || got[0].Credential.RefreshToken != "rt-9" {
		t.Errorf("credential = %+v", got[0].Credential)
	}
	if got[0].Identity.Plan != "max" {
		t.Errorf("plan = %q, want max", got[0].Identity.Plan)
	}
}

// A legacy file predating unix-ms timestamps may carry a seconds-scale
// expiresAt. If that value is stored as-is, a real expiry gets read as a
// moment in 1970 and the credential looks permanently expired, forcing a
// refresh on every request.
func TestImportLegacyNormalizesSecondsScaleExpiresAt(t *testing.T) {
	p := writeFile(t, t.TempDir(), "legacy.json", `{
	  "accounts": [
	    {"name": "seconds@example.com", "type": "oauth",
	     "accessToken": "at-1", "refreshToken": "rt-1", "expiresAt": 1786986000}
	  ]
	}`)

	got, err := ImportFile(p, ImportSourceLegacy)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if got[0].Credential.ExpiresAt != 1786986000_000 {
		t.Errorf("ExpiresAt = %d, want normalized unix ms 1786986000000", got[0].Credential.ExpiresAt)
	}
}

// Same defect, Claude Code branch.
func TestImportClaudeCodeNormalizesSecondsScaleExpiresAt(t *testing.T) {
	p := writeFile(t, t.TempDir(), "creds.json", `{
	  "claudeAiOauth": {"accessToken": "at-9", "refreshToken": "rt-9",
	                    "expiresAt": 1786986000, "subscriptionType": "max"}
	}`)

	got, err := ImportFile(p, ImportSourceClaudeCode)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if got[0].Credential.ExpiresAt != 1786986000_000 {
		t.Errorf("ExpiresAt = %d, want normalized unix ms 1786986000000", got[0].Credential.ExpiresAt)
	}
}

func TestImportSkipsAccountsWithNoUsableCredential(t *testing.T) {
	p := writeFile(t, t.TempDir(), "legacy.json",
		`{"accounts":[{"name":"broken","type":"oauth"},{"name":"ok","type":"apikey","apiKey":"k"}]}`)

	got, err := ImportFile(p, ImportSourceLegacy)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 || got[0].Label != "ok" {
		t.Errorf("got %+v, want only the usable account", got)
	}
}

func TestImportMissingFileReportsNotExist(t *testing.T) {
	_, err := ImportFile(filepath.Join(t.TempDir(), "nope.json"), ImportSourceLegacy)
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if id == "" {
			t.Fatal("empty id")
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
