package account

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
)

// stubProvider counts Refresh calls and can be made slow, so coalescing is
// observable.
type stubProvider struct {
	refreshes atomic.Int32
	delay     time.Duration
	err       error
	panic     bool
	// started, when non-nil, receives one value as each Refresh begins.
	started chan struct{}
}

func (s *stubProvider) Name() string { return "stub" }

func (s *stubProvider) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	s.refreshes.Add(1)
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	// Honours ctx, as a real provider's HTTP call does: without that the refresh
	// timeout would be untestable and, worse, unenforceable.
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return provider.Credential{}, ctx.Err()
		}
	}
	if s.panic {
		panic("stubProvider: Refresh panicked")
	}
	if s.err != nil {
		return provider.Credential{}, s.err
	}
	return provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  "refreshed",
		RefreshToken: "rt-next",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	}, nil
}

func (s *stubProvider) Profile(context.Context, provider.Credential) (provider.Profile, error) {
	return provider.Profile{}, provider.ErrUnsupported
}
func (s *stubProvider) Login(context.Context) (provider.LoginSession, error) {
	return provider.LoginSession{}, provider.ErrUnsupported
}

func (s *stubProvider) Quota(context.Context, provider.Credential) (provider.Quota, error) {
	return provider.Quota{}, provider.ErrUnsupported
}
func (s *stubProvider) Endpoint(provider.Account) *url.URL {
	u, _ := url.Parse("https://upstream.invalid")
	return u
}
func (s *stubProvider) Authorize(*http.Request, provider.Credential)             {}
func (s *stubProvider) RewriteBody(b []byte, _ provider.Account) ([]byte, error) { return b, nil }
func (s *stubProvider) ClassifyResponse(*http.Response) provider.Outcome         { return provider.Outcome{} }
func (s *stubProvider) ParseUsage([]byte) (*provider.UsageDelta, bool)           { return nil, false }
func (s *stubProvider) ParseUsageBody([]byte) (*provider.UsageDelta, bool)       { return nil, false }

// persistLog records every credential handed to Options.Persist.
//
// The lock is not decoration and the accessor is not ceremony. Persist runs on
// the detached refresh goroutine, whose lifetime is deliberately independent of
// the EnsureFresh call that started it: a caller that cancels its wait, or one
// that short-circuits because the credential is already fresh, returns while
// that goroutine is still on its way to Persist. A test goroutine reading this
// log therefore has, in general, NO happens-before edge with the write. Handing
// the test a bare *[]provider.Credential guarded by a mutex it cannot reach made
// every such read an unsynchronised one — a data race on the slice header, not
// merely a stale count.
type persistLog struct {
	mu    sync.Mutex
	creds []provider.Credential
}

func (l *persistLog) add(c provider.Credential) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.creds = append(l.creds, c)
}

func (l *persistLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.creds)
}

func newTestManager(t *testing.T, p *stubProvider, accts ...config.Account) (*Manager, *persistLog) {
	t.Helper()
	persisted := &persistLog{}
	m := New(accts, map[string]provider.Provider{"stub": p}, Options{
		SwitchThreshold: 0.98,
		Persist: func(_ string, c provider.Credential) error {
			persisted.add(c)
			return nil
		},
	})
	return m, persisted
}

func expiredOAuth() provider.Credential {
	return provider.Credential{
		Type:         provider.CredentialOAuth,
		AccessToken:  "stale",
		RefreshToken: "rt",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
	}
}

func TestEnsureFreshRefreshesExpiredCredentialAndPersists(t *testing.T) {
	p := &stubProvider{}
	m, persisted := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	acc, ok := m.Get("a")
	if !ok {
		t.Fatal("Get: account not found")
	}
	if got := acc.Credential.AccessToken; got != "refreshed" {
		t.Errorf("AccessToken = %q, want refreshed", got)
	}
	if n := persisted.count(); n != 1 {
		t.Errorf("persisted %d times, want 1", n)
	}
}

func TestEnsureFreshSkipsValidCredential(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "good", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0 for a valid credential", n)
	}
}

func TestEnsureFreshForceRefreshesValidCredential(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "good", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	})

	if err := m.EnsureFresh(context.Background(), "a", true); err != nil {
		t.Fatalf("EnsureFresh(force): %v", err)
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want 1", n)
	}
}

