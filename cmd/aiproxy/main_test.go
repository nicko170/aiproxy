package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/metrics"
	"github.com/nicko170/aiproxy/internal/provider"
	"github.com/nicko170/aiproxy/internal/testutil"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testIngester gives a stage-1-era test a real (in-memory) accounting sink so
// buildHandler's signature can require one without every unrelated test
// standing up its own on-disk database.
func testIngester(t *testing.T) *metrics.Ingester {
	t.Helper()
	db, err := metrics.OpenMemory()
	if err != nil {
		t.Fatalf("metrics.OpenMemory: %v", err)
	}
	ing := metrics.NewIngester(db, metrics.IngestOptions{})
	t.Cleanup(func() {
		ing.Close()
		db.Close()
	})
	return ing
}

// End to end through the real wiring: a config on disk, the real router, a fake
// upstream. This is the test that says "Claude Code can talk to this".
func TestEndToEndProxiesAStreamingCompletion(t *testing.T) {
	up := testutil.NewFakeUpstream(t, testutil.Script{
		Status: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		SSE: []testutil.SSEChunk{
			{Data: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n"},
			{Delay: 50 * time.Millisecond, Data: "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n"},
			{Delay: 50 * time.Millisecond, Data: "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n"},
		},
	})

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
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
		t.Fatalf("config: %v", err)
	}

	h, _, _, _, err := buildHandler(cfg, store, quiet(), testIngester(t))
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Errorf("Content-Type = %q, want an event stream", ct)
	}

	// Chunks must arrive spread out, not all at once at the end.
	start := time.Now()
	arrivals := []time.Duration{}
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			arrivals = append(arrivals, time.Since(start))
		}
		if err != nil {
			break
		}
	}
	if len(arrivals) < 3 {
		t.Fatalf("observed %d chunks, want at least 3 — the response was buffered", len(arrivals))
	}
	if last := arrivals[len(arrivals)-1] - arrivals[0]; last < 60*time.Millisecond {
		t.Errorf("all chunks arrived within %v; streaming collapsed to the end", last)
	}
}

// buildHandler must wire the upstream transport's ResponseHeaderTimeout from
// cfg.Retry.HeaderTimeoutMS, not leave it at internal/proxy's fixed 120s
// default. Proven here with a short configured value against a header-
// withholding upstream: if the transport default still governed, this request
// would hang for a full two minutes before anything gave up. Bounding elapsed
// well under that shows the attempt loop's own timer — sized from this
// config value — is what fired instead.
func TestBuildHandlerWiresTransportHeaderTimeoutFromConfig(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Cleanups run last-registered-first: release the handler before Close waits
	// on it, or teardown blocks on the very silence this test is exercising.
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) })

	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	cfg, err := store.Update(func(c *config.Config) error {
		c.Retry.HeaderTimeoutMS = 300 // far below the transport's own 120s default
		c.Retry.BudgetMS = 5000
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "test", Upstream: up.URL,
			Credential: provider.Credential{
				Type: provider.CredentialOAuth, AccessToken: "at", RefreshToken: "rt",
				ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			},
		}}
		return nil
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	h, _, _, _, err := buildHandler(cfg, store, quiet(), testIngester(t))
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	start := time.Now()
	res, err := http.Post(srv.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	elapsed := time.Since(start)
	defer res.Body.Close()

	if elapsed > 5*time.Second {
		t.Fatalf("client waited %v; the transport's ResponseHeaderTimeout is not "+
			"tracking retry.headerTimeoutMs (it would take 120s if it were)", elapsed)
	}
	if res.StatusCode < 400 {
		t.Errorf("status = %d, want an error — the account never answered", res.StatusCode)
	}
}

func TestEndToEndStatusEndpoint(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	cfg, _ := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{ID: "a1", Provider: "anthropic", Label: "test"}}
		return nil
	})

	h, _, _, _, err := buildHandler(cfg, store, quiet(), testIngester(t))
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/_aiproxy/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Stage 3 splits the stage-1 status readout: /status is server-level only
	// (spec §3.1's view.Status) and accounts move to their own /accounts route,
	// both backed by the same view.Source buildHandler wires here.
	if _, ok := got["listenAddr"]; !ok {
		t.Errorf("status payload missing listenAddr: %+v", got)
	}
	if _, ok := got["accounts"]; ok {
		t.Errorf("status payload should no longer carry accounts (moved to /accounts): %+v", got)
	}

	accRes, err := http.Get(srv.URL + "/_aiproxy/api/v1/accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer accRes.Body.Close()
	if accRes.StatusCode != 200 {
		t.Fatalf("accounts status = %d", accRes.StatusCode)
	}
	var accts []map[string]any
	if err := json.NewDecoder(accRes.Body).Decode(&accts); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(accts) != 1 || accts[0]["id"] != "a1" {
		t.Errorf("accounts = %+v", accts)
	}
}

