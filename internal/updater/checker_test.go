package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClock() func() time.Time {
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}

// waitFor polls cond until it holds or the deadline passes. The checker's
// loop is a goroutine; polling a predicate is how a test observes it without
// reaching into its internals or sleeping a fixed guess.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCheckerCachesAnAvailableUpdate(t *testing.T) {
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", nil)
	c := newTestClient(t, srv, "0.1.0")
	ck := NewChecker(c, true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	waitFor(t, "the first check", func() bool { return ck.State().CheckedAt != 0 })
	st := ck.State()
	if !st.Available {
		t.Error("Available should be true")
	}
	if st.Latest != "0.2.0" || st.Current != "0.1.0" {
		t.Errorf("State = %+v", st)
	}
	if st.PageURL == "" {
		t.Error("PageURL should be set so the UI can link to the release")
	}
	if st.Err != "" || st.Disabled || st.DevBuild {
		t.Errorf("State = %+v", st)
	}
}

// Property 5: a disabled checker makes no outbound request at all. This is
// the test that keeps the opt-out honest.
func TestDisabledCheckerMakesNoRequests(t *testing.T) {
	hits := 0
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", &hits)
	ck := NewChecker(newTestClient(t, srv, "0.1.0"), false, 10*time.Millisecond,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	time.Sleep(80 * time.Millisecond) // several intervals' worth
	if hits != 0 {
		t.Errorf("made %d requests while disabled, want 0", hits)
	}
	if st := ck.State(); !st.Disabled || st.Available {
		t.Errorf("State = %+v, want Disabled and not Available", st)
	}
}

// Enabling live must not mean waiting a whole interval for an answer.
func TestSetEnabledTriggersAnImmediateCheck(t *testing.T) {
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", nil)
	ck := NewChecker(newTestClient(t, srv, "0.1.0"), false, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Hour))
	ck.Start()
	defer ck.Stop()

	if ck.State().CheckedAt != 0 {
		t.Fatal("a disabled checker should not have checked")
	}
	ck.SetEnabled(true)
	waitFor(t, "the kicked check", func() bool { return ck.State().Available })
	if st := ck.State(); st.Disabled {
		t.Errorf("State = %+v, want not Disabled", st)
	}
}

// A transient network failure must not make an available update vanish from
// the header — the last good answer survives, with the error recorded beside
// it (spec: the check's caching rules).
func TestFailedCheckKeepsTheLastGoodAnswer(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	ck := NewChecker(newTestClient(t, srv, "0.1.0"), true, 10*time.Millisecond,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	waitFor(t, "the good check", func() bool { return ck.State().Available })
	fail = true
	waitFor(t, "the failed check", func() bool { return ck.State().Err != "" })

	st := ck.State()
	if !st.Available || st.Latest != "0.2.0" {
		t.Errorf("a failed check erased the last good answer: %+v", st)
	}
}

func TestCheckerReportsADevBuild(t *testing.T) {
	hits := 0
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", &hits)
	ck := NewChecker(newTestClient(t, srv, "dev"), true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	waitFor(t, "the first check", func() bool { return ck.State().CheckedAt != 0 })
	st := ck.State()
	if !st.DevBuild || st.Available || st.Err != "" {
		t.Errorf("State = %+v, want DevBuild with no error and nothing available", st)
	}
	if hits != 0 {
		t.Errorf("a dev build made %d requests, want 0", hits)
	}
}

// Once an update is installed, the header must stop offering it: the
// remaining action is a restart, which the flash says, and an "available"
// badge alongside "installed" would contradict itself.
func TestApplyClearsAvailable(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD")
	c := applyClient(t, srv, "0.1.0", exe)
	ck := NewChecker(c, true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()
	waitFor(t, "the first check", func() bool { return ck.State().Available })

	res, err := ck.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Updated || res.Version != "0.2.0" {
		t.Errorf("Result = %+v", res)
	}
	if st := ck.State(); st.Available {
		t.Error("Available should be cleared once the update is installed")
	}
}

func TestCheckerApplyReportsUpToDate(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD")
	ck := NewChecker(applyClient(t, srv, "0.2.0", exe), true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Hour))
	ck.Start()
	defer ck.Stop()

	if _, err := ck.Apply(context.Background()); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("err = %v, want ErrUpToDate", err)
	}
}