// Several concurrent requests on one account must produce ONE refresh. Without
// coalescing, a burst turns into a burst of refreshes, and the upstream rotates
// the refresh token under itself so all but one attempt fail.
func TestEnsureFreshCoalescesConcurrentCallers(t *testing.T) {
	p := &stubProvider{delay: 50 * time.Millisecond}
	m, persisted := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.EnsureFresh(context.Background(), "a", false)
			// Every caller — leader or follower — must itself observe the
			// refreshed credential once its own call returns. A follower that
			// merely returned nil because it happened to short-circuit on
			// needsRefreshLocked would pass the error check below identically
			// while never having actually seen the new token.
			acc, ok := m.Get("a")
			if !ok {
				t.Errorf("caller %d: account vanished", i)
				return
			}
			if acc.Credential.AccessToken != "refreshed" {
				t.Errorf("caller %d: AccessToken = %q, want refreshed", i, acc.Credential.AccessToken)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1", n)
	}
	if n := persisted.count(); n != 1 {
		t.Errorf("persisted %d times, want 1", n)
	}
}

// The same coalescing must hold on the error path: every follower observes the
// leader's rejection rather than attempting its own retry.
func TestEnsureFreshCoalescesConcurrentCallersOnError(t *testing.T) {
	p := &stubProvider{delay: 50 * time.Millisecond, err: errors.New("invalid_grant")}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.EnsureFresh(context.Background(), "a", false)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("caller %d: got nil error, want the leader's rejection", i)
		}
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1 even on the error path", n)
	}
}

// rejected is a refusal of the credential ITSELF, the only failure that may
// sideline an account. Providers wrap provider.ErrCredentialRejected around
// their own rejection sentinels so this distinction crosses the seam without
// internal/account importing a concrete provider.
func rejected(msg string) error {
	return fmt.Errorf("%w: %s", provider.ErrCredentialRejected, msg)
}

func TestEnsureFreshMarksAccountErroredOnRejection(t *testing.T) {
	p := &stubProvider{err: rejected("invalid_grant")}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err == nil {
		t.Fatal("expected the refresh error to propagate")
	}
	acc, ok := m.Get("a")
	if !ok {
		t.Fatal("Get: account not found")
	}
	if acc.Status != StatusErrored {
		t.Errorf("Status = %v, want StatusErrored", acc.Status)
	}
	// The sidelining half, stated as the thing that actually matters: a rejected
	// credential must drop out of selection until someone logs in again.
	if _, err := m.Select(SelectRequest{}); !errors.Is(err, ErrNoAccount) {
		t.Errorf("Select = %v, want ErrNoAccount — a rejected credential stayed selectable", err)
	}
}

// Spec §11: transport errors are not credential errors, and must never sideline
// an account.
//
// The defect this pins down: ANY refresh error set StatusErrored, StatusErrored
// makes an account ineligible in Select, and only a successful refresh clears it
// — which can only be reached THROUGH Select. So a single DNS hiccup or dropped
// connection removed an account permanently, and with two accounts the proxy
// answered 429 for every request until the process was restarted.
func TestEnsureFreshDoesNotSidelineAnAccountOnTransportFailure(t *testing.T) {
	p := &stubProvider{err: &net.OpError{
		Op: "dial", Net: "tcp", Err: errors.New("connection refused"),
	}}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err == nil {
		t.Fatal("expected the transport error to propagate to the caller")
	}

	acc, ok := m.Get("a")
	if !ok {
		t.Fatal("Get: account not found")
	}
	if acc.Status != StatusActive {
		t.Errorf("Status = %v, want StatusActive; a network failure says nothing about "+
			"the credential and must not sideline the account", acc.Status)
	}
	if acc.LastError == "" {
		t.Error("LastError should still record what went wrong, so the status readout explains it")
	}
	// The consequence, asserted directly: the account is still there to try again.
	got, err := m.Select(SelectRequest{})
	if err != nil {
		t.Fatalf("Select after a transport failure: %v — one network blip removed the "+
			"account until the process restarts", err)
	}
	if got.ID != "a" {
		t.Errorf("Select = %q, want a", got.ID)
	}

	// And the retry genuinely happens rather than short-circuiting on a recorded
	// error, so the account recovers on its own the moment the network does.
	p.err = nil
	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("second EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 2 {
		t.Errorf("refreshed %d times, want 2 — the retry never reached the provider", n)
	}
	acc, _ = m.Get("a")
	if acc.Credential.AccessToken != "refreshed" {
		t.Errorf("AccessToken = %q, want refreshed", acc.Credential.AccessToken)
	}
}