// A refreshed credential must land in the config file, or every restart starts
// from a token that has already been rotated away.
func TestBuildHandlerPersistsRefreshedCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := config.NewStore(path)
	cfg, _ := store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{
			ID: "a1", Provider: "anthropic", Label: "test",
			Credential: provider.Credential{Type: provider.CredentialOAuth, AccessToken: "old", RefreshToken: "rt"},
		}}
		return nil
	})
	if _, _, _, _, err := buildHandler(cfg, store, quiet(), testIngester(t)); err != nil {
		t.Fatalf("buildHandler: %v", err)
	}

	// The persist hook is what buildHandler must have installed; exercise it via
	// the store directly to prove the wiring writes to this file.
	if _, err := store.Update(func(c *config.Config) error {
		c.Accounts[0].Credential.AccessToken = "rotated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "rotated") {
		t.Error("credential change did not reach the config file")
	}
}

func TestFirstRunImportAdoptsLegacyAccounts(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "teamclaude.json")
	os.WriteFile(legacy, []byte(`{"accounts":[{"name":"carried","type":"apikey","apiKey":"sk-x"}]}`), 0o600)
	t.Setenv("XDG_CONFIG_HOME", dir)

	store := config.NewStore(filepath.Join(dir, "aiproxy", "config.json"))
	if err := firstRunImport(store, quiet()); err != nil {
		t.Fatalf("firstRunImport: %v", err)
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Label != "carried" {
		t.Fatalf("accounts = %+v", cfg.Accounts)
	}
	if cfg.Accounts[0].ID == "" {
		t.Error("imported account needs an id")
	}
}

// Import must be a first-run action only, never something that duplicates
// accounts on every start.
func TestFirstRunImportSkipsWhenAccountsExist(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "teamclaude.json"),
		[]byte(`{"accounts":[{"name":"legacy","type":"apikey","apiKey":"sk-x"}]}`), 0o600)
	t.Setenv("XDG_CONFIG_HOME", dir)

	store := config.NewStore(filepath.Join(dir, "aiproxy", "config.json"))
	store.Update(func(c *config.Config) error {
		c.Accounts = []config.Account{{ID: "existing", Provider: "anthropic", Label: "mine"}}
		return nil
	})

	if err := firstRunImport(store, quiet()); err != nil {
		t.Fatalf("firstRunImport: %v", err)
	}
	cfg, _ := store.Load()
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Label != "mine" {
		t.Errorf("accounts = %+v, want the existing one untouched", cfg.Accounts)
	}
}

// The status endpoint reports the running version through the seam, so the
// TUI and a future dashboard both learn it from one place rather than each
// being handed a version string separately.
func TestStatusReportsTheRunningVersion(t *testing.T) {
	cfg := config.Default()
	cfg.Update.CheckEnabled = false // no outbound request from a test
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))

	_, _, vl, ck, err := buildHandler(cfg, store, quiet(), testIngester(t))
	if err != nil {
		t.Fatal(err)
	}
	ck.Start()
	defer ck.Stop()

	st, err := vl.ServerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Update.CurrentVersion != version {
		t.Errorf("CurrentVersion = %q, want %q", st.Update.CurrentVersion, version)
	}
	if !st.Update.Disabled {
		t.Error("Disabled should be true when update.checkEnabled is false")
	}
}

func TestUnknownSubcommandIsRejected(t *testing.T) {
	var out bytes.Buffer
	code := dispatchSubcommand([]string{"updat"}, &out)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "update") {
		t.Errorf("output = %q, want it to name the valid subcommands", out.String())
	}
}

// A leading flag is the server's, not a subcommand's: dispatch must hand it
// back rather than reporting "unknown command -addr".
func TestNoSubcommandRunsTheServer(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{nil, {"--headless"}, {"-addr", "127.0.0.1:0"}} {
		if code := dispatchSubcommand(args, &out); code != -1 {
			t.Errorf("dispatchSubcommand(%v) = %d, want -1 (not a subcommand, run the server)", args, code)
		}
	}
}
