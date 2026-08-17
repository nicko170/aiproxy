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
	// ImportSourceLegacy reads a prior tool's config: {"accounts":[...]}.
	ImportSourceLegacy ImportSource = "legacy"
	// ImportSourceClaudeCode reads Claude Code's own credential file.
	ImportSourceClaudeCode ImportSource = "claude-code"
)

// LegacyPath is the config we can adopt accounts from on a first run.
func LegacyPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "teamclaude.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "teamclaude.json")
}

// ClaudeCodePath is Claude Code's credential file.
func ClaudeCodePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

// NewID returns a stable opaque account handle.
func NewID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type legacyFile struct {
	Accounts []legacyAccount `json:"accounts"`
}

type legacyAccount struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	AccessToken  string            `json:"accessToken"`
	RefreshToken string            `json:"refreshToken"`
	ExpiresAt    int64             `json:"expiresAt"`
	APIKey       string            `json:"apiKey"`
	Priority     int               `json:"priority"`
	Disabled     bool              `json:"disabled"`
	AccountUUID  string            `json:"accountUuid"`
	OrgUUID      string            `json:"orgUuid"`
	OrgName      string            `json:"orgName"`
	Upstream     string            `json:"upstream"`
	ModelMap     map[string]string `json:"modelMap"`
}

type claudeCodeFile struct {
	ClaudeAiOauth *struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
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
	case ImportSourceLegacy:
		var f legacyFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		out := make([]Account, 0, len(f.Accounts))
		for _, la := range f.Accounts {
			a, ok := fromLegacy(la)
			if !ok {
				continue
			}
			out = append(out, a)
		}
		return out, nil

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
				ExpiresAt:    f.ClaudeAiOauth.ExpiresAt,
			},
			Identity: Identity{Plan: f.ClaudeAiOauth.SubscriptionType},
		}}, nil
	}
	return nil, fmt.Errorf("unknown import source %q", src)
}

func fromLegacy(la legacyAccount) (Account, bool) {
	cred := provider.Credential{}
	switch la.Type {
	case "apikey":
		if la.APIKey == "" {
			return Account{}, false
		}
		cred = provider.Credential{Type: provider.CredentialAPIKey, APIKey: la.APIKey}
	default: // oauth
		if la.AccessToken == "" && la.RefreshToken == "" {
			return Account{}, false
		}
		cred = provider.Credential{
			Type:         provider.CredentialOAuth,
			AccessToken:  la.AccessToken,
			RefreshToken: la.RefreshToken,
			ExpiresAt:    la.ExpiresAt,
		}
	}
	return Account{
		ID:         NewID(),
		Provider:   "anthropic",
		Label:      la.Name,
		Priority:   la.Priority,
		Disabled:   la.Disabled,
		Credential: cred,
		Identity: Identity{
			AccountUUID: la.AccountUUID,
			OrgUUID:     la.OrgUUID,
			OrgName:     la.OrgName,
		},
		Upstream: la.Upstream,
		ModelMap: la.ModelMap,
	}, true
}
