package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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
				ExpiresAt:    normalizeExpiresAt(f.ClaudeAiOauth.ExpiresAt),
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
