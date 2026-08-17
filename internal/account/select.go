package account

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// ErrNoAccount reports that no account is currently eligible.
var ErrNoAccount = errors.New("no eligible account")

// SelectRequest describes what a request needs from an account.
type SelectRequest struct {
	Model     string
	SessionID string
	// Exclude holds account ids already attempted or refused for this request.
	Exclude map[string]bool
}

// BucketAppliesTo reports whether a quota bucket constrains a given model.
//
// Window buckets ("5h", "7d") bind everything. A model-scoped bucket
// ("7d_fable") binds only models whose name carries that token, so an account
// out of one model's weekly allowance still serves the others. An empty model
// name is treated conservatively: every bucket binds.
func BucketAppliesTo(bucket, model string) bool {
	suffix, scoped := cutModelScope(bucket)
	if !scoped {
		return true
	}
	if model == "" {
		return true
	}
	return strings.Contains(strings.ToLower(model), suffix)
}

func cutModelScope(bucket string) (string, bool) {
	i := strings.Index(bucket, "_")
	if i < 0 || i+1 >= len(bucket) {
		return "", false
	}
	return strings.ToLower(bucket[i+1:]), true
}

// RecordSession pins a client session to the account that served it, so the
// upstream prompt cache for that conversation stays warm.
func (m *Manager) RecordSession(sessionID, accountID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.affinity[sessionID] = accountID
}

// MarkRateLimited holds an account out of selection for d.
func (m *Manager) MarkRateLimited(id string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil {
		until := m.opts.Now().Add(d).UnixMilli()
		if until > a.RateLimitedUntil {
			a.RateLimitedUntil = until
		}
	}
}

// ClearRateLimited releases a hold. Any non-429 response is live proof the hold
// no longer binds, which is what lets a revalidating request restore an account.
func (m *Manager) ClearRateLimited(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.byID[id]; a != nil {
		a.RateLimitedUntil = 0
	}
}

// Select returns the best eligible account, honouring session affinity first.
// The returned Account is a value copy: it must never be a pointer into live
// Manager state, since a caller reading it races EnsureFresh mutating the
// account's Credential under the lock.
func (m *Manager) Select(req SelectRequest) (Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nowMS := m.opts.Now().UnixMilli()

	if m.opts.SessionAffinity && req.SessionID != "" {
		if id, ok := m.affinity[req.SessionID]; ok {
			if a := m.byID[id]; a != nil && m.eligibleLocked(a, req, nowMS) {
				return copyAccount(a), nil
			}
		}
	}

	candidates := make([]*Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		if m.eligibleLocked(a, req, nowMS) {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return Account{}, ErrNoAccount
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i], candidates[j]
		// A paused account stays eligible — a hinted throttle should queue
		// requests on the same warm account rather than push the burst
		// elsewhere — but must never outrank a healthy one. It is chosen only
		// when nothing unpaused is left.
		pi, pj := ai.PausedUntil > nowMS, aj.PausedUntil > nowMS
		if pi != pj {
			return !pi
		}
		if ai.Priority != aj.Priority {
			return ai.Priority < aj.Priority
		}
		// Spend the allowance that expires soonest first; an unknown reset sorts
		// last so a known-expiring account is preferred over an unknown one.
		ri, rj := soonestReset(ai, req.Model), soonestReset(aj, req.Model)
		if ri != rj {
			if ri == 0 {
				return false
			}
			if rj == 0 {
				return true
			}
			return ri < rj
		}
		return ai.ID < aj.ID // deterministic
	})
	return copyAccount(candidates[0]), nil
}

func (m *Manager) eligibleLocked(a *Account, req SelectRequest, nowMS int64) bool {
	if a.Disabled || a.Status == StatusErrored {
		return false
	}
	if req.Exclude[a.ID] {
		return false
	}
	if a.RateLimitedUntil > nowMS {
		return false
	}
	if !hasCredential(a) {
		return false
	}
	for name, b := range a.Buckets {
		if !BucketAppliesTo(name, req.Model) {
			continue
		}
		if b.Status == "rejected" {
			return false
		}
		if b.Utilization >= m.opts.SwitchThreshold {
			return false
		}
	}
	return true
}

func hasCredential(a *Account) bool {
	return a.Credential.AccessToken != "" || a.Credential.RefreshToken != "" || a.Credential.APIKey != ""
}

// soonestReset is the nearest future reset among buckets binding this model.
func soonestReset(a *Account, model string) int64 {
	var best int64
	for name, b := range a.Buckets {
		if !BucketAppliesTo(name, model) || b.ResetsAt == 0 {
			continue
		}
		if best == 0 || b.ResetsAt < best {
			best = b.ResetsAt
		}
	}
	return best
}
