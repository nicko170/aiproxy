// Package account owns account runtime state, selection, and admission. It
// knows nothing about HTTP beyond the credential types the provider defines.
package account

import (
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

// copyAccount returns a value copy of a, deep-copying Buckets so the copy
// shares no mutable state with the live account. Every read path that hands an
// account out of the Manager (Get, All, Select, Snapshot) must go through this,
// rather than returning *Account: a caller holding a pointer can race a
// concurrent EnsureFresh writing a.Credential, and a Credential is three plain
// strings — a torn read produces a garbage token, not merely a stale one.
func copyAccount(a *Account) Account {
	out := *a
	out.Buckets = make(map[string]provider.QuotaBucket, len(a.Buckets))
	for k, v := range a.Buckets {
		out.Buckets[k] = v
	}
	return out
}
