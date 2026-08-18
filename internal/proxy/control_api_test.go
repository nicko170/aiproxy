package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/testutil"
	"github.com/nicko170/aiproxy/internal/view"
)

// The control API is a thin adapter over view.Source: these tests exercise it
// as an HTTP client would, over the real router, and check that handlers do
// no aggregation of their own (they seed the metrics store directly, the same
// way internal/view's own tests do, and confirm the numbers survive the HTTP
// round trip unchanged).

func seedUsage(t *testing.T, ms *metrics.Store, base int64) {
	t.Helper()
	ing := metrics.NewIngester(ms, metrics.IngestOptions{})
	defer ing.Close()
	ing.Record(metrics.Sample{
		StartedAt: base + 1000, AccountID: "acct-0", Provider: "anthropic",
		Model: "claude-sonnet-5", Status: 200, Outcome: "ok",
		InputTokens: 100, OutputTokens: 20, TTFBMS: 40, WaitMS: 0,
	})
	if err := ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := metrics.RollupOnce(context.Background(), ms, time.UnixMilli(base+60000), time.Hour); err != nil {
		t.Fatalf("RollupOnce: %v", err)
	}
}

func TestControlAPIUsageReturnsSeededTotals(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedUsage(t, h.ms, base)

	url := fmt.Sprintf("%s%s/api/v1/usage?from=%d&to=%d&granularity=minute&groupBy=model",
		h.srv.URL, ReservedPrefix, base, base+60000)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}

	var got struct {
		Points []struct {
			Key         string `json:"key"`
			InputTokens int64  `json:"inputTokens"`
		} `json:"points"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var total int64
	for _, p := range got.Points {
		total += p.InputTokens
	}
	if total != 100 {
		t.Errorf("input tokens = %d, want 100", total)
	}
}

// An empty window must report zero points, not an error — the case a
// query suite that only ever seeds rows cannot catch.
func TestControlAPIUsageOnEmptyWindowReturnsEmptyPoints(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	future := time.Now().Add(48 * time.Hour).UnixMilli()

	url := fmt.Sprintf("%s%s/api/v1/usage?from=%d&to=%d&granularity=hour&groupBy=model",
		h.srv.URL, ReservedPrefix, future, future+3600_000)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 on an empty window", res.StatusCode)
	}
	var got struct {
		Points []json.RawMessage `json:"points"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Points) != 0 {
		t.Errorf("points = %v, want none", got.Points)
	}
}

func TestControlAPIUsageRejectsMissingWindow(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/usage?granularity=hour&groupBy=model")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing from/to", res.StatusCode)
	}
}

func TestControlAPIUsageRejectsUnknownGroupBy(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	url := fmt.Sprintf("%s%s/api/v1/usage?from=0&to=1&groupBy=nonsense", h.srv.URL, ReservedPrefix)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown groupBy", res.StatusCode)
	}
}

func TestControlAPITotalsOnEmptyWindowReturnsZeroNotError(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	future := time.Now().Add(48 * time.Hour).UnixMilli()

	url := fmt.Sprintf("%s%s/api/v1/totals?from=%d&to=%d", h.srv.URL, ReservedPrefix, future, future+1000)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 on an empty window", res.StatusCode)
	}
	var got struct {
		Requests int64 `json:"requests"`
	}
	json.NewDecoder(res.Body).Decode(&got)
	if got.Requests != 0 {
		t.Errorf("requests = %d, want 0", got.Requests)
	}
}

func TestControlAPITotalsReflectsSeededData(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedUsage(t, h.ms, base)

	url := fmt.Sprintf("%s%s/api/v1/totals?from=%d&to=%d", h.srv.URL, ReservedPrefix, base, base+60000)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		Requests    int64 `json:"requests"`
		InputTokens int64 `json:"inputTokens"`
	}
	json.NewDecoder(res.Body).Decode(&got)
	if got.Requests != 1 || got.InputTokens != 100 {
		t.Errorf("totals = %+v", got)
	}
}

func TestControlAPILatencyReflectsSeededData(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC).UnixMilli()
	seedUsage(t, h.ms, base)

	url := fmt.Sprintf("%s%s/api/v1/latency?from=%d&to=%d", h.srv.URL, ReservedPrefix, base, base+60000)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		TTFBP50MS int64 `json:"ttfbP50Ms"`
	}
	json.NewDecoder(res.Body).Decode(&got)
	if got.TTFBP50MS != 40 {
		t.Errorf("ttfbP50Ms = %d, want 40", got.TTFBP50MS)
	}
}

func TestControlAPIQuotaHistoryRequiresAccountParam(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/quota/history?from=0&to=1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing account", res.StatusCode)
	}
}

func TestControlAPIQuotaHistoryForUnknownAccountReturnsEmptyNotError(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	future := time.Now().Add(time.Hour).UnixMilli()
	url := fmt.Sprintf("%s%s/api/v1/quota/history?account=nope&from=0&to=%d", h.srv.URL, ReservedPrefix, future)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var got []json.RawMessage
	json.NewDecoder(res.Body).Decode(&got)
	if len(got) != 0 {
		t.Errorf("points = %v, want none", got)
	}
}

