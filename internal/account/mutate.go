package account

import (
	"fmt"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// SetEnabled toggles whether an account participates in selection. The
// account continues to exist and hold its accumulated state (quota, errors);
// disabling only removes it from Select's candidate set (see eligibleLocked).
func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return fmt.Errorf("unknown account %q", id)
	}
	a.Disabled = !enabled
	return nil
}

// SetPriority changes an account's ranking for Select, which prefers the
// lowest priority value among eligible candidates (see select.go).
func (m *Manager) SetPriority(id string, priority int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.byID[id]
	if a == nil {
		return fmt.Errorf("unknown account %q", id)
	}
	a.Priority = priority
	return nil
}

// Remove deletes an account from the registry entirely, including any
// session-affinity entries pinned to it. Leaving a stale affinity entry
// behind would point a future request at an account id Select can never find
// again, silently falling through to whatever ordinary selection picks
// instead without anyone having decided that was fine.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID[id] == nil {
		return fmt.Errorf("unknown account %q", id)
	}
	delete(m.byID, id)
	for i, a := range m.accounts {
		if a.ID == id {
			m.accounts = append(m.accounts[:i], m.accounts[i+1:]...)
			break
		}
	}
	for session, acctID := range m.affinity {
		if acctID == id {
			delete(m.affinity, session)
		}
	}
	return nil
}

// Add registers a brand-new account into the live registry without a
// restart — the mirror image of Remove. ImportCredentials and a successful
// Login both persist through config.Store first and then call this, exactly
// like every other Local mutation (persist, then apply; see
// view.Local.mu's doc comment): a failed persist must never leave an account
// live that a restart would not reproduce.
//
// A duplicate id is rejected rather than silently overwriting the existing
// account's accumulated runtime state (quota history, in-flight count,
// errors) — ids are assigned once and never reused (spec §6.2), so a
// collision here means a caller reused one, not that this is an update.
func (m *Manager) Add(c config.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byID[c.ID]; exists {
		return fmt.Errorf("account %q already exists", c.ID)
	}
	a := fromConfig(c)
	m.accounts = append(m.accounts, a)
	m.byID[a.ID] = a
	return nil
}

// Provider returns the registered provider.Provider for a name (e.g.
// "anthropic"), so a caller above Manager — view.Local's Login, specifically
// — can drive a provider-level operation like Login without Manager having
// to expose its whole provider registry or import anything above itself.
func (m *Manager) Provider(name string) (provider.Provider, bool) {
	p, ok := m.providers[name]
	return p, ok
}

// SetSwitchThreshold changes the utilization above which an account is
// treated as spent for selection purposes (§4.4). It takes effect on the very
// next Select call: eligibleLocked reads m.opts.SwitchThreshold under the same
// mutex this method locks, so there is no separate propagation step and no
// restart required.
func (m *Manager) SetSwitchThreshold(v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts.SwitchThreshold = v
}

// SetSessionAffinity turns session pinning on or off. Like SetSwitchThreshold,
// this is read from the same locked field Select consults, so it applies
// immediately with no restart.
func (m *Manager) SetSessionAffinity(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts.SessionAffinity = v
}
