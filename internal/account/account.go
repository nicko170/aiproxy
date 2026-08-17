// Package account owns account runtime state, selection, and admission. It
// knows nothing about HTTP beyond the credential types the provider defines.
package account

import (
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

type Status int

const (
	StatusActive Status = iota
	StatusErrored
)

func (s Status) String() string {
	if s == StatusErrored {
		return "errored"
	}
	return "active"
}

// Account is one upstream identity plus everything learned about it at runtime.
// Guarded by Manager's mutex; never mutate outside the manager.
type Account struct {
	ID          string
	Label       string
	Provider    string
	Priority    int
	Disabled    bool
	Credential  provider.Credential
	AccountUUID string
	OrgName     string
	Plan        string
	Upstream    string
	ModelMap    map[string]string

	Status    Status
	LastError string

	// Buckets is the most recent quota observation per bucket name.
	Buckets map[string]provider.QuotaBucket
	// RateLimitedUntil holds the account out of selection entirely (unix ms).
	RateLimitedUntil int64
	// PausedUntil keeps the account selectable but makes Admit wait (unix ms).
	PausedUntil int64
	// RampStartedAt begins a storm-control window (unix ms); 0 means no ramp.
	RampStartedAt int64
	InFlight      int
}

// ToProvider projects the fields a provider is allowed to see.
func (a *Account) ToProvider() provider.Account {
	return provider.Account{
		ID:          a.ID,
		Label:       a.Label,
		Credential:  a.Credential,
		AccountUUID: a.AccountUUID,
		Upstream:    a.Upstream,
		ModelMap:    a.ModelMap,
	}
}

func fromConfig(c config.Account) *Account {
	return &Account{
		ID:          c.ID,
		Label:       c.Label,
		Provider:    c.Provider,
		Priority:    c.Priority,
		Disabled:    c.Disabled,
		Credential:  c.Credential,
		AccountUUID: c.Identity.AccountUUID,
		OrgName:     c.Identity.OrgName,
		Plan:        c.Identity.Plan,
		Upstream:    c.Upstream,
		ModelMap:    c.ModelMap,
		Status:      StatusActive,
		Buckets:     map[string]provider.QuotaBucket{},
	}
}

// millis converts a time to unix ms, treating the zero time as 0.
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
