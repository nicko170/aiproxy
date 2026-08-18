package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/updater"
)

// latestServer answers the redirect the version lookup follows. It serves no
// asset, which is all --check needs.
func latestServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/owner/repo/releases/latest" {
			w.Header().Set("Location", "/owner/repo/releases/tag/"+tag)
			w.WriteHeader(http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeClientFor(srv *httptest.Server, exe string) func(string) *updater.Client {
	return func(current string) *updater.Client {
		return updater.New("owner/repo", current,
			updater.WithBaseURL(srv.URL),
			updater.WithHTTPClient(srv.Client()),
			updater.WithPlatform("linux", "amd64"),
			updater.WithExecPath(func() (string, error) { return exe, nil }),
		)
	}
}

// --check exits 1 when an update exists, so a wrapper script can act on it
// without parsing prose.
func TestUpdateCheckExitsOneWhenAnUpdateIsAvailable(t *testing.T) {
	srv := latestServer(t, "v0.2.0")
	var out bytes.Buffer
	code := runUpdate([]string{"--check"}, "0.1.0", &out, fakeClientFor(srv, ""))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "0.2.0") {
		t.Errorf("output = %q, want the available version", out.String())
	}
}

func TestUpdateCheckExitsZeroWhenCurrent(t *testing.T) {
	srv := latestServer(t, "v0.1.0")
	var out bytes.Buffer
	if code := runUpdate([]string{"--check"}, "0.1.0", &out, fakeClientFor(srv, "")); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

// --check must never install anything, whatever it finds.
func TestUpdateCheckDoesNotInstall(t *testing.T) {
	srv := latestServer(t, "v0.2.0")
	exe := filepath.Join(t.TempDir(), "aiproxy")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runUpdate([]string{"--check"}, "0.1.0", &out, fakeClientFor(srv, exe))
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Errorf("--check modified the binary: %q", got)
	}
}

func TestUpdateOnADevBuildExplainsAndFails(t *testing.T) {
	srv := latestServer(t, "v0.2.0")
	var out bytes.Buffer
	code := runUpdate(nil, "dev", &out, fakeClientFor(srv, ""))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "dev build") {
		t.Errorf("output = %q, want it to name the problem", out.String())
	}
}

func TestUpdateRejectsUnknownFlags(t *testing.T) {
	var out bytes.Buffer
	if code := runUpdate([]string{"--nope"}, "0.1.0", &out, fakeClientFor(latestServer(t, "v0.2.0"), "")); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