func TestControlAPISetAccountEnabledPersistsAndReflectsInAccountsList(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+ReservedPrefix+"/api/v1/accounts/acct-0/enabled",
		strings.NewReader(`{"enabled":false}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}

	got, ok := h.mgr.Get("acct-0")
	if !ok || !got.Disabled {
		t.Fatalf("account = %+v, ok=%v; want disabled", got, ok)
	}
	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Accounts[0].Disabled {
		t.Error("the change should have persisted to config")
	}
}

func TestControlAPISetAccountEnabledUnknownAccountReturns404(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+ReservedPrefix+"/api/v1/accounts/nope/enabled",
		strings.NewReader(`{"enabled":false}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestControlAPISetAccountEnabledRejectsMalformedBody(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+ReservedPrefix+"/api/v1/accounts/acct-0/enabled",
		strings.NewReader(`not json`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestControlAPISetPriorityChangesSelectionOrder(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+ReservedPrefix+"/api/v1/accounts/acct-0/priority",
		strings.NewReader(`{"priority":9}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}

	got, _ := h.mgr.Get("acct-0")
	if got.Priority != 9 {
		t.Errorf("priority = %d, want 9", got.Priority)
	}
}

func TestControlAPIRemoveAccountDeletesItAndIsThenNotFound(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+ReservedPrefix+"/api/v1/accounts/acct-0", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if _, ok := h.mgr.Get("acct-0"); ok {
		t.Error("account should be gone")
	}

	// Deleting it again must now report 404, not silently succeed.
	req2, _ := http.NewRequest(http.MethodDelete, h.srv.URL+ReservedPrefix+"/api/v1/accounts/acct-0", nil)
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res2.Body)
	res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Errorf("second delete status = %d, want 404", res2.StatusCode)
	}
}

func TestControlAPIUpdateSettingsRejectsInvalidValues(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	before, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	body := `{"switchThreshold":-1,"retryBudgetMs":10000,"inlineAbsorbMaxMs":5000,` +
		`"headerTimeoutMs":60000,"bodyIdleMs":120000,"quotaProbeIntervalSeconds":300,"metricsRetentionDays":90}`
	res, err := http.Post(h.srv.URL+ReservedPrefix+"/api/v1/settings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}

	after, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after.Routing.SwitchThreshold != before.Routing.SwitchThreshold {
		t.Error("an invalid settings update must not have written anything")
	}
}

func TestControlAPIUpdateSettingsPersistsValidValues(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	body := `{"switchThreshold":0.5,"retryBudgetMs":8000,"inlineAbsorbMaxMs":4000,` +
		`"headerTimeoutMs":30000,"bodyIdleMs":60000,"sessionAffinity":false,` +
		`"blockedModels":["*fable*"],"quotaProbeIntervalSeconds":120,"metricsRetentionDays":30}`
	res, err := http.Post(h.srv.URL+ReservedPrefix+"/api/v1/settings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}

	cfg, err := h.cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Routing.SwitchThreshold != 0.5 || cfg.Retry.BudgetMS != 8000 || cfg.Metrics.RetentionDays != 30 {
		t.Errorf("persisted config = %+v", cfg)
	}
}

// GET settings must read back exactly what POST settings wrote — the getter
// that closes the write-only crack in invariant 4 (stage 4 could not
// previously read current settings back through the seam at all).
func TestControlAPIGetSettingsReflectsAPersistedUpdate(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	body := `{"switchThreshold":0.6,"retryBudgetMs":8000,"inlineAbsorbMaxMs":4000,` +
		`"headerTimeoutMs":30000,"bodyIdleMs":60000,"sessionAffinity":false,` +
		`"blockedModels":["*fable*"],"quotaProbeIntervalSeconds":120,"metricsRetentionDays":30}`
	res, err := http.Post(h.srv.URL+ReservedPrefix+"/api/v1/settings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("POST status = %d", res.StatusCode)
	}

	getRes, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer getRes.Body.Close()
	if getRes.StatusCode != 200 {
		t.Fatalf("GET status = %d", getRes.StatusCode)
	}
	var got struct {
		SwitchThreshold float64  `json:"switchThreshold"`
		SessionAffinity bool     `json:"sessionAffinity"`
		BlockedModels   []string `json:"blockedModels"`
	}
	if err := json.NewDecoder(getRes.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SwitchThreshold != 0.6 || got.SessionAffinity != false || len(got.BlockedModels) != 1 || got.BlockedModels[0] != "*fable*" {
		t.Errorf("GET settings = %+v, want the values just POSTed", got)
	}
}

// POST settings' response body reports which changed fields actually took
// effect (Live) versus which were persisted but need a restart
// (NeedsRestart) — data a stage-4 settings screen can act on, rather than a
// bare {"ok":true} it would have to take on faith.
func TestControlAPIUpdateSettingsResponseReportsLiveAndNeedsRestartFields(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	// switchThreshold changes (live); blockedModels changes (restart-gated);
	// everything else matches config.Default(), so it should appear in
	// neither list.
	body := `{"switchThreshold":0.5,"retryBudgetMs":10000,"inlineAbsorbMaxMs":5000,` +
		`"headerTimeoutMs":60000,"bodyIdleMs":120000,"sessionAffinity":true,` +
		`"blockedModels":["*fable*"],"quotaProbeIntervalSeconds":300,"metricsRetentionDays":90}`
	res, err := http.Post(h.srv.URL+ReservedPrefix+"/api/v1/settings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var got struct {
		Live         []string `json:"live"`
		NeedsRestart []string `json:"needsRestart"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Live) != 1 || got.Live[0] != "switchThreshold" {
		t.Errorf("Live = %v, want exactly [switchThreshold]", got.Live)
	}
	if len(got.NeedsRestart) != 1 || got.NeedsRestart[0] != "blockedModels" {
		t.Errorf("NeedsRestart = %v, want exactly [blockedModels]", got.NeedsRestart)
	}
}

// A control endpoint must gate a remote, unauthenticated caller exactly like
// the proxy path does; loopback stays exempt. httptest.NewServer always
// presents a loopback RemoteAddr (see TestControlAPIGatesRemoteCallersWithoutAKeyOverLoopback
// below), so this bypasses the network entirely and calls the live router's
// ServeHTTP directly with a synthetic remote address on the request itself —
// the only way to exercise the remote-caller-denied path against the real
// control API rather than against Authorized in isolation (TestAuthorized).
// Deleting the Authorized check at the top of controlHandler must make every
// assertion here fail.
func TestControlAPIGatesRemoteCallersWithoutAKey(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.APIKey = "secret"
	}, testutil.Script{Status: 200, Body: `{}`})

	handler := h.srv.Config.Handler
	const remote = "203.0.113.9:1234"

	// A read route with no credential from a remote address: must be denied.
	getNoKey := httptest.NewRequest(http.MethodGet, ReservedPrefix+"/api/v1/status", nil)
	getNoKey.RemoteAddr = remote
	recGet := httptest.NewRecorder()
	handler.ServeHTTP(recGet, getNoKey)
	if recGet.Code != http.StatusUnauthorized {
		t.Errorf("GET status, remote, no key: status = %d, want 401", recGet.Code)
	}

	// A mutation route with no credential from a remote address: must also
	// be denied — this is the route guarding account priority changes.
	postNoKey := httptest.NewRequest(http.MethodPost, ReservedPrefix+"/api/v1/accounts/acct-0/priority",
		strings.NewReader(`{"priority":9}`))
	postNoKey.RemoteAddr = remote
	recPost := httptest.NewRecorder()
	handler.ServeHTTP(recPost, postNoKey)
	if recPost.Code != http.StatusUnauthorized {
		t.Errorf("POST priority, remote, no key: status = %d, want 401", recPost.Code)
	}

	// The same remote caller, presenting the correct key, must succeed.
	getWithKey := httptest.NewRequest(http.MethodGet, ReservedPrefix+"/api/v1/status", nil)
	getWithKey.RemoteAddr = remote
	getWithKey.Header.Set("x-api-key", "secret")
	recGetOK := httptest.NewRecorder()
	handler.ServeHTTP(recGetOK, getWithKey)
	if recGetOK.Code != http.StatusOK {
		t.Errorf("GET status, remote, correct key: status = %d, want 200", recGetOK.Code)
	}
}

// Complements the direct-dispatch test above with the ordinary end-to-end
// path: a real loopback connection (what httptest.NewServer always presents)
// must keep working with a key configured, since loopback is exempt from the
// gate entirely.
func TestControlAPIGatesRemoteCallersWithoutAKeyOverLoopback(t *testing.T) {
	h := newRouterHarness(t, func(o *HandlerOptions) {
		o.APIKey = "secret"
	}, testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+ReservedPrefix+"/api/v1/status", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("loopback status call with a configured key = %d, want 200", res.StatusCode)
	}
}

// writeMutationError must distinguish the caller naming something that does
// not exist (view.ErrUnknownAccount, reported as 404) from every other
// failure a Source mutation can return, such as a config-store write error
// (reported as 500). Before this was fixed, every error was reported as 404
// regardless of cause.
func TestWriteMutationErrorDistinguishesUnknownAccountFromOtherFailures(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"unknown account", fmt.Errorf("%w: %q", view.ErrUnknownAccount, "nope"), http.StatusNotFound},
		{"other failure", errors.New("disk full"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeMutationError(rec, c.err)
			if rec.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, c.wantStatus)
			}
		})
	}
}

func TestControlAPIEventsStreamsACompletedRequest(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{"ok":true}`})

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+ReservedPrefix+"/api/v1/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}

	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	// Give the subscription a moment to register before generating traffic,
	// otherwise the event may be published before anyone is listening.
	time.Sleep(20 * time.Millisecond)

	go func() {
		http.Post(h.srv.URL+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"claude-sonnet-5"}`))
	}()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("event stream closed before the expected event arrived")
			}
			if strings.HasPrefix(line, "data:") && strings.Contains(line, "claude-sonnet-5") {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for an SSE event carrying the request's model")
		}
	}
}