// A refresh that SUCCEEDED but could not be written to disk leaves a working
// credential in memory. Marking the account errored threw that away and rotated
// off a perfectly good account because of a disk problem. The loss is real and
// must be visible — the rotated token is gone on restart — but it is not a reason
// to stop serving.
func TestEnsureFreshKeepsAWorkingCredentialWhenPersistFails(t *testing.T) {
	p := &stubProvider{}
	m := New([]config.Account{{
		ID: "a", Provider: "stub", Label: "a", Credential: expiredOAuth(),
	}}, map[string]provider.Provider{"stub": p}, Options{
		SwitchThreshold: 0.98,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Persist: func(string, provider.Credential) error {
			return errors.New("read-only file system")
		},
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("EnsureFresh: %v — a persistence failure must not fail the refresh, "+
			"which the caller reads as a reason to rotate away", err)
	}

	acc, ok := m.Get("a")
	if !ok {
		t.Fatal("Get: account not found")
	}
	if acc.Status != StatusActive {
		t.Errorf("Status = %v, want StatusActive; the credential in memory works", acc.Status)
	}
	if acc.Credential.AccessToken != "refreshed" {
		t.Errorf("AccessToken = %q, want the refreshed credential", acc.Credential.AccessToken)
	}
	if !strings.Contains(acc.LastError, "persist") {
		t.Errorf("LastError = %q; a lost rotated credential must be visible in the "+
			"status readout, since it will not survive a restart", acc.LastError)
	}
	if _, err := m.Select(SelectRequest{}); err != nil {
		t.Errorf("Select = %v; an account whose credential works must keep serving", err)
	}
}

// A dead refresh token must not be retried on every sequential call: once an
// account is StatusErrored and its credential is still actually expired
// (not merely inside the renew-soon threshold), EnsureFresh returns the
// recorded error immediately instead of paying another 3-attempt retry.
func TestEnsureFreshShortCircuitsAfterHardRejectionWithoutRetrying(t *testing.T) {
	p := &stubProvider{err: rejected("invalid_grant")}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	if err := m.EnsureFresh(context.Background(), "a", false); err == nil {
		t.Fatal("expected the refresh error to propagate")
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Fatalf("refreshed %d times after the first failure, want 1", n)
	}

	if err := m.EnsureFresh(context.Background(), "a", false); err == nil {
		t.Fatal("expected the recorded error to be returned again")
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want still 1 — a dead refresh token was retried instead of short-circuited", n)
	}

	// force still gets a real attempt, and a successful one clears the error.
	p.err = nil
	if err := m.EnsureFresh(context.Background(), "a", true); err != nil {
		t.Fatalf("forced EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 2 {
		t.Errorf("refreshed %d times, want 2 — force must bypass the short-circuit", n)
	}
	acc, ok := m.Get("a")
	if !ok {
		t.Fatal("Get: account not found")
	}
	if acc.Status != StatusActive {
		t.Errorf("Status = %v, want StatusActive after a successful forced refresh", acc.Status)
	}
}

// A panic inside Refresh or Persist must not leave the single-flight entry
// poisoned: without deferred cleanup nothing ever closes call.done and every
// later EnsureFresh on that account hangs until its context expires.
//
// The refresh now runs on its own goroutine, so there is no http.Handler on that
// stack for net/http to recover on and an unrecovered panic would take the whole
// process down. It is therefore recovered at the refresh and converted to an
// ordinary refresh failure, which also marks the account errored — the same
// treatment any other failed refresh gets.
func TestEnsureFreshCleansUpSingleFlightAfterProviderPanic(t *testing.T) {
	p := &stubProvider{panic: true}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	err := m.EnsureFresh(context.Background(), "a", false)
	if err == nil {
		t.Fatal("a panicking refresh must surface as an error, not as success")
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Errorf("err = %v, want it to name the panic", err)
	}
	acc, ok := m.Get("a")
	if !ok {
		t.Fatal("Get: account not found")
	}
	if acc.Status != StatusErrored {
		t.Errorf("Status = %v, want StatusErrored after a panicking refresh", acc.Status)
	}

	// The point of the test: the single-flight entry was released, so a later
	// refresh runs rather than hanging on a channel nothing will ever close.
	// force bypasses the errored-account short-circuit so a real attempt happens.
	p.panic = false
	done := make(chan error, 1)
	go func() { done <- m.EnsureFresh(context.Background(), "a", true) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureFresh after a prior panic: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureFresh hung — a prior panic left the single-flight entry poisoned")
	}
	if n := p.refreshes.Load(); n != 2 {
		t.Errorf("refreshed %d times, want 2 — the retry after the panic never reached the provider", n)
	}
}

// A caller that gives up must not cancel a refresh other callers are waiting on.
// The refresh runs on its own context precisely so the work survives the caller,
// and the next request finds the finished result rather than starting a second
// refresh — which is what coalescing exists to prevent. The upstream rotates the
// refresh token on every exchange, so a duplicate refresh strands one of them.
func TestEnsureFreshSurvivesTheCancellationOfItsCaller(t *testing.T) {
	p := &stubProvider{delay: 150 * time.Millisecond, started: make(chan struct{}, 1)}
	m, persisted := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	// First caller abandons its wait almost immediately.
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- m.EnsureFresh(ctx, "a", false) }()
	<-p.started // the refresh is genuinely under way before we cancel
	cancel()

	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller err = %v, want context.Canceled", err)
	}

	// The refresh must still be running and must still complete. A second caller
	// arriving after it lands sees the new credential.
	//
	// Wait for the persist as well as the rotated credential, and note why: this
	// is the ONE test with no happens-before edge to the refresh goroutine. Every
	// other caller returns through call.done, which the goroutine closes as its
	// last act; this caller was cancelled, and the second EnsureFresh below
	// short-circuits on an already-fresh credential without waiting on anything.
	// runRefresh publishes the credential under the manager lock and only THEN
	// calls Persist, so observing the new token through Get proves nothing about
	// whether the goroutine has reached Persist yet. Taking the rotated token as
	// the signal left the persist count both racy and, once the race was fixed,
	// flaky.
	deadline := time.Now().Add(3 * time.Second)
	for {
		acc, _ := m.Get("a")
		if acc.Credential.AccessToken == "refreshed" && persisted.count() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refresh died with its caller: token = %q, persisted %d times",
				acc.Credential.AccessToken, persisted.count())
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := m.EnsureFresh(context.Background(), "a", false); err != nil {
		t.Fatalf("second caller: %v", err)
	}
	acc, _ := m.Get("a")
	if acc.Credential.AccessToken != "refreshed" {
		t.Errorf("AccessToken = %q, want the completed refresh's", acc.Credential.AccessToken)
	}
	if n := p.refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1 — the abandoned refresh was repeated", n)
	}
	if n := persisted.count(); n != 1 {
		t.Errorf("persisted %d times, want 1", n)
	}
}

