package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/view"
)

// fakeLoginSource is a minimal view.Source stand-in for exercising
// loginSessionRegistry directly, without a full routerHarness: only Login is
// implemented (begin is the only Source method the registry ever calls); the
// embedded nil view.Source makes every other method panic if a test
// accidentally reaches it, which is preferable to a silent wrong answer.
type fakeLoginSource struct {
	view.Source
	login func(ctx context.Context) (provider.LoginSession, error)
}

func (f fakeLoginSource) Login(ctx context.Context, _ string) (provider.LoginSession, error) {
	return f.login(ctx)
}

// The control API's begin handler used to pass r.Context() straight into
// src.Login, tying the session's lifetime to the HTTP request that started
// it (see loginBeginHandler and the C1 fix). This proves begin never does
// that: the context Login receives must not already be some other,
// unrelated context's — it is begin's own, request-independent one.
func TestLoginSessionRegistryBeginUsesARequestIndependentContext(t *testing.T) {
	reg := newLoginSessionRegistry()

	done := make(chan provider.LoginResult, 1)
	var gotCtx context.Context
	src := fakeLoginSource{login: func(ctx context.Context) (provider.LoginSession, error) {
		gotCtx = ctx
		return provider.LoginSession{URL: "https://example.invalid", Done: done}, nil
	}}

	id, _, err := reg.begin(src, "anthropic")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if gotCtx == nil {
		t.Fatal("Login was never called")
	}
	if err := gotCtx.Err(); err != nil {
		t.Errorf("context handed to Login was already done (%v); it must be begin's own, not a caller's", err)
	}
	if _, ok := reg.get(id); !ok {
		t.Error("session should still be registered and alive")
	}
}

// I3: a session's registry entry must eventually release once the session
// reaches a terminal status ("done" or "error"), not accumulate for the
// life of the process. This drives both outcomes directly against the
// registry (a completed session and a cancelled/errored one) and asserts
// both leave r.sessions empty once sessionTTL elapses.
func TestLoginSessionRegistryReleasesTerminalSessions(t *testing.T) {
	newSession := func() (*loginSessionRegistry, chan provider.LoginResult) {
		reg := newLoginSessionRegistry()
		reg.sessionTTL = time.Millisecond
		done := make(chan provider.LoginResult, 1)
		return reg, done
	}

	t.Run("completed", func(t *testing.T) {
		reg, done := newSession()
		src := fakeLoginSource{login: func(context.Context) (provider.LoginSession, error) {
			return provider.LoginSession{URL: "https://example.invalid", Done: done}, nil
		}}

		id, _, err := reg.begin(src, "anthropic")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, ok := reg.get(id); !ok {
			t.Fatal("session should be registered right after begin")
		}

		done <- provider.LoginResult{Profile: provider.Profile{Email: "a@example.com"}}

		assertSessionReleased(t, reg, id)
	})

	t.Run("cancelled", func(t *testing.T) {
		reg, done := newSession()
		src := fakeLoginSource{login: func(context.Context) (provider.LoginSession, error) {
			return provider.LoginSession{URL: "https://example.invalid", Done: done}, nil
		}}

		id, _, err := reg.begin(src, "anthropic")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}

		done <- provider.LoginResult{Err: context.Canceled}

		assertSessionReleased(t, reg, id)
	})
}

// assertSessionReleased polls the registry until id is gone or a deadline
// elapses, so this does not depend on sleeping for exactly sessionTTL.
func assertSessionReleased(t *testing.T, reg *loginSessionRegistry, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.get(id); !ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s was never released from the registry", id)
}
