package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient points a Client at srv instead of github.com and pins the
// platform, so asset names are deterministic regardless of the host.
func newTestClient(t *testing.T, srv *httptest.Server, current string, opts ...Option) *Client {
	t.Helper()
	base := append([]Option{
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
		WithPlatform("linux", "amd64"),
	}, opts...)
	return New("owner/repo", current, base...)
}

// redirectServer answers /owner/repo/releases/latest with a 302 to loc, the
// way github.com does, and records how many requests it received.
func redirectServer(t *testing.T, loc string, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		if r.URL.Path != "/owner/repo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckResolvesTheTagFromTheRedirect(t *testing.T) {
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", nil)
	rel, err := newTestClient(t, srv, "0.1.0").Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rel.Version != "0.2.0" {
		t.Errorf("Version = %q, want 0.2.0", rel.Version)
	}
	if rel.Tag != "v0.2.0" {
		t.Errorf("Tag = %q, want v0.2.0", rel.Tag)
	}
	if rel.AssetName != "aiproxy_0.2.0_linux_amd64.tar.gz" {
		t.Errorf("AssetName = %q", rel.AssetName)
	}
	wantAsset := srv.URL + "/owner/repo/releases/download/v0.2.0/aiproxy_0.2.0_linux_amd64.tar.gz"
	if rel.AssetURL != wantAsset {
		t.Errorf("AssetURL = %q, want %q", rel.AssetURL, wantAsset)
	}
	wantSums := srv.URL + "/owner/repo/releases/download/v0.2.0/checksums.txt"
	if rel.ChecksumURL != wantSums {
		t.Errorf("ChecksumURL = %q, want %q", rel.ChecksumURL, wantSums)
	}
	if rel.PageURL != srv.URL+"/owner/repo/releases/tag/v0.2.0" {
		t.Errorf("PageURL = %q", rel.PageURL)
	}
}

// A repo with no releases redirects to /releases, with no /tag/ segment.
// That is a state, not a failure, and gets its own sentinel.
func TestCheckReportsNoReleases(t *testing.T) {
	srv := redirectServer(t, "/owner/repo/releases", nil)
	_, err := newTestClient(t, srv, "0.1.0").Check(context.Background())
	if !errors.Is(err, ErrNoReleases) {
		t.Fatalf("err = %v, want ErrNoReleases", err)
	}
}

// A dev build must not even reach the network: nothing it could learn would
// be actionable, and property 5 (honour the opt-out) is easier to trust when
// every no-op path is provably request-free.
func TestCheckRefusesADevBuildWithoutARequest(t *testing.T) {
	hits := 0
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", &hits)
	_, err := newTestClient(t, srv, "dev").Check(context.Background())
	if !errors.Is(err, ErrDevBuild) {
		t.Fatalf("err = %v, want ErrDevBuild", err)
	}
	if hits != 0 {
		t.Errorf("made %d requests, want 0", hits)
	}
}

func TestNewerComparesAgainstTheRunningVersion(t *testing.T) {
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", nil)
	c := newTestClient(t, srv, "0.1.0")
	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Newer(rel) {
		t.Error("0.2.0 should be newer than 0.1.0")
	}
	if newTestClient(t, srv, "0.2.0").Newer(rel) {
		t.Error("0.2.0 should not be newer than itself")
	}
}
