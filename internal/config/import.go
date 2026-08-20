package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
)

// ImportSource names a credential file layout we can read.
type ImportSource string

const (
	// ImportSourceClaudeCode reads Claude Code's own credential file.
	ImportSourceClaudeCode ImportSource = "claude-code"
	// ImportSourceCodex reads the Codex CLI's own credential file.
	ImportSourceCodex ImportSource = "codex"
)

// ClaudeCodePath is Claude Code's credential file.
func ClaudeCodePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// CodexPath is the Codex CLI's credential file.
func CodexPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// NewID returns a stable opaque account handle.
func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// normalizeExpiresAt converts a possibly-seconds timestamp to unix millis.
// Values below 1e12 are seconds; anything at or above it is already millis.
//
// This is a deliberate duplicate of anthropic.NormalizeExpiresAt: internal/config
// sits below internal/provider/anthropic in the dependency graph (providers
// depend on config-shaped types, not the reverse), so config cannot import it.
// Both copies normalize once, at their own ingestion boundary — here, when
// reading an external credential file; there, when parsing a token refresh
// response — rather than at comparison time.
func normalizeExpiresAt(v int64) int64 {
	if v == 0 {
		return 0
	}
	if v < 1e12 {
		return v * 1000
	}
	return v
}

// importedExpiry is the ExpiresAt an imported credential gets. A file that
// states an expiry is believed (normalized to millis); a file that states none
// is treated as EXPIRED AS OF THE IMPORT, so the very next EnsureFresh renews
// it.
//
// This exists because Codex's auth.json carries no expiry at all, and
// account.Manager.needsRefreshLocked reads ExpiresAt == 0 as "no expiry known,
// do not churn". The combination is a credential that is never proactively
// refreshed: once the access token ages out, the prober's EnsureFresh no-ops,
// Quota answers 401, and a 401 is not a throttling error so it carries no
// backoff — the proxy then warns every cycle, forever, until an inference
// request happens to force a refresh on its own path. An imported account
// silently stops reporting quota and never says why.
//
// Two ways to fix it were available: derive an expiry from auth.json's
// last_refresh plus the observed ~10-day access-token lifetime, or treat an
// unknown expiry as immediately refreshable. This takes the SECOND, because:
//
//   - The 10-day lifetime is observed, not documented. If upstream shortens it,
//     a derived expiry silently reinstates the exact bug — proactive refresh
//     stops firing — and does so invisibly. Nothing here should depend on a
//     number we cannot verify.
//   - It is self-correcting rather than a standing assumption: the first
//     refresh returns a real expires_in, Persist writes the real ExpiresAt
//     back, and this fabricated value is never consulted again.
//   - It fails in the cheap direction. The cost is one token call shortly
//     after an import, on a credential the source tool has itself been
//     refreshing; and it surfaces a dead refresh token at import time, when
//     the operator is present, instead of ten days later. A failed refresh
//     does not churn either: EnsureFresh short-circuits on an already-expired
//     credential that a previous attempt already failed on.
//
// Claude Code's file normally does state an expiry, but it goes through the
// same helper so the two importers cannot drift: one carrying an expiry and
// the other not is exactly the asymmetry that produced this.
func importedExpiry(stated int64, now time.Time) int64 {
	if v := normalizeExpiresAt(stated); v != 0 {
		return v
	}
	return now.UnixMilli()
}

type claudeCodeFile struct {
	ClaudeAiOauth *struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// codexFile is the Codex CLI's own auth.json layout. Only the chatgpt auth
// mode carries OAuth tokens; an apikey-mode file has Tokens nil and nothing
// to adopt.
//
// There is deliberately no expiry field to read: auth.json states none, only a
// last_refresh timestamp. See importedExpiry for what is done about that and
// why last_refresh is not used to derive one.
type codexFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   *struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// ImportFile reads accounts from an external credential file. Accounts with no
// usable credential are skipped rather than imported broken. The source file is
// never modified.
func ImportFile(path string, src ImportSource) ([]Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	switch src {
	case ImportSourceClaudeCode:
		var f claudeCodeFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if f.ClaudeAiOauth == nil || f.ClaudeAiOauth.AccessToken == "" {
			return nil, nil
		}
		return []Account{{
			ID:       NewID(),
			Provider: "anthropic",
			Label:    "imported (claude code)",
			Credential: provider.Credential{
				Type:         provider.CredentialOAuth,
				AccessToken:  f.ClaudeAiOauth.AccessToken,
				RefreshToken: f.ClaudeAiOauth.RefreshToken,
				ExpiresAt:    importedExpiry(f.ClaudeAiOauth.ExpiresAt, time.Now()),
			},
			Identity: Identity{Plan: f.ClaudeAiOauth.SubscriptionType},
		}}, nil

	case ImportSourceCodex:
		var f codexFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if f.Tokens == nil || f.Tokens.AccessToken == "" {
			return nil, nil
		}
		return []Account{{
			ID:       NewID(),
			Provider: "openai",
			Label:    "imported (codex)",
			Credential: provider.Credential{
				Type:         provider.CredentialOAuth,
				AccessToken:  f.Tokens.AccessToken,
				RefreshToken: f.Tokens.RefreshToken,
				AccountID:    f.Tokens.AccountID,
				// auth.json carries no expiry, so this is stamped as
				// already-expired and the next EnsureFresh renews it. Without
				// it, ExpiresAt == 0 means "no expiry known" to
				// account.Manager and proactive refresh never fires at all.
				ExpiresAt: importedExpiry(0, time.Now()),
			},
			// AccountID is the identity the dedupe path keys on (see
			// importDedupeKey in internal/view): without it here, re-running
			// the import would append a second account every time rather than
			// recognising the one already present.
			Identity: Identity{AccountUUID: f.Tokens.AccountID},
		}}, nil
	}
	return nil, fmt.Errorf("unknown import source %q", src)
}
