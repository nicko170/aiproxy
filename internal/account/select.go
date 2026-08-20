package account

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
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

	// The operator override comes first: ahead of affinity, ahead of ranking.
	if a := m.pinnedChoiceLocked(req, nowMS); a != nil {
		return copyAccount(a), nil
	}

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
		// Spend the allowance closest to being wasted. A score of 0 means
		// nothing could be scored, so an account we know nothing about sorts
		// last rather than first — descending order gives that for free.
		ri, rj := expiringAllowance(ai, req.Model, nowMS), expiringAllowance(aj, req.Model, nowMS)
		if ri != rj {
			return ri > rj
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
	if !servesModel(a, req.Model) {
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

// servesModel reports whether this account can reach the requested model. An
// account with no catalogue yet is treated as able to serve anything: unknown
// must not mean unusable, or a new account is dead until its first probe.
func servesModel(a *Account, model string) bool {
	if model == "" || len(a.Models) == 0 {
		return true
	}
	for _, m := range a.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}

// expiringAllowance scores how much unused allowance an account is on track to
// waste, so selection spends the capacity that is about to vanish first.
//
// For one window the quantity that matters is a rate: to avoid wasting headroom
// H before the window resets in T hours, you must spend H/T per hour. The
// account facing the highest required burn rate is the one closest to losing
// capacity, so the score is the largest such rate across the windows binding
// this model.
//
// Headroom is weighted by the window's own length. Utilization is a fraction of
// that window's limit, and the limits are not the same size — losing 90% of a
// weekly allowance is a far bigger loss than 90% of a five-hour one. Taking the
// bare minimum reset across buckets, as this function used to, made the 5h
// window dominate the key (it nearly always resets sooner) and left an expiring
// weekly window invisible.
//
// The upstream never reports absolute limits, so window duration stands in for
// relative capacity. That is an approximation, and it deliberately errs toward
// draining long windows — which is the intent: a weekly allowance wasted is gone
// for a week, while a 5h allowance regenerates before the day is out.
//
// Returns 0 when nothing can be scored — no buckets, unknown resets, resets
// already in the past, or no headroom left.
func expiringAllowance(a *Account, model string, nowMS int64) float64 {
	var best float64
	for name, b := range a.Buckets {
		if !BucketAppliesTo(name, model) || b.ResetsAt == 0 {
			continue
		}
		hoursLeft := float64(b.ResetsAt-nowMS) / float64(time.Hour/time.Millisecond)
		if hoursLeft <= 0 {
			// Stale or just reset. Not an infinitely urgent window: dividing by
			// a non-positive remainder would score it above every real
			// candidate and pin selection to the oldest reading we hold.
			continue
		}
		headroom := 1 - b.Utilization
		if headroom <= 0 {
			continue
		}
		if rate := headroom * windowHours(name) / hoursLeft; rate > best {
			best = rate
		}
	}
	return best
}

// windowHours is the length of a named quota window ("5h", "7d", "7d_oi"),
// standing in for how much allowance it holds. An unrecognised shape weighs 1,
// which keeps it in the ranking without letting it dominate one.
func windowHours(name string) float64 {
	if i := strings.Index(name, "_"); i > 0 {
		name = name[:i] // "7d_oi" -> "7d"
	}
	if len(name) < 2 {
		return 1
	}
	n, err := strconv.Atoi(name[:len(name)-1])
	if err != nil || n <= 0 {
		return 1
	}
	switch name[len(name)-1] {
	case 'h':
		return float64(n)
	case 'd':
		return float64(n) * 24
	case 'w':
		return float64(n) * 24 * 7
	}
	return 1
}

// Pin forces every request onto one account, ahead of session affinity and the
// ranking. It is the operator saying "use this one now", so it outranks rules
// that exist to make a good automatic choice.
//
// It does NOT outrank eligibility. An account cannot serve a model it lacks, or
// answer while rate-limited, and pretending otherwise would turn an override
// into an outage. What the pin overrides is preference, not possibility.
func (m *Manager) Pin(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID[id] == nil {
		return fmt.Errorf("unknown account %q", id)
	}
	m.pinned = id
	return nil
}

// Unpin restores normal routing.
func (m *Manager) Unpin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pinned = ""
}

// Pinned reports the forced account, or "" when routing is normal.
func (m *Manager) Pinned() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pinned
}

// pinnedChoiceLocked resolves the override for one request. It returns the
// account to use, or nil to fall through to normal routing, and clears a pin
// that has served its purpose.
//
// The distinction that matters here is between "cannot serve THIS request" and
// "is finished". Only the second ends the override:
//
//   - Ineligible — a model it lacks, a transient rate-limit hold, or already in
//     Exclude because the retry loop just failed on it. Rotate, but KEEP the
//     pin: all three resolve on their own, and cancelling an operator's
//     override because one unrelated request named the wrong model, or because
//     a single 401 arrived, would make the feature untrustworthy.
//   - Exhausted — see pinExhaustedLocked. The pin is one-shot, so this is where
//     it ends.
//
// Exclusion needs no branch of its own: eligibleLocked already rejects an
// excluded account, and an account that is both excluded and genuinely spent
// should still end the pin. An explicit early return here would have preserved
// a pin on a spent account purely because this request had already tried it.
func (m *Manager) pinnedChoiceLocked(req SelectRequest, nowMS int64) *Account {
	if m.pinned == "" {
		return nil
	}
	a := m.byID[m.pinned]
	if a == nil {
		m.pinned = "" // removed from the config entirely
		return nil
	}
	if m.eligibleLocked(a, req, nowMS) {
		return a
	}
	if m.pinExhaustedLocked(a, nowMS) {
		m.pinned = ""
	}
	return nil
}

// pinExhaustedLocked reports whether a pinned account is finished rather than
// merely unavailable for one request.
//
// Only ACCOUNT-WIDE quota counts. A model-scoped window can be spent while the
// account still serves everything else, and ending an override set for general
// traffic because one model family ran out would be surprising. Disabled and
// errored also count, since neither recovers without someone intervening.
func (m *Manager) pinExhaustedLocked(a *Account, nowMS int64) bool {
	if a.Disabled || a.Status == StatusErrored {
		return true
	}
	for name, b := range a.Buckets {
		if _, scoped := cutModelScope(name); scoped {
			continue
		}
		if b.Status == "rejected" || b.Utilization >= m.opts.SwitchThreshold {
			return true
		}
	}
	return false
}
