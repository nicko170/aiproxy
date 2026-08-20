package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
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

// A credential file present but carrying nothing usable must import zero
// accounts rather than one broken account that fails on its first request.
func TestImportSkipsAccountsWithNoUsableCredential(t *testing.T) {
	p := writeFile(t, t.TempDir(), "creds.json", `{"claudeAiOauth":{"accessToken":""}}`)

	got, err := ImportFile(p, ImportSourceClaudeCode)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing imported", got)
	}
}

func TestImportMissingFileReportsNotExist(t *testing.T) {
	_, err := ImportFile(filepath.Join(t.TempDir(), "nope.json"), ImportSourceClaudeCode)
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want a not-exist error", err)
	}
}

func TestImportCodexCredentials(t *testing.T) {
	p := writeFile(t, t.TempDir(), "auth.json", `{
	  "auth_mode":"chatgpt",
	  "tokens":{"id_token":"idt","access_token":"at","refresh_token":"rt","account_id":"acc-1"},
	  "last_refresh":"2026-08-12T04:36:46Z"}`)

	got, err := ImportFile(p, ImportSourceCodex)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want 1 account", got)
	}
	if got[0].Provider != "openai" {
		t.Errorf("provider = %q, want openai", got[0].Provider)
	}
	if got[0].Credential.AccessToken != "at" || got[0].Credential.RefreshToken != "rt" {
		t.Errorf("credential not carried across: %+v", got[0].Credential)
	}
	if got[0].Credential.AccountID != "acc-1" {
		t.Errorf("AccountID = %q; the chatgpt-account-id header needs it", got[0].Credential.AccountID)
	}
}

// IMPORTANT 6. auth.json states no expiry, and account.Manager reads
// ExpiresAt == 0 as "no expiry known, do not churn" — so a Codex-imported
// credential was never proactively refreshed at all. After the access token
// aged out the prober's EnsureFresh no-opped, Quota answered 401, and a 401
// carries no backoff, so it warned every cycle forever.
func TestImportCodexStampsAnExpirySoRefreshCanFire(t *testing.T) {
	before := time.Now().UnixMilli()
	p := writeFile(t, t.TempDir(), "auth.json", `{
	  "auth_mode":"chatgpt",
	  "tokens":{"id_token":"idt","access_token":"at","refresh_token":"rt","account_id":"acc-1"},
	  "last_refresh":"2026-08-12T04:36:46Z"}`)

	got, err := ImportFile(p, ImportSourceCodex)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	exp := got[0].Credential.ExpiresAt
	if exp == 0 {
		t.Fatal("ExpiresAt = 0; account.Manager reads that as 'no expiry known' and never refreshes")
	}
	// Stamped as expired-at-import, so the next EnsureFresh renews. Deliberately
	// not derived from last_refresh plus an assumed 10-day lifetime: see
	// importedExpiry.
	if exp < before || exp > time.Now().UnixMilli() {
		t.Errorf("ExpiresAt = %d, want the import instant (between %d and now)", exp, before)
	}
}

// A file that DOES state an expiry is believed rather than overwritten, so the
// Claude Code path is not made to churn by the fix above.
func TestImportClaudeCodeKeepsTheStatedExpiry(t *testing.T) {
	want := time.Now().Add(8 * time.Hour).UnixMilli()
	p := writeFile(t, t.TempDir(), "creds.json",
		`{"claudeAiOauth":{"accessToken":"at","refreshToken":"rt","expiresAt":`+
			strconv.FormatInt(want, 10)+`}}`)

	got, err := ImportFile(p, ImportSourceClaudeCode)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if got[0].Credential.ExpiresAt != want {
		t.Errorf("ExpiresAt = %d, want %d", got[0].Credential.ExpiresAt, want)
	}
}

// An api-key-mode auth.json has no OAuth tokens to adopt.
func TestImportCodexSkipsApiKeyMode(t *testing.T) {
	p := writeFile(t, t.TempDir(), "auth.json", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x","tokens":null}`)
	got, err := ImportFile(p, ImportSourceCodex)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
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
