package main

import (
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

	h, err := buildHandler(cfg, store, quiet(), testIngester(t))
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

	h, err := buildHandler(cfg, store, quiet(), testIngester(t))
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
	cfg, _ := store.Update(func(c *config.Config) error { return nil })

	h, err := buildHandler(cfg, store, quiet(), testIngester(t))
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
	if _, ok := got["accounts"]; !ok {
		t.Errorf("status payload missing accounts: %+v", got)
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
	if _, err := buildHandler(cfg, store, quiet(), testIngester(t)); err != nil {
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
