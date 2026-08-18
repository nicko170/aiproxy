// Package config owns the on-disk configuration: its schema, its defaults, and
// the single serialized writer every mutation goes through.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"

	"github.com/nicko170/aiproxy/internal/provider"
)

// Account is one upstream identity. ID is a stable opaque handle assigned once
// and never reused: array position must never be an identity, because reordering
// or relabelling would silently repoint anything referring to it.
type Account struct {
	ID         string              `json:"id"`
	Provider   string              `json:"provider"`
	Label      string              `json:"label"`
	Priority   int                 `json:"priority"`
	Disabled   bool                `json:"disabled"`
	Credential provider.Credential `json:"credential"`
	Identity   Identity            `json:"identity"`
	Upstream   string              `json:"upstream,omitempty"`
	ModelMap   map[string]string   `json:"modelMap,omitempty"`
}

type Identity struct {
	AccountUUID string `json:"accountUuid,omitempty"`
	OrgUUID     string `json:"orgUuid,omitempty"`
	OrgName     string `json:"orgName,omitempty"`
	Plan        string `json:"plan,omitempty"`
}

type Listen struct {
	Addr   string `json:"addr"`
	APIKey string `json:"apiKey"`
}

type Routing struct {
	SwitchThreshold float64  `json:"switchThreshold"`
	SessionAffinity bool     `json:"sessionAffinity"`
	BlockedModels   []string `json:"blockedModels"`
}

// Retry holds two independent clocks, and conflating them is a defect that has
// already shipped once.
//
// BudgetMS bounds only time the PROXY adds before the first byte: retry backoff,
// waiting on a paused account, inline absorption of a rate limit, credential
// refresh, and discarding a response we are rotating away from.
//
// HeaderTimeoutMS bounds one attempt's wait for response headers, which is the
// upstream's own work and must not draw the budget down. Time-to-first-token on
// a large context with extended thinking is seconds; a budget of 1 000 applied
// to it cancels healthy requests and answers 429.
type Retry struct {
	BudgetMS          int `json:"budgetMs"`
	InlineAbsorbMaxMS int `json:"inlineAbsorbMaxMs"`
	BodyIdleMS        int `json:"bodyIdleMs"`
	HeaderTimeoutMS   int `json:"headerTimeoutMs"`
}

type QuotaProbe struct {
	IntervalSeconds int `json:"intervalSeconds"`
}

type Metrics struct {
	RetentionDays int `json:"retentionDays"`
}

type MITM struct {
	Enabled bool `json:"enabled"`
}

// Update controls in-app update checking. CheckEnabled is opt-out rather than
// opt-in because a proxy that silently runs months behind a security fix is
// the worse default — but it IS an outbound request that tells github.com
// this installation's IP and version, so turning it off must be one setting,
// documented, and honoured absolutely (no request is made when it is false).
//
// CheckIntervalHours is a day by default: a release cadence measured in weeks
// does not reward polling, and the answer is cached besides.
type Update struct {
	CheckEnabled       bool `json:"checkEnabled"`
	CheckIntervalHours int  `json:"checkIntervalHours"`
}

type Config struct {
	Listen     Listen     `json:"listen"`
	Accounts   []Account  `json:"accounts"`
	Routing    Routing    `json:"routing"`
	Retry      Retry      `json:"retry"`
	QuotaProbe QuotaProbe `json:"quotaProbe"`
	Metrics    Metrics    `json:"metrics"`
	MITM       MITM       `json:"mitm"`
	Update     Update     `json:"update"`
}

// Default returns the configuration for a fresh install.
//
// QuotaProbe defaults to 300s rather than something aggressive: the zero-spend
// usage endpoint has its own rate limit, and polling it every 30s gets the probe
// itself throttled, after which quota data goes stale and account selection
// decides on outdated numbers.
func Default() Config {
	return Config{
		Listen:     Listen{Addr: "127.0.0.1:3456", APIKey: newAPIKey()},
		Accounts:   []Account{},
		Routing:    Routing{SwitchThreshold: 0.98, SessionAffinity: true, BlockedModels: []string{}},
		Retry:      Retry{BudgetMS: 10000, InlineAbsorbMaxMS: 5000, BodyIdleMS: 120000, HeaderTimeoutMS: 60000},
		QuotaProbe: QuotaProbe{IntervalSeconds: 300},
		Metrics:    Metrics{RetentionDays: 90},
		MITM:       MITM{Enabled: true},
		Update:     Update{CheckEnabled: true, CheckIntervalHours: 24},
	}
}

func newAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "ap-" + base64.RawURLEncoding.EncodeToString(b)
}

// Dir is the configuration directory, honouring XDG_CONFIG_HOME.
func Dir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "aiproxy")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aiproxy"
	}
	return filepath.Join(home, ".config", "aiproxy")
}

func Path() string { return filepath.Join(Dir(), "config.json") }

// DBPath is the accounting database, a sibling of the config.
func DBPath() string { return filepath.Join(Dir(), "metrics.db") }
