package warmer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/account"
	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/provider/anthropic"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// upstream records every warm request it receives and answers like the real
// Messages API, including the rate-limit headers a warm exists to create.
type upstream struct {
	srv *httptest.Server

	mu         sync.Mutex
	requests   []recorded
	status     int
	resetsAt   int64
	omitLimits bool
}

type recorded struct {
	path  string
	auth  string
	body  map[string]any
	vsn   string
	ctype string
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{status: 200, resetsAt: time.Now().Add(5 * time.Hour).Unix()}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		u.mu.Lock()
		u.requests = append(u.requests, recorded{
			path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body,
			vsn: r.Header.Get("anthropic-version"), ctype: r.Header.Get("content-type"),
		})
		status, resets, omit := u.status, u.resetsAt, u.omitLimits
		u.mu.Unlock()

		if !omit {
			w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.01")
			w.Header().Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(resets, 10))
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"h"}]}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) seen() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recorded(nil), u.requests...)
}

// fail makes the upstream answer status WITHOUT rate-limit headers, which is
// what a real server error looks like. It matters: if a failed warm still
// returned headers, the recorded window would itself stop the retry and a test
// about the cooldown would pass without the cooldown existing.
func (u *upstream) fail(status int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = status
	u.omitLimits = true
}

func acct(id string, priority int, url string) config.Account {
	return config.Account{
		ID: id, Provider: "anthropic", Label: id, Priority: priority, Upstream: url,
		Credential: provider.Credential{
			Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		},
	}
}

func newWarmer(t *testing.T, opts Options, accts ...config.Account) (*Warmer, *account.Manager) {
	t.Helper()
	providers := map[string]provider.Provider{"anthropic": anthropic.New(http.DefaultClient)}
	mgr := account.New(accts, providers, account.Options{
		SwitchThreshold: 0.98,
		Persist:         func(string, provider.Credential) error { return nil },
	})
	if opts.Log == nil {
		opts.Log = quiet()
	}
	return New(mgr, providers, http.DefaultTransport, opts), mgr
}

// hot marks an account as being deep into its own window; cold leaves the
// standby's window unstarted, which is the state warming exists to fix.
func hot(m *account.Manager, id string, util float64) {
	m.UpdateQuota(id, []provider.QuotaBucket{
		{Name: "5h", Utilization: util, ResetsAt: time.Now().Add(time.Hour).UnixMilli()},
	})
}

// The core case: one account half-spent, the other never touched, so the
// standby's five-hour clock is not running and will not start until traffic
// fails over onto it.
func TestWarmsAStandbyOnceTheActiveAccountIsHalfSpent(t *testing.T) {
	u := newUpstream(t)
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5},
		acct("busy", 0, u.srv.URL), acct("standby", 1, u.srv.URL))
	hot(mgr, "busy", 0.55)

	if err := w.WarmNow(context.Background()); err != nil {
		t.Fatalf("WarmNow: %v", err)
	}
	got := u.seen()
	if len(got) != 1 {
		t.Fatalf("made %d requests, want exactly 1", len(got))
	}
	if got[0].path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", got[0].path)
	}
	if got[0].auth == "" || got[0].vsn == "" {
		t.Errorf("auth=%q version=%q; a warm has no client to supply either", got[0].auth, got[0].vsn)
	}
	if mt, _ := got[0].body["max_tokens"].(float64); mt != 1 {
		t.Errorf("max_tokens = %v, want 1: a warm starts a window, it does not do work", mt)
	}
	// The response's own rate-limit headers must be recorded, or the next cycle
	// sees the clock still stopped and warms the same account again.
	if st := w.Status().Accounts["standby"]; st.LastWarmedAt == 0 || st.LastError != "" {
		t.Errorf("status = %+v, want a recorded success", st)
	}
}

// Below the threshold there is no handover coming, so nothing should be spent.
func TestDoesNotWarmBelowTheThreshold(t *testing.T) {
	u := newUpstream(t)
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5},
		acct("busy", 0, u.srv.URL), acct("standby", 1, u.srv.URL))
	hot(mgr, "busy", 0.49)

	if err := w.WarmNow(context.Background()); err != nil {
		t.Fatalf("WarmNow: %v", err)
	}
	if n := len(u.seen()); n != 0 {
		t.Errorf("made %d requests below the threshold, want 0", n)
	}
}

