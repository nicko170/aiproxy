package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

// The whole point of the stage: a real request through the real wiring lands as
// a row with the right token counts.
func TestRequestLandsInTheMetricsStore(t *testing.T) {
	began := time.Now().UnixMilli()
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "data: {\"type\":\"message_start\",\"message\":{\"usage\":" +
				"{\"input_tokens\":210,\"output_tokens\":1,\"cache_read_input_tokens\":3000," +
				"\"cache_creation_input_tokens\":12}}}\n\n"},
			{Delay: 10 * time.Millisecond,
				Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":88}}\n\n"},
		},
	})

	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	cfg, err := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "test", Upstream: up.URL(),
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	db, err := metrics.Open(filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ing := metrics.NewIngester(db, metrics.IngestOptions{})
	defer ing.Close()

	h, _, _, _, err := buildHandler(cfg, store, quiet(), ing)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if err := ing.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var model, outcome string
	var in, out, cr, cw, startedAt, ttfb, duration int64
	var status, stream int
	err = db.DB().QueryRow(`
SELECT model, outcome, status, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
       stream, started_at, ttfb_ms, duration_ms
FROM requests ORDER BY id DESC LIMIT 1`).
		Scan(&model, &outcome, &status, &in, &out, &cr, &cw, &stream, &startedAt, &ttfb, &duration)
	if err != nil {
		t.Fatalf("no row was written: %v", err)
	}

	if model != "claude-opus-5" || status != 200 || outcome != "ok" {
		t.Errorf("row = model %q / status %d / outcome %q", model, status, outcome)
	}
	if in != 210 || out != 88 || cr != 3000 || cw != 12 {
		t.Errorf("tokens = %d/%d/%d/%d, want 210/88/3000/12", in, out, cr, cw)
	}

	// These three columns went unasserted, which is how a row with started_at=0
	// and a stream flag that only meant "a body arrived" survived every gate.
	if startedAt < began || startedAt > time.Now().UnixMilli() {
		t.Errorf("started_at = %d, want a real timestamp within this test's run (>= %d)", startedAt, began)
	}
	if stream != 1 {
		t.Errorf("stream = %d, want 1 — the upstream answered text/event-stream", stream)
	}
	if ttfb < 0 {
		t.Errorf("ttfb_ms = %d, want >= 0 — a first byte was produced, so it must not carry the no-first-byte sentinel", ttfb)
	}
	if duration < ttfb {
		t.Errorf("duration_ms %d < ttfb_ms %d — a request cannot finish before its first byte", duration, ttfb)
	}

	// And it is queryable after a rollup.
	if err := metrics.RollupOnce(context.Background(), db, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	totals, err := db.Totals(context.Background(),
		metrics.Window{From: time.Now().Add(-time.Hour).UnixMilli(), To: time.Now().Add(time.Hour).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 1 || totals.OutputTokens != 88 {
		t.Errorf("totals = %+v, want 1 request / 88 out", totals)
	}
}

// A metrics failure must never surface to the client.
func TestMetricsFailureDoesNotFailTheRequest(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{Status: 200, Body: `{"ok":true}`})

	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	cfg, _ := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "t", Upstream: up.URL(),
			Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "k"},
		}}
		return nil
	})

	db, _ := metrics.Open(filepath.Join(dir, "metrics.db"))
	ing := metrics.NewIngester(db, metrics.IngestOptions{})
	// Close the store out from under the ingester: writes now fail.
	db.Close()

	h, _, _, _, err := buildHandler(cfg, store, quiet(), ing)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()

	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200 — a metrics failure must never reach the client", res.StatusCode)
	}
}

