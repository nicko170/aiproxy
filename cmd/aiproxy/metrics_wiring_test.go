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

	h, err := buildHandler(cfg, store, quiet(), ing)
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
	var in, out, cr, cw int64
	var status int
	err = db.DB().QueryRow(`
SELECT model, outcome, status, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
FROM requests ORDER BY id DESC LIMIT 1`).
		Scan(&model, &outcome, &status, &in, &out, &cr, &cw)
	if err != nil {
		t.Fatalf("no row was written: %v", err)
	}

	if model != "claude-opus-5" || status != 200 || outcome != "ok" {
		t.Errorf("row = model %q / status %d / outcome %q", model, status, outcome)
	}
	if in != 210 || out != 88 || cr != 3000 || cw != 12 {
		t.Errorf("tokens = %d/%d/%d/%d, want 210/88/3000/12", in, out, cr, cw)
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

	h, err := buildHandler(cfg, store, quiet(), ing)
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