// A caller's own context bounds only its own wait. This is what lets the proxy
// cap a refresh by the request's remaining budget.
func TestEnsureFreshReturnsWhenTheCallersContextExpiresFirst(t *testing.T) {
	p := &stubProvider{delay: 2 * time.Second, started: make(chan struct{}, 1)}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := m.EnsureFresh(ctx, "a", false)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Errorf("waited %v for a 60ms context against a 2s refresh — the caller's "+
			"bound is not being honoured", elapsed)
	}
}

// A refresh that outruns RefreshTimeout must fail like any other refresh, and
// crucially the single-flight entry is released so a later EnsureFresh runs
// instead of hanging on it forever.
//
// It must NOT sideline the account. A token endpoint that stopped answering is a
// transport failure, not a statement about the credential, and this test used to
// assert the opposite — which is precisely the behaviour spec §11 forbids.
func TestEnsureFreshTimesOutASlowRefreshAndStaysUsable(t *testing.T) {
	p := &stubProvider{delay: 5 * time.Second}
	var mu sync.Mutex
	m := New([]config.Account{{
		ID: "a", Provider: "stub", Credential: expiredOAuth(),
	}}, map[string]provider.Provider{"stub": p}, Options{
		SwitchThreshold: 0.98,
		RefreshTimeout:  60 * time.Millisecond,
		Persist: func(string, provider.Credential) error {
			mu.Lock()
			defer mu.Unlock()
			return nil
		},
	})

	start := time.Now()
	err := m.EnsureFresh(context.Background(), "a", false)
	if err == nil {
		t.Fatal("a refresh past RefreshTimeout must fail rather than succeed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v; RefreshTimeout did not bound the refresh", elapsed)
	}
	acc, ok := m.Get("a")
	if !ok {
		t.Fatal("Get: account not found")
	}
	if acc.Status != StatusActive {
		t.Errorf("Status = %v, want StatusActive after a timed-out refresh: a slow token "+
			"endpoint is a transport failure and must not sideline the account", acc.Status)
	}
	if acc.LastError == "" {
		t.Error("LastError should record the timeout even though the account keeps serving")
	}

	// The single-flight entry must have been released.
	p.delay = 0
	done := make(chan error, 1)
	go func() { done <- m.EnsureFresh(context.Background(), "a", true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EnsureFresh after a timed-out refresh: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureFresh hung — a timed-out refresh wedged the single-flight entry")
	}
}

func TestEnsureFreshIgnoresAPIKeyCredentials(t *testing.T) {
	p := &stubProvider{}
	m, _ := newTestManager(t, p, config.Account{
		ID: "a", Provider: "stub",
		Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk"},
	})

	if err := m.EnsureFresh(context.Background(), "a", true); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if n := p.refreshes.Load(); n != 0 {
		t.Errorf("refreshed %d times, want 0 — an API key does not expire", n)
	}
}