// An account whose window is already running has nothing to start. Warming it
// would spend a request to learn what the quota data already says.
func TestDoesNotWarmAnAccountWhoseWindowIsAlreadyRunning(t *testing.T) {
	u := newUpstream(t)
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5},
		acct("busy", 0, u.srv.URL), acct("standby", 1, u.srv.URL))
	hot(mgr, "busy", 0.9)
	hot(mgr, "standby", 0.02) // already ticking

	if err := w.WarmNow(context.Background()); err != nil {
		t.Fatalf("WarmNow: %v", err)
	}
	if n := len(u.seen()); n != 0 {
		t.Errorf("made %d requests, want 0: the standby's clock is already running", n)
	}
}

// Warming records the response's rate-limit headers, so a second cycle sees the
// window it just started and stops. Without this the trigger latches on and
// bills a request every interval.
func TestASecondCycleDoesNotWarmTheSameAccountAgain(t *testing.T) {
	u := newUpstream(t)
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5},
		acct("busy", 0, u.srv.URL), acct("standby", 1, u.srv.URL))
	hot(mgr, "busy", 0.8)

	for i := range 3 {
		if err := w.WarmNow(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if n := len(u.seen()); n != 1 {
		t.Errorf("made %d requests across three cycles, want 1", n)
	}
}

// A failing warm must back off. Retried every interval, one dead credential
// becomes a permanent stream of billable attempts.
func TestAFailedWarmBacksOffInsteadOfRetryingEveryCycle(t *testing.T) {
	u := newUpstream(t)
	u.fail(500)
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5},
		acct("busy", 0, u.srv.URL), acct("standby", 1, u.srv.URL))
	hot(mgr, "busy", 0.8)

	if err := w.WarmNow(context.Background()); err == nil {
		t.Fatal("WarmNow returned nil for a 500")
	}
	for range 3 {
		_ = w.WarmNow(context.Background())
	}
	if n := len(u.seen()); n != 1 {
		t.Errorf("made %d requests, want 1: a failure must cool down", n)
	}
	if st := w.Status().Accounts["standby"]; st.LastError == "" {
		t.Error("the failure should be visible in Status")
	}
}

// Disabled means disabled: the loop must never send anything.
func TestDisabledNeverSends(t *testing.T) {
	u := newUpstream(t)
	w, mgr := newWarmer(t, Options{Enabled: false, Threshold: 0.5, Interval: 5 * time.Millisecond},
		acct("busy", 0, u.srv.URL), acct("standby", 1, u.srv.URL))
	hot(mgr, "busy", 0.9)

	w.Start()
	time.Sleep(60 * time.Millisecond)
	w.Stop()
	if n := len(u.seen()); n != 0 {
		t.Errorf("made %d requests while disabled, want 0", n)
	}
}

// The background loop must actually fire, and Stop must end it.
func TestStartWarmsAndStopEndsTheLoop(t *testing.T) {
	u := newUpstream(t)
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5, Interval: 5 * time.Millisecond},
		acct("busy", 0, u.srv.URL), acct("standby", 1, u.srv.URL))
	hot(mgr, "busy", 0.9)

	w.Start()
	deadline := time.Now().Add(2 * time.Second)
	for len(u.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	w.Stop()
	if n := len(u.seen()); n != 1 {
		t.Errorf("made %d requests, want exactly 1", n)
	}
}

// An API-key account has no OAuth plan window to start, so warming it would
// spend money for nothing.
func TestSkipsNonOAuthAccounts(t *testing.T) {
	u := newUpstream(t)
	standby := acct("standby", 1, u.srv.URL)
	standby.Credential = provider.Credential{Type: provider.CredentialAPIKey, APIKey: "sk"}
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5},
		acct("busy", 0, u.srv.URL), standby)
	hot(mgr, "busy", 0.9)

	if err := w.WarmNow(context.Background()); err != nil {
		t.Fatalf("WarmNow: %v", err)
	}
	if n := len(u.seen()); n != 0 {
		t.Errorf("made %d requests, want 0 for an api-key account", n)
	}
}

// With several standbys, warm the one traffic would actually fail over to
// rather than an arbitrary map-order pick.
func TestWarmsTheHighestPriorityStandbyFirst(t *testing.T) {
	u := newUpstream(t)
	w, mgr := newWarmer(t, Options{Enabled: true, Threshold: 0.5},
		acct("busy", 0, u.srv.URL), acct("third", 9, u.srv.URL), acct("next", 1, u.srv.URL))
	hot(mgr, "busy", 0.8)

	if err := w.WarmNow(context.Background()); err != nil {
		t.Fatalf("WarmNow: %v", err)
	}
	got := u.seen()
	if len(got) != 1 {
		t.Fatalf("made %d requests, want 1", len(got))
	}
	if st := w.Status().Accounts["next"]; st.LastWarmedAt == 0 {
		t.Errorf("warmed the wrong account; status = %+v", w.Status().Accounts)
	}
}
