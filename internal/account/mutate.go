package account

import "fmt"

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