// newWiredProxy stands up the real composition — config store, handler,
// metrics store and ingester — against a scripted upstream, so a test can
// assert on the row the real OnResult path writes.
func newWiredProxy(t *testing.T, mutate func(*config.Config), scripts ...testutil.Script) (*httptest.Server, *metrics.Store, *metrics.Ingester) {
	t.Helper()
	up := testutil.NewFakeUpstream(t, scripts...)

	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	cfg, err := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "test", Upstream: up.URL(),
			Credential: provider.Credential{Type: provider.CredentialAPIKey, APIKey: "k"},
		}}
		if mutate != nil {
			mutate(c)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	db, err := metrics.Open(filepath.Join(dir, "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ing := metrics.NewIngester(db, metrics.IngestOptions{})
	t.Cleanup(func() { ing.Close() })

	h, _, _, _, err := buildHandler(cfg, store, quiet(), ing)
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, db, ing
}

func post(t *testing.T, srv *httptest.Server, body string) int {
	t.Helper()
	res, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

// The stream column must mean "the upstream answered with an event stream",
// not "a body arrived" — derived from Bytes > 0 it was a duplicate of
// bytes > 0 and every non-streaming JSON answer recorded stream=1.
func TestStreamColumnReflectsTheContentTypeNotTheByteCount(t *testing.T) {
	t.Run("non-streaming JSON records stream=0", func(t *testing.T) {
		srv, db, ing := newWiredProxy(t, nil, testutil.Script{
			Status: 200,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   `{"type":"message","usage":{"input_tokens":5,"output_tokens":2}}`,
		})
		if got := post(t, srv, `{"model":"claude-opus-5","messages":[]}`); got != 200 {
			t.Fatalf("status = %d, want 200", got)
		}
		if err := ing.Flush(); err != nil {
			t.Fatal(err)
		}

		var stream int
		var bytesSeen int64
		if err := db.DB().QueryRow(`
SELECT stream, input_tokens FROM requests ORDER BY id DESC LIMIT 1`).Scan(&stream, &bytesSeen); err != nil {
			t.Fatal(err)
		}
		if stream != 0 {
			t.Errorf("stream = %d, want 0 — a non-streaming JSON response is not a stream", stream)
		}
		// A body really did arrive, so this is not passing by accident: the old
		// Bytes > 0 derivation would have recorded 1 here.
		if bytesSeen == 0 {
			t.Error("the response carried no usage, so this test would pass under the buggy derivation too")
		}
	})

	t.Run("SSE records stream=1", func(t *testing.T) {
		srv, db, ing := newWiredProxy(t, nil, testutil.Script{
			Status: 200,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			SSE: []testutil.SSEChunk{{Data: "data: {\"type\":\"message_start\",\"message\":" +
				"{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"}},
		})
		if got := post(t, srv, `{"model":"claude-opus-5","stream":true,"messages":[]}`); got != 200 {
			t.Fatalf("status = %d, want 200", got)
		}
		if err := ing.Flush(); err != nil {
			t.Fatal(err)
		}

		var stream int
		if err := db.DB().QueryRow(`SELECT stream FROM requests ORDER BY id DESC LIMIT 1`).Scan(&stream); err != nil {
			t.Fatal(err)
		}
		if stream != 1 {
			t.Errorf("stream = %d, want 1", stream)
		}
	})
}

// A blocked model is refused locally, and that refusal has to be findable. With
// started_at left at zero the row was invisible to every window query, rolled
// into bucket 0, deleted by the first prune, and dragged the TTFB percentile
// toward zero — while reporting itself as "ok".
func TestBlockedRequestProducesAQueryableRowWithOutcomeBlocked(t *testing.T) {
	began := time.Now().UnixMilli()
	srv, db, ing := newWiredProxy(t,
		func(c *config.Config) { c.Routing.BlockedModels = []string{"*fable*"} },
		testutil.Script{Status: 200, Body: `{"ok":true}`})

	if got := post(t, srv, `{"model":"claude-fable-5","messages":[]}`); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
	if err := ing.Flush(); err != nil {
		t.Fatal(err)
	}

	var outcome string
	var status, stream int
	var startedAt, ttfb int64
	err := db.DB().QueryRow(`
SELECT outcome, status, stream, started_at, ttfb_ms FROM requests ORDER BY id DESC LIMIT 1`).
		Scan(&outcome, &status, &stream, &startedAt, &ttfb)
	if err != nil {
		t.Fatalf("no row was written for a blocked request: %v", err)
	}
	if outcome != "blocked" {
		t.Errorf("outcome = %q, want %q — a refusal must not be recorded as a success", outcome, "blocked")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if stream != 0 {
		t.Errorf("stream = %d, want 0", stream)
	}
	if ttfb != -1 {
		t.Errorf("ttfb_ms = %d, want -1 — no first byte was produced, and the percentile queries exclude negatives", ttfb)
	}
	if startedAt < began {
		t.Fatalf("started_at = %d, want a real timestamp (>= %d)", startedAt, began)
	}

	// The point of stamping started_at: the row is reachable through the window
	// queries stage 3 is built on.
	w := metrics.Window{From: began - time.Minute.Milliseconds(), To: time.Now().Add(time.Minute).UnixMilli()}
	totals, err := db.Totals(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 1 {
		t.Errorf("Totals over the window = %d requests, want 1 — the blocked row is invisible to window queries", totals.Requests)
	}

	series, err := db.UsageSeries(context.Background(), metrics.SeriesQuery{
		Window: w, Granularity: metrics.GranularityMinute, GroupBy: metrics.GroupByOutcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range series.Points {
		if p.Key == "blocked" {
			found = true
		}
	}
	if !found {
		t.Errorf("no \"blocked\" point in the outcome breakdown: %+v", series.Points)
	}

	// And it must not pollute the TTFB percentile spec §2.1 exists to expose.
	lat, err := db.LatencyPercentiles(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	if lat.TTFBP50 != 0 {
		t.Errorf("TTFB p50 = %d over a window whose only row produced no first byte, want 0 (excluded)", lat.TTFBP50)
	}
}
