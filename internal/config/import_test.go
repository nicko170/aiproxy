package config

import (
	"os"
	"path/filepath"
	"testing"
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
