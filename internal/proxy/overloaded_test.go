package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// overloadRetry is the config these tests share. The main budget is
// deliberately SMALLER than the overload budget, so a test that only passes
// because overload waits were charged to the main budget fails instead.
func overloadRetry() RetryConfig {
	return RetryConfig{
		Budget:           500 * time.Millisecond,
		InlineAbsorbMax:  500 * time.Millisecond,
		BodyIdle:         5 * time.Second,
		OverloadedBudget: 5 * time.Second,
	}
}

// hinted529 is a 529 stating the shortest hint upstream can usefully give.
// Tests use a hint rather than relying on the backoff schedule so their timing
// stays pinned to one number they set, not to a constant they do not own.
func hinted529() testutil.Script {
	return testutil.Script{
		Status: 529,
		Header: http.Header{"Retry-After": []string{"1"}},
		Body:   `{"type":"error","error":{"type":"overloaded_error"}}`,
	}
}

// A 529 is Anthropic running out of capacity, not this account running out of
// quota. No other account can fix it, so the retry stays put rather than spend
// a second account's send allowance on a condition it cannot resolve.
func TestAttemptRetriesOverloadOnTheSameAccount(t *testing.T) {
	h := newHarness(t, 2, overloadRetry(),
		hinted529(),
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 once upstream recovers", res.StatusCode)
	}
	if got := h.last().AccountID; got != "acct-0" {
		t.Errorf("served by %q, want acct-0: an overload must not rotate", got)
	}
	if h.last().Rotated {
		t.Error("Rotated should be false; nothing was wrong with the account")
	}
	if n := len(h.upstream.Requests()); n != 2 {
		t.Errorf("made %d upstream attempts, want 2", n)
	}
}

// The point of the separate clock: overload waits must not draw down the budget
// that rotation, credential refresh, and rate-limit absorption depend on. Here
// two 1s waits run against a 500ms main budget, so a shared clock exhausts long
// before the 200 is ever reached.
func TestAttemptOverloadWaitsDoNotDrawDownTheMainBudget(t *testing.T) {
	h := newHarness(t, 1, overloadRetry(),
		hinted529(), hinted529(),
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; overload waits must not spend the main budget", res.StatusCode)
	}
	if n := len(h.upstream.Requests()); n != 3 {
		t.Errorf("made %d upstream attempts, want 3", n)
	}
}

// maxSendsPerAccount is 2, and it exists to stop a misbehaving upstream
// spinning on one account. An overload retry is not that — it is a deliberate,
// clocked wait — so it earns its own send instead of consuming the cap and
// rotating away to accounts that are equally overloaded. With a second account
// available, the cap binding would be visible as a rotation.
func TestAttemptOverloadRetriesAreNotCappedByTheSendAllowance(t *testing.T) {
	h := newHarness(t, 2, overloadRetry(),
		hinted529(), hinted529(),
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after two overload retries", res.StatusCode)
	}
	if got := h.last().AccountID; got != "acct-0" {
		t.Errorf("served by %q; the send cap should not have forced a rotation", got)
	}
	if h.last().Rotated {
		t.Error("Rotated should be false across all three sends")
	}
}

// When there is no time left to wait it out, the client gets upstream's own 529
// and body — not a proxy-invented error. Claude Code already understands 529
// and retries on its own; replacing it would hide what actually happened.
func TestAttemptRelaysTheRealOverloadWhenItsBudgetRunsOut(t *testing.T) {
	cfg := overloadRetry()
	cfg.OverloadedBudget = 300 * time.Millisecond // no room for even one 1s wait
	h := newHarness(t, 1, cfg, hinted529())

	// Posted directly rather than through h.post, which discards the body.
	start := time.Now()
	res, err := http.Post(h.srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	elapsed := time.Since(start)

	if res.StatusCode != 529 {
		t.Fatalf("status = %d, want the upstream's own 529", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"overloaded_error"`) {
		t.Errorf("body = %q, want upstream's own error payload", body)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v; the overload budget should have stopped the retries", elapsed)
	}
	if got := h.last().Outcome; got != provider.OutcomeOverloaded {
		t.Errorf("outcome = %v, want overloaded", got)
	}
}

// A 529 says nothing about this account's quota, so it must not put the account
// into a hold. Holding on an upstream-wide condition would walk the whole
// rotation into a pause for something none of the accounts caused.
func TestAttemptDoesNotHoldTheAccountOnOverload(t *testing.T) {
	cfg := overloadRetry()
	cfg.OverloadedBudget = 300 * time.Millisecond
	h := newHarness(t, 1, cfg, hinted529())

	if res, _ := h.post(); res.StatusCode != 529 {
		t.Fatalf("status = %d, want 529", res.StatusCode)
	}

	got, err := h.mgr.Select(account.SelectRequest{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Select after an overload: %v", err)
	}
	if got.ID != "acct-0" {
		t.Errorf("selected %q; a 529 must not hold the account it landed on", got.ID)
	}
}

// An upstream that states a duration longer than the clock allows is surfaced
// rather than slept on, the same way a long Retry-After on a 429 already is.
func TestAttemptSurfacesAnOverloadHintLongerThanItsBudget(t *testing.T) {
	cfg := overloadRetry()
	cfg.OverloadedBudget = 2 * time.Second
	h := newHarness(t, 1, cfg, testutil.Script{
		Status: 529,
		Header: http.Header{"Retry-After": []string{"120"}},
		Body:   `{"type":"error","error":{"type":"overloaded_error"}}`,
	})

	res, elapsed := h.post()
	if res.StatusCode != 529 {
		t.Fatalf("status = %d, want 529", res.StatusCode)
	}
	if elapsed > time.Second {
		t.Errorf("took %v; a hint beyond the overload budget must not be slept on", elapsed)
	}
	if n := len(h.upstream.Requests()); n != 1 {
		t.Errorf("made %d upstream attempts, want 1", n)
	}
}

// With no hint at all the schedule takes over, so an overload with bare headers
// is still absorbed rather than passed straight back.
func TestAttemptRetriesAnOverloadCarryingNoHint(t *testing.T) {
	cfg := overloadRetry()
	cfg.OverloadedBudget = 3 * time.Second
	h := newHarness(t, 1, cfg,
		testutil.Script{Status: 529, Body: `{"type":"error","error":{"type":"overloaded_error"}}`},
		testutil.Script{Status: 200, Body: `{"ok":true}`},
	)

	res, _ := h.post()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200; a hintless overload should still be retried", res.StatusCode)
	}
	if n := len(h.upstream.Requests()); n != 2 {
		t.Errorf("made %d upstream attempts, want 2", n)
	}
}
