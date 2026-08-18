# In-App Update Checking and Self-Update Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a released `aiproxy` binary tell its operator when a newer release exists and replace itself with it, from the TUI or from `aiproxy update`.

**Architecture:** A new leaf package `internal/updater` resolves the latest GitHub release through the `/releases/latest` redirect, downloads the platform tarball, verifies it against the release's `checksums.txt`, and swaps the running binary by `os.Rename` inside its own directory. A background `updater.Checker` caches the answer on a long cadence with the same `Start()`/`Stop()` lifecycle as `metrics.Roller` and `prober.Prober`. Availability rides on `view.Status.Update` — the same precedent as `Status.Probe` — so the TUI's existing 2s status poll renders it with no new poll and no new read route. Exactly one new `view.Source` method, `ApplyUpdate`, is added, routed at `POST /_aiproxy/api/v1/update`.

**Tech Stack:** Go 1.26.5, standard library only (`net/http`, `archive/tar`, `compress/gzip`, `crypto/sha256`) — **no new module dependency**. Bubble Tea TUI with golden-frame tests. chi router for the control API.

**Spec:** `docs/superpowers/specs/2026-08-18-self-update-design.md`

## Global Constraints

- **No new go.mod dependency.** Version comparison, tar/gzip extraction, and hashing all use the standard library.
- **Go 1.26.5**, module `github.com/nicko170/aiproxy`.
- **A failed update never damages the installed binary.** Verification happens before anything is moved into place, never after. Every temp file is removed on every error path.
- **No unverified bytes are ever made executable.** The archive is checked against `checksums.txt` before it is unpacked.
- **The check never blocks the request path or the render path.** It runs on its own goroutine and writes to a cache; `ServerStatus` reads that cache. No `Source` call ever performs a network round trip to github.com.
- **The check is opt-out and honours the opt-out.** With `update.checkEnabled` false, zero outbound requests are made.
- **Windows is out of scope.** Only darwin and linux ship; `os.Rename` over a running binary is a unix guarantee.
- **Asset naming is load-bearing and fixed:** `aiproxy_<version>_<os>_<arch>.tar.gz` plus `checksums.txt`, both under `https://github.com/<repo>/releases/download/<tag>/`. The same strings are constructed in `.github/workflows/release.yml` and `install.sh`. Versions are the tag minus its leading `v` (`v0.2.0` → `0.2.0`); an unstamped build is the literal string `dev`.
- **`internal/tui` may import only `internal/view`** (enforced by `TestTUIImportsOnlyTheViewSeam`). `internal/view` and `internal/proxy` may import `internal/updater`; `internal/updater` imports nothing from this module.
- **Every `view.Source` method needs a route** in `internal/proxy/router.go` and an entry in `routeFor` in `internal/proxy/lockstep_test.go` (enforced by `TestEveryViewSourceMethodHasAControlRoute`).
- Run the full suite with `go test ./...` from the repo root. Golden frames are regenerated with `go test ./internal/tui -run TestGoldenFrames -update` and the resulting diff must be read, not just accepted.

## File Structure

**Created:**
- `internal/updater/version.go` — `Compare`, `parseVersion`. Version ordering only.
- `internal/updater/version_test.go`
- `internal/updater/updater.go` — `Client`, `Release`, `Result`, options, the error sentinels, `Check`, `Apply`, and the download/verify/extract helpers.
- `internal/updater/updater_test.go` — end-to-end against an `httptest.Server` serving a real tarball.
- `internal/updater/checker.go` — `Checker`, `State`, the background loop, the cache.
- `internal/updater/checker_test.go`

**Modified:**
- `internal/config/config.go` — `Update` block, `Default()`, and the `loadLocked` sanity guard in `internal/config/store.go`.
- `internal/view/types.go` — `UpdateStatus`, `UpdateResult`, `Status.Update`, two `Settings` fields.
- `internal/view/settings.go` — validate the new interval.
- `internal/view/source.go` — the `ApplyUpdate` method on the interface.
- `internal/view/local.go` — the `updates` field, `NewLocal` parameter, `updateStatus()`, `ApplyUpdate`, settings plumbing.
- `internal/proxy/control.go` — `applyUpdateHandler`, `writeUpdateError`.
- `internal/proxy/router.go` — the `POST /api/v1/update` route.
- `internal/proxy/lockstep_test.go` — the `ApplyUpdate` entry in `routeFor`.
- `cmd/aiproxy/main.go` — construct the `Client` and `Checker`, `Start()`/`defer Stop()`, pass into `view.NewLocal`, and dispatch the `update` subcommand.
- `internal/tui/app.go` — header segment, `u` key, `applyUpdate` command, `updateAppliedMsg`, footer and help.
- `internal/tui/settings.go` — two new setting rows.
- `internal/tui/frames_test.go` — two new frame cases.
- `README.md` — an Updating section.

---

### Task 1: Version comparison

**Files:**
- Create: `internal/updater/version.go`
- Test: `internal/updater/version_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `updater.Compare(a, b string) int` — negative when `a` sorts below `b`, 0 when equal, positive when above. Ignores a leading `v` on either side. Unparseable input sorts below anything parseable; two unparseable values are equal.

- [ ] **Step 1: Write the failing test**

Create `internal/updater/version_test.go`:

```go
package updater

import "testing"

// TestCompare pins the ordering rules the header and the install path both
// depend on. 0.10.0 vs 0.9.0 is the case a string comparison gets wrong, and
// it is the reason this function exists at all.
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"0.1.0", "v0.1.0", 0},
		{"0.2.0", "0.1.0", 1},
		{"0.1.0", "0.2.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.1.10", "0.1.9", 1},
		// A prerelease sorts below its release.
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc1", 1},
		{"1.0.0-rc1", "1.0.0-rc2", -1},
		{"1.0.0-rc1", "1.0.0-rc1", 0},
		// A prerelease of a higher version still beats a lower release.
		{"1.1.0-rc1", "1.0.0", 1},
		// Unparseable sorts below anything parseable, and equals itself.
		{"dev", "0.1.0", -1},
		{"0.1.0", "dev", 1},
		{"dev", "dev", 0},
		{"", "0.1.0", -1},
		{"1.2", "1.2.0", -1},
		{"1.2.x", "1.2.0", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/updater -run TestCompare`
Expected: FAIL — the package does not build, `undefined: Compare`.

- [ ] **Step 3: Write the implementation**

Create `internal/updater/version.go`:

```go
// Package updater checks for newer aiproxy releases and replaces the running
// binary with one.
//
// It is deliberately a leaf: it imports the standard library and nothing else
// in this module, which is what lets the whole download-verify-swap path be
// tested end to end against an httptest.Server with no proxy running. The
// packages above it (internal/view for the presentation seam, cmd/aiproxy for
// the CLI) depend on it; it depends on none of them.
package updater

import (
	"strconv"
	"strings"
)

// devVersion is what main.version holds in a build that was not stamped by
// the release workflow. Such a build has no defensible comparison point, so
// it is never offered an update (see ErrDevBuild).
const devVersion = "dev"

// version is a parsed major.minor.patch with an optional prerelease suffix.
// ok is false for anything this project does not produce.
type version struct {
	major, minor, patch int
	pre                 string
	ok                  bool
}

// parseVersion reads "0.2.0", "v0.2.0", or "1.0.0-rc1". Build metadata
// ("+sha") is not accepted: the release workflow never produces it, and
// treating an unknown shape as unparseable is safer than guessing at an
// ordering for it.
func parseVersion(s string) version {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}
		}
		nums[i] = n
	}
	return version{major: nums[0], minor: nums[1], patch: nums[2], pre: pre, ok: true}
}

// Compare orders two aiproxy versions: negative when a sorts below b, zero
// when they are equal, positive when a sorts above.
//
// This is a bounded comparator for the versions this project's release
// workflow produces, not a general semver implementation, and it exists so
// that adding update checking takes no new dependency. Two rules are
// load-bearing: numeric fields compare numerically (so 0.10.0 > 0.9.0, which
// a string comparison gets backwards), and a prerelease sorts BELOW its
// release (1.0.0-rc1 < 1.0.0). Anything unparseable — "dev" above all —
// sorts below anything parseable, so an unstamped build is never told it is
// ahead of a real release.
func Compare(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	switch {
	case !pa.ok && !pb.ok:
		return 0
	case !pa.ok:
		return -1
	case !pb.ok:
		return 1
	}
	for _, d := range [...]int{pa.major - pb.major, pa.minor - pb.minor, pa.patch - pb.patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1
	case pb.pre == "":
		return -1
	}
	return sign(strings.Compare(pa.pre, pb.pre))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/updater -run TestCompare -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/updater/version.go internal/updater/version_test.go
git commit -m "feat(updater): compare aiproxy release versions"
```

---

### Task 2: Resolving the latest release

**Files:**
- Create: `internal/updater/updater.go`
- Test: `internal/updater/updater_test.go`

**Interfaces:**
- Consumes: `Compare`, `devVersion` from Task 1.
- Produces:
  - `const DefaultRepo = "nicko170/aiproxy"`
  - `type Release struct { Version, Tag, PageURL, AssetName, AssetURL, ChecksumURL string }`
  - `type Client struct{ ... }` with `func New(repo, current string, opts ...Option) *Client`
  - `Option` values: `WithHTTPClient(*http.Client)`, `WithBaseURL(string)`, `WithPlatform(goos, goarch string)`, `WithExecPath(func() (string, error))`
  - `func (c *Client) Check(ctx context.Context) (Release, error)`
  - `func (c *Client) Current() string`
  - `func (c *Client) Newer(rel Release) bool`
  - Sentinels: `ErrDevBuild`, `ErrNoReleases`, `ErrUpToDate`, `ErrNotWritable`, `ErrChecksumMismatch`, `ErrUpdateInProgress`

- [ ] **Step 1: Write the failing test**

Create `internal/updater/updater_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/updater -run 'TestCheck|TestNewer'`
Expected: FAIL — `undefined: New`, `undefined: Release`, and so on.

- [ ] **Step 3: Write the implementation**

Create `internal/updater/updater.go`:

```go
package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultRepo is the GitHub repository releases are published to. It is a
// constant rather than configurable: an updater that can be pointed at an
// arbitrary repository is an arbitrary-code-execution switch, and nothing in
// this product needs one. Tests override the host with WithBaseURL, which
// changes where the bytes come from but not whose they are.
const DefaultRepo = "nicko170/aiproxy"

const defaultBaseURL = "https://github.com"

// binaryName is the archive member holding the executable, matching what
// .github/workflows/release.yml puts there ("tar -C build aiproxy").
const binaryName = "aiproxy"

// Sentinels. Each layer above maps these to its own idiom (an HTTP status, a
// TUI flash, an exit code) rather than re-deriving the cause from a message
// string; see the table in the design spec's error-handling section.
var (
	// ErrDevBuild means the running binary was not stamped by the release
	// workflow, so there is no version to compare against.
	ErrDevBuild = errors.New("dev build: install a release to update in place")
	// ErrNoReleases means the repository has published no releases yet.
	ErrNoReleases = errors.New("no releases published yet")
	// ErrUpToDate means the latest release is not newer than what is running.
	ErrUpToDate = errors.New("already up to date")
	// ErrNotWritable means the directory holding the running binary cannot be
	// written to, so the swap could never complete. Reported before anything
	// is downloaded.
	ErrNotWritable = errors.New("install directory is not writable")
	// ErrChecksumMismatch means the downloaded archive did not match the
	// release's checksums.txt, or was not listed in it at all. The installed
	// binary is untouched when this is returned.
	ErrChecksumMismatch = errors.New("download failed checksum verification")
	// ErrUpdateInProgress means another Apply is already running. Returned by
	// the seam, not by Apply itself (see view.Local.ApplyUpdate).
	ErrUpdateInProgress = errors.New("an update is already running")
)

// Release is one published release and every URL derived from its tag. The
// asset name is constructed here, and identically in install.sh and the
// release workflow — that string is load-bearing across all three.
type Release struct {
	Version     string // "0.2.0" — the tag without its leading v
	Tag         string // "v0.2.0"
	PageURL     string
	AssetName   string
	AssetURL    string
	ChecksumURL string
}

// Result reports what Apply did, so the TUI flash and the CLI print the same
// facts from the same place.
type Result struct {
	Updated         bool
	PreviousVersion string
	Version         string
	Path            string
}

// Client resolves and installs releases. current is the running binary's
// version ("dev" when unstamped).
type Client struct {
	repo    string
	current string
	baseURL string
	goos    string
	goarch  string

	// http follows redirects, because a release asset download redirects to
	// object storage. Check needs the OPPOSITE behaviour and builds its own
	// no-redirect client over this one's Transport; see Check.
	http     *http.Client
	execPath func() (string, error)
}

// Option configures a Client at construction.
type Option func(*Client)

// WithHTTPClient overrides the client used for asset downloads. Its
// CheckRedirect must follow redirects; Check derives a non-following client
// from its Transport rather than reusing its redirect policy.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithBaseURL overrides "https://github.com", so tests can serve the whole
// release surface from an httptest.Server.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithPlatform overrides runtime.GOOS/GOARCH, so a test asserts one asset
// name rather than whichever the host happens to be.
func WithPlatform(goos, goarch string) Option {
	return func(c *Client) { c.goos, c.goarch = goos, goarch }
}

// WithExecPath overrides how Apply locates the binary to replace. Tests point
// it at a temp file; production resolves os.Executable through its symlinks.
func WithExecPath(fn func() (string, error)) Option {
	return func(c *Client) { c.execPath = fn }
}

// New builds a Client for repo (e.g. DefaultRepo), reporting current as the
// running version.
func New(repo, current string, opts ...Option) *Client {
	c := &Client{
		repo:    repo,
		current: current,
		baseURL: defaultBaseURL,
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
		// Generous but bounded: a release tarball over a slow link is minutes,
		// a hung connection must not be forever.
		http:     &http.Client{Timeout: 10 * time.Minute},
		execPath: resolveExecPath,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Current is the running version this Client compares against.
func (c *Client) Current() string { return c.current }

// Newer reports whether rel is a version worth installing.
func (c *Client) Newer(rel Release) bool { return Compare(rel.Version, c.current) > 0 }

// Check reports what the latest release is. It never returns ErrUpToDate:
// comparing against the running version is the caller's job (Checker for the
// header, Apply for the install), so one lookup serves both without either
// having to read a success out of an error.
//
// Resolution uses GitHub's own redirect rather than its API:
//
//	GET /<repo>/releases/latest  ->  302  Location: /<repo>/releases/tag/v0.2.0
//
// That means no token, no 60-request/hour unauthenticated rate limit, and no
// coupling to the shape of GitHub's release JSON. install.sh resolves the
// version exactly this way, for exactly these reasons.
func (c *Client) Check(ctx context.Context) (Release, error) {
	if c.current == devVersion {
		return Release{}, ErrDevBuild
	}

	url := c.baseURL + "/" + c.repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}

	// A dedicated non-following client: c.http MUST follow redirects for the
	// asset download (GitHub sends those to object storage), so the redirect
	// policy cannot simply be set on it. Sharing the Transport keeps
	// connection reuse and any test-server TLS config.
	noRedirect := &http.Client{
		Transport: c.http.Transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("resolve latest release: %w", err)
	}
	defer resp.Body.Close()
	// Drain a little so the connection can be reused; a redirect body is a
	// short HTML stub.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return Release{}, fmt.Errorf("resolve latest release: unexpected status %d", resp.StatusCode)
	}
	tag, ok := tagFromLocation(loc)
	if !ok {
		return Release{}, ErrNoReleases
	}
	return c.release(tag), nil
}

// tagFromLocation pulls "v0.2.0" out of ".../releases/tag/v0.2.0". A repo
// with no releases redirects to ".../releases" instead, which has no /tag/
// segment — that is what ok=false means here.
func tagFromLocation(loc string) (string, bool) {
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", false
	}
	tag := strings.Trim(loc[i+len("/tag/"):], "/")
	if tag == "" {
		return "", false
	}
	return tag, true
}

// release derives every URL from a tag. The asset name must match
// .github/workflows/release.yml's tar name and install.sh's construction of
// it exactly; all three build "aiproxy_<version>_<os>_<arch>.tar.gz".
func (c *Client) release(tag string) Release {
	v := strings.TrimPrefix(tag, "v")
	asset := fmt.Sprintf("%s_%s_%s_%s.tar.gz", binaryName, v, c.goos, c.goarch)
	base := c.baseURL + "/" + c.repo + "/releases"
	return Release{
		Version:     v,
		Tag:         tag,
		PageURL:     base + "/tag/" + tag,
		AssetName:   asset,
		AssetURL:    base + "/download/" + tag + "/" + asset,
		ChecksumURL: base + "/download/" + tag + "/checksums.txt",
	}
}

// resolveExecPath is the running binary's real path: os.Executable, then
// through any symlink, so an update replaces what is actually running rather
// than a link pointing at it. A symlink that cannot be resolved falls back to
// the unresolved path instead of failing the whole update.
func resolveExecPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/updater -v`
Expected: PASS (both `TestCompare` and the four new tests)

- [ ] **Step 5: Commit**

```bash
git add internal/updater/updater.go internal/updater/updater_test.go
git commit -m "feat(updater): resolve the latest release through GitHub's redirect"
```

---

### Task 3: Downloading, verifying, and swapping the binary

**Files:**
- Modify: `internal/updater/updater.go` (append to the file created in Task 2)
- Test: `internal/updater/apply_test.go`

**Interfaces:**
- Consumes: `Client`, `Release`, `Result`, `Option`, the sentinels, `Compare` (Tasks 1–2).
- Produces: `func (c *Client) Apply(ctx context.Context, rel Release) (Result, error)`.

**Order matters and is the whole point.** Refuse a dev build, resolve the exec path, probe the directory for writability *before downloading anything*, download, verify against `checksums.txt`, only then unpack, chmod, and `os.Rename`. A failure at any step must leave the installed binary byte-identical.

- [ ] **Step 1: Write the failing test**

Create `internal/updater/apply_test.go`:

```go
package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// releaseFixture is a whole fake release: one tarball holding a binary, plus
// the checksums.txt that vouches for it.
type releaseFixture struct {
	assetName string
	archive   []byte
	checksums string
	// hits counts requests per path, so a test can assert that a refusal
	// happened BEFORE the download rather than after it.
	hits map[string]int
}

// tarGz builds a real gzipped tar with the given members, because the whole
// value of this test is that the extraction path is exercised for real
// rather than against a mock reader.
func tarGz(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range members {
		hdr := &tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newReleaseFixture builds a release for version v whose binary contains
// body. corrupt makes checksums.txt vouch for different bytes than the asset
// actually contains; omitFromChecksums leaves the asset out of the file.
func newReleaseFixture(t *testing.T, v, body string, corrupt, omitFromChecksums bool) *releaseFixture {
	t.Helper()
	asset := fmt.Sprintf("aiproxy_%s_linux_amd64.tar.gz", v)
	archive := tarGz(t, map[string][]byte{
		"aiproxy":  []byte(body),
		"LICENSE":  []byte("MIT"),
		"README.md": []byte("# aiproxy"),
	})
	digest := sha256Hex(archive)
	if corrupt {
		digest = sha256Hex([]byte("something else entirely"))
	}
	sums := ""
	if !omitFromChecksums {
		// sha256sum's own format: digest, two spaces, filename.
		sums = digest + "  " + asset + "\n"
	}
	sums += sha256Hex([]byte("unrelated")) + "  aiproxy_" + v + "_darwin_arm64.tar.gz\n"
	return &releaseFixture{assetName: asset, archive: archive, checksums: sums, hits: map[string]int{}}
}

// serve returns a server answering the three URLs a release needs: the
// latest-release redirect, the asset, and checksums.txt.
func (f *releaseFixture) serve(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits[r.URL.Path]++
		switch r.URL.Path {
		case "/owner/repo/releases/latest":
			w.Header().Set("Location", "/owner/repo/releases/tag/"+tag)
			w.WriteHeader(http.StatusFound)
		case "/owner/repo/releases/download/" + tag + "/" + f.assetName:
			w.Write(f.archive)
		case "/owner/repo/releases/download/" + tag + "/checksums.txt":
			w.Write([]byte(f.checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// installedBinary writes a stand-in for the running binary into its own
// directory and returns its path.
func installedBinary(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "aiproxy")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func applyClient(t *testing.T, srv *httptest.Server, current, exe string) *Client {
	t.Helper()
	return New("owner/repo", current,
		WithBaseURL(srv.URL),
		WithHTTPClient(srv.Client()),
		WithPlatform("linux", "amd64"),
		WithExecPath(func() (string, error) { return exe, nil }),
	)
}

func TestApplyReplacesTheBinary(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW BINARY", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD BINARY")
	c := applyClient(t, srv, "0.1.0", exe)

	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Apply(context.Background(), rel)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Updated || res.Version != "0.2.0" || res.PreviousVersion != "0.1.0" || res.Path != exe {
		t.Errorf("Result = %+v", res)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW BINARY" {
		t.Errorf("binary content = %q, want %q", got, "NEW BINARY")
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", fi.Mode().Perm())
	}
	// No temp files left beside it.
	assertNoLeftovers(t, filepath.Dir(exe))
}

// The mode of the binary being replaced is preserved, not reset to a default:
// an operator who tightened permissions must not have them widened by an
// update.
func TestApplyPreservesTheExistingMode(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD")
	if err := os.Chmod(exe, 0o700); err != nil {
		t.Fatal(err)
	}
	c := applyClient(t, srv, "0.1.0", exe)
	rel, _ := c.Check(context.Background())
	if _, err := c.Apply(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(exe)
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", fi.Mode().Perm())
	}
}

// Property 1, the one that matters most: every failure leaves the installed
// binary byte-for-byte identical, and leaves no debris behind it.
func TestApplyFailuresLeaveTheBinaryUntouched(t *testing.T) {
	cases := []struct {
		name    string
		build   func(t *testing.T) *releaseFixture
		wantErr error
	}{
		{
			name:    "checksum mismatch",
			build:   func(t *testing.T) *releaseFixture { return newReleaseFixture(t, "0.2.0", "EVIL", true, false) },
			wantErr: ErrChecksumMismatch,
		},
		{
			name:    "asset not listed in checksums.txt",
			build:   func(t *testing.T) *releaseFixture { return newReleaseFixture(t, "0.2.0", "NEW", false, true) },
			wantErr: ErrChecksumMismatch,
		},
		{
			name: "truncated archive",
			build: func(t *testing.T) *releaseFixture {
				f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
				// checksums.txt still vouches for the whole archive, so a
				// short body fails verification — which is exactly how a
				// half-finished download is meant to be caught.
				f.archive = f.archive[:len(f.archive)/2]
				return f
			},
			wantErr: ErrChecksumMismatch,
		},
		{
			name: "archive without the aiproxy member",
			build: func(t *testing.T) *releaseFixture {
				f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
				f.archive = tarGz(t, map[string][]byte{"totally-not-aiproxy": []byte("nope")})
				f.checksums = sha256Hex(f.archive) + "  " + f.assetName + "\n"
				return f
			},
			wantErr: nil, // a plain error, asserted as non-nil below
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.build(t)
			srv := f.serve(t, "v0.2.0")
			exe := installedBinary(t, "OLD BINARY")
			c := applyClient(t, srv, "0.1.0", exe)

			rel, err := c.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Apply(context.Background(), rel)
			if err == nil {
				t.Fatal("Apply succeeded, want failure")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			got, rerr := os.ReadFile(exe)
			if rerr != nil {
				t.Fatalf("installed binary is gone: %v", rerr)
			}
			if string(got) != "OLD BINARY" {
				t.Errorf("installed binary was modified: %q", got)
			}
			assertNoLeftovers(t, filepath.Dir(exe))
		})
	}
}

// An unwritable directory is reported before a single byte is downloaded:
// there is no point spending a operator's bandwidth on a swap that cannot
// happen, and the message can name the fix instead of a transfer error.
func TestApplyRefusesAnUnwritableDirectoryBeforeDownloading(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows is out of scope")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD BINARY")
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	c := applyClient(t, srv, "0.1.0", exe)
	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Apply(context.Background(), rel)
	if !errors.Is(err, ErrNotWritable) {
		t.Fatalf("err = %v, want ErrNotWritable", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should name the directory so the message can suggest a fix, got %q", err)
	}
	if f.hits["/owner/repo/releases/download/v0.2.0/"+f.assetName] != 0 {
		t.Error("downloaded the asset despite an unwritable directory")
	}
}

func TestApplyIsANoOpWhenAlreadyCurrent(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD BINARY")
	c := applyClient(t, srv, "0.2.0", exe)

	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Apply(context.Background(), rel)
	if !errors.Is(err, ErrUpToDate) {
		t.Fatalf("err = %v, want ErrUpToDate", err)
	}
	if res.Updated {
		t.Error("Updated should be false")
	}
	if res.Version != "0.2.0" {
		t.Errorf("Version = %q, want the running version 0.2.0", res.Version)
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD BINARY" {
		t.Errorf("binary was touched: %q", got)
	}
}

func TestApplyRefusesADevBuild(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD BINARY")
	c := applyClient(t, srv, "dev", exe)
	if _, err := c.Apply(context.Background(), Release{Version: "0.2.0"}); !errors.Is(err, ErrDevBuild) {
		t.Fatalf("err = %v, want ErrDevBuild", err)
	}
}

// Property 3: a process holding the old binary open keeps reading the old
// bytes across the swap, which is why an update cannot disturb an in-flight
// request. This asserts the rename semantics the design relies on, at the
// level where they are actually true.
func TestApplyLeavesAnOpenHandleOnTheOldInode(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW BINARY", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD BINARY")

	held, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	c := applyClient(t, srv, "0.1.0", exe)
	rel, _ := c.Check(context.Background())
	if _, err := c.Apply(context.Background(), rel); err != nil {
		t.Fatal(err)
	}

	buf, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "OLD BINARY" {
		t.Errorf("open handle reads %q; the running process must keep its own inode", buf)
	}
}

// A gzip bomb — a small archive that decompresses to something enormous — is
// refused rather than written to the operator's disk. 64 MiB of zeros
// compresses to tens of kilobytes, so this costs a small download and a
// bounded write, not a real 64 MiB transfer.
func TestApplyRefusesAnOversizedDecompressedBinary(t *testing.T) {
	huge := make([]byte, maxArchiveBytes+1)
	archive := tarGz(t, map[string][]byte{"aiproxy": huge})
	asset := "aiproxy_0.2.0_linux_amd64.tar.gz"
	f := &releaseFixture{
		assetName: asset,
		archive:   archive,
		checksums: sha256Hex(archive) + "  " + asset + "\n",
		hits:      map[string]int{},
	}
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD BINARY")
	c := applyClient(t, srv, "0.1.0", exe)

	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(context.Background(), rel); err == nil {
		t.Fatal("Apply accepted an oversized binary")
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD BINARY" {
		t.Errorf("installed binary was modified: %q", got)
	}
	assertNoLeftovers(t, filepath.Dir(exe))
}

// assertNoLeftovers fails if anything other than the binary itself is left in
// the install directory. Temp files are dot-prefixed, so this catches an
// error path that forgot its cleanup.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "aiproxy" {
			t.Errorf("leftover file in install dir: %s", e.Name())
		}
	}
}
```

The import block above omits `"io"` and `"strings"`, which `TestApplyLeavesAnOpenHandleOnTheOldInode` and `TestApplyRefusesAnUnwritableDirectoryBeforeDownloading` need — add both.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/updater -run TestApply`
Expected: FAIL — `c.Apply undefined`.

- [ ] **Step 3: Write the implementation**

Append to `internal/updater/updater.go`, and add `"archive/tar"`, `"compress/gzip"`, `"crypto/sha256"`, and `"encoding/hex"` to its import block:

```go
// maxArchiveBytes and maxChecksumBytes bound what a compromised or confused
// server can make this process write to disk and hold in memory. The release
// tarball is a few megabytes; 64 MiB is generous headroom and still a limit.
const (
	maxArchiveBytes  = 64 << 20
	maxChecksumBytes = 1 << 20
)

// Apply installs rel over the running binary and reports what it did.
//
// The step order is the design, not an implementation detail. Nothing is
// downloaded until the install directory has been proven writable, nothing is
// unpacked until the archive has been verified against checksums.txt, and
// nothing replaces the installed binary except a single os.Rename of a fully
// written, correctly permissioned file in the same directory. Every failure
// path therefore leaves the installed binary byte-identical, and every temp
// file is removed on the way out.
//
// os.Rename is why the temp files live in filepath.Dir(exe) rather than
// os.TempDir(): rename is only atomic within one filesystem, and /tmp is
// frequently a different one.
//
// Apply does not restart anything. The running process keeps its open inode
// and goes on serving the old code until the operator quits it; Result is
// what the caller turns into "restart to apply".
func (c *Client) Apply(ctx context.Context, rel Release) (Result, error) {
	if c.current == devVersion {
		return Result{}, ErrDevBuild
	}
	if !c.Newer(rel) {
		// Reported with the running version in place, so a caller can say
		// "already on 0.2.0" without having to look it up again.
		return Result{PreviousVersion: c.current, Version: c.current}, ErrUpToDate
	}

	exe, err := c.execPath()
	if err != nil {
		return Result{}, fmt.Errorf("locate the running binary: %w", err)
	}
	dir := filepath.Dir(exe)

	// Writability probe FIRST. A Homebrew-owned or root-owned install fails
	// here, before a byte moves, and the error names the directory so the
	// message above can suggest re-running the installer or using the package
	// manager that owns it. Probing beats sniffing the path for known
	// prefixes: it asks the only question that actually matters.
	archive, err := os.CreateTemp(dir, ".aiproxy-update-*")
	if err != nil {
		return Result{}, fmt.Errorf("%w: %s", ErrNotWritable, dir)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)

	n, err := c.download(ctx, rel.AssetURL, archive)
	if cerr := archive.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Result{}, err
	}
	if n == 0 {
		return Result{}, fmt.Errorf("%s downloaded as an empty file", rel.AssetName)
	}

	sums, err := c.fetch(ctx, rel.ChecksumURL, maxChecksumBytes)
	if err != nil {
		return Result{}, err
	}
	want, ok := checksumFor(string(sums), rel.AssetName)
	if !ok {
		return Result{}, fmt.Errorf("%w: %s is not listed in checksums.txt",
			ErrChecksumMismatch, rel.AssetName)
	}
	got, err := sha256File(archivePath)
	if err != nil {
		return Result{}, err
	}
	if got != want {
		// Both digests are in the message deliberately. This is the one
		// failure here that could mean something other than a bad day on the
		// network, and summarizing it away would remove the only evidence.
		return Result{}, fmt.Errorf("%w: %s expected sha256 %s, got %s",
			ErrChecksumMismatch, rel.AssetName, want, got)
	}

	// Preserve the mode of what is being replaced rather than imposing a
	// default: an operator who tightened permissions keeps them.
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(exe); err == nil {
		mode = fi.Mode().Perm()
	}

	staged, err := extractBinary(archivePath, dir, mode)
	if err != nil {
		return Result{}, err
	}
	// A no-op once the rename below succeeds; the safety net if it does not.
	defer os.Remove(staged)

	if err := os.Rename(staged, exe); err != nil {
		return Result{}, fmt.Errorf("replace %s: %w", exe, err)
	}
	return Result{
		Updated:         true,
		PreviousVersion: c.current,
		Version:         rel.Version,
		Path:            exe,
	}, nil
}

// get issues one GET and rejects anything but 200, so a 404 for a missing
// asset does not get written to disk as if it were a tarball.
func (c *Client) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	return resp, nil
}

// download streams url into w, capped at maxArchiveBytes. Hitting the cap
// exactly is treated as an overrun rather than a complete download, because
// there is no way to tell the two apart from a LimitReader.
func (c *Client) download(ctx context.Context, url string, w io.Writer) (int64, error) {
	resp, err := c.get(ctx, url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxArchiveBytes))
	if err != nil {
		return n, fmt.Errorf("download %s: %w", url, err)
	}
	if n == maxArchiveBytes {
		return n, fmt.Errorf("download %s: exceeds the %d-byte limit", url, int64(maxArchiveBytes))
	}
	return n, nil
}

func (c *Client) fetch(ctx context.Context, url string, max int64) ([]byte, error) {
	resp, err := c.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// checksumFor finds asset's expected digest in a sha256sum-format file. The
// "*" prefix sha256sum writes for binary mode is tolerated, since that is a
// property of how the file was generated rather than of what it means.
func checksumFor(text, asset string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if strings.TrimPrefix(f[1], "*") == asset {
			return f[0], true
		}
	}
	return "", false
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractBinary writes the archive's "aiproxy" member into a temp file in dir
// with the given mode, and returns its path. The member name is matched
// exactly: the release tarball also carries LICENSE and README.md, which are
// skipped, and anything else in there is skipped too rather than being
// written somewhere on the strength of a path it chose for itself.
func extractBinary(archivePath, dir string, mode os.FileMode) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg || h.Name != binaryName {
			continue
		}

		out, err := os.CreateTemp(dir, ".aiproxy-new-*")
		if err != nil {
			return "", fmt.Errorf("%w: %s", ErrNotWritable, dir)
		}
		n, err := io.Copy(out, io.LimitReader(tr, maxArchiveBytes))
		if err == nil && n == maxArchiveBytes {
			err = fmt.Errorf("%s in the archive exceeds the %d-byte limit",
				binaryName, int64(maxArchiveBytes))
		}
		if err == nil {
			err = out.Chmod(mode)
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(out.Name())
			return "", err
		}
		return out.Name(), nil
	}
	return "", fmt.Errorf("archive %s does not contain %q", filepath.Base(archivePath), binaryName)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/updater -v`
Expected: PASS, including every subtest of `TestApplyFailuresLeaveTheBinaryUntouched`.

- [ ] **Step 5: Commit**

```bash
git add internal/updater/updater.go internal/updater/apply_test.go
git commit -m "feat(updater): verify and swap the binary atomically"
```

---

### Task 4: The background checker

**Files:**
- Create: `internal/updater/checker.go`
- Test: `internal/updater/checker_test.go`

**Interfaces:**
- Consumes: `Client`, `Release`, `Result`, sentinels, `Compare` (Tasks 1–3).
- Produces:
  - `type State struct { Current, Latest, PageURL string; Available bool; CheckedAt int64; Err string; Disabled, DevBuild bool }`
  - `func NewChecker(c *Client, enabled bool, interval time.Duration, opts ...CheckerOption) *Checker`
  - `CheckerOption` values: `WithCheckerClock(func() time.Time)`, `WithCheckerLogger(*slog.Logger)`, `WithInitialDelay(time.Duration)`
  - `func (ck *Checker) Start()`, `func (ck *Checker) Stop()`
  - `func (ck *Checker) State() State`
  - `func (ck *Checker) SetEnabled(bool)`
  - `func (ck *Checker) Apply(ctx context.Context) (Result, error)`

Lifecycle mirrors `prober.Prober` exactly: `Start()` is guarded by a `sync.Once` and always spawns its goroutine (even when disabled) so `Stop()` is always safe after it; `Stop()` must follow `Start()`.

- [ ] **Step 1: Write the failing test**

Create `internal/updater/checker_test.go`:

```go
package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClock() func() time.Time {
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}

// waitFor polls cond until it holds or the deadline passes. The checker's
// loop is a goroutine; polling a predicate is how a test observes it without
// reaching into its internals or sleeping a fixed guess.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCheckerCachesAnAvailableUpdate(t *testing.T) {
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", nil)
	c := newTestClient(t, srv, "0.1.0")
	ck := NewChecker(c, true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	waitFor(t, "the first check", func() bool { return ck.State().CheckedAt != 0 })
	st := ck.State()
	if !st.Available {
		t.Error("Available should be true")
	}
	if st.Latest != "0.2.0" || st.Current != "0.1.0" {
		t.Errorf("State = %+v", st)
	}
	if st.PageURL == "" {
		t.Error("PageURL should be set so the UI can link to the release")
	}
	if st.Err != "" || st.Disabled || st.DevBuild {
		t.Errorf("State = %+v", st)
	}
}

// Property 5: a disabled checker makes no outbound request at all. This is
// the test that keeps the opt-out honest.
func TestDisabledCheckerMakesNoRequests(t *testing.T) {
	hits := 0
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", &hits)
	ck := NewChecker(newTestClient(t, srv, "0.1.0"), false, 10*time.Millisecond,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	time.Sleep(80 * time.Millisecond) // several intervals' worth
	if hits != 0 {
		t.Errorf("made %d requests while disabled, want 0", hits)
	}
	if st := ck.State(); !st.Disabled || st.Available {
		t.Errorf("State = %+v, want Disabled and not Available", st)
	}
}

// Enabling live must not mean waiting a whole interval for an answer.
func TestSetEnabledTriggersAnImmediateCheck(t *testing.T) {
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", nil)
	ck := NewChecker(newTestClient(t, srv, "0.1.0"), false, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Hour))
	ck.Start()
	defer ck.Stop()

	if ck.State().CheckedAt != 0 {
		t.Fatal("a disabled checker should not have checked")
	}
	ck.SetEnabled(true)
	waitFor(t, "the kicked check", func() bool { return ck.State().Available })
	if st := ck.State(); st.Disabled {
		t.Errorf("State = %+v, want not Disabled", st)
	}
}

// A transient network failure must not make an available update vanish from
// the header — the last good answer survives, with the error recorded beside
// it (spec: the check's caching rules).
func TestFailedCheckKeepsTheLastGoodAnswer(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	ck := NewChecker(newTestClient(t, srv, "0.1.0"), true, 10*time.Millisecond,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	waitFor(t, "the good check", func() bool { return ck.State().Available })
	fail = true
	waitFor(t, "the failed check", func() bool { return ck.State().Err != "" })

	st := ck.State()
	if !st.Available || st.Latest != "0.2.0" {
		t.Errorf("a failed check erased the last good answer: %+v", st)
	}
}

func TestCheckerReportsADevBuild(t *testing.T) {
	hits := 0
	srv := redirectServer(t, "/owner/repo/releases/tag/v0.2.0", &hits)
	ck := NewChecker(newTestClient(t, srv, "dev"), true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	waitFor(t, "the first check", func() bool { return ck.State().CheckedAt != 0 })
	st := ck.State()
	if !st.DevBuild || st.Available || st.Err != "" {
		t.Errorf("State = %+v, want DevBuild with no error and nothing available", st)
	}
	if hits != 0 {
		t.Errorf("a dev build made %d requests, want 0", hits)
	}
}

// Once an update is installed, the header must stop offering it: the
// remaining action is a restart, which the flash says, and an "available"
// badge alongside "installed" would contradict itself.
func TestApplyClearsAvailable(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD")
	c := applyClient(t, srv, "0.1.0", exe)
	ck := NewChecker(c, true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()
	waitFor(t, "the first check", func() bool { return ck.State().Available })

	res, err := ck.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Updated || res.Version != "0.2.0" {
		t.Errorf("Result = %+v", res)
	}
	if st := ck.State(); st.Available {
		t.Error("Available should be cleared once the update is installed")
	}
}

func TestCheckerApplyReportsUpToDate(t *testing.T) {
	f := newReleaseFixture(t, "0.2.0", "NEW", false, false)
	srv := f.serve(t, "v0.2.0")
	exe := installedBinary(t, "OLD")
	ck := NewChecker(applyClient(t, srv, "0.2.0", exe), true, time.Hour,
		WithCheckerClock(testClock()), WithInitialDelay(time.Hour))
	ck.Start()
	defer ck.Stop()

	if _, err := ck.Apply(context.Background()); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("err = %v, want ErrUpToDate", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/updater -run 'TestChecker|TestDisabled|TestSetEnabled|TestFailedCheck|TestApplyClears'`
Expected: FAIL — `undefined: NewChecker`.

- [ ] **Step 3: Write the implementation**

Create `internal/updater/checker.go`:

```go
package updater

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// defaultInitialDelay is how long after Start the first check waits. A
// proxy's first seconds belong to binding its listener and loading accounts,
// not to an errand nobody is waiting on.
const defaultInitialDelay = 30 * time.Second

// defaultInterval is the fallback when a config supplies a non-positive
// interval, which would otherwise spin the loop.
const defaultInterval = 24 * time.Hour

// State is the cached answer to "is there a newer release", shaped so
// view.Local can map it straight onto view.UpdateStatus.
type State struct {
	// Current is the running binary's version, so a UI can render "0.1.0 →
	// 0.2.0" without a second source for the left-hand side.
	Current string
	// Latest is the most recent release seen, or "" if none has been.
	Latest string
	// PageURL links to that release's page.
	PageURL string
	// Available is true only when Latest sorts above Current AND the check is
	// enabled AND no update has been installed since.
	Available bool
	// CheckedAt is unix ms of the last completed check attempt, 0 if none.
	CheckedAt int64
	// Err is the last check's failure, or "" — it does NOT clear Latest or
	// Available, so a transient network failure cannot make an available
	// update disappear from the UI.
	Err string
	// Disabled reflects update.checkEnabled.
	Disabled bool
	// DevBuild is true when the running binary was never stamped with a
	// version, which is reported rather than shown as a misleading "up to
	// date".
	DevBuild bool
}

// Checker keeps State fresh on a long cadence so nothing on the render path
// or the request path ever waits on github.com (design property 4). Its
// lifecycle is deliberately identical to metrics.Roller, metrics.Pruner, and
// prober.Prober: constructed in cmd/aiproxy's buildHandler, Start()/Stop()
// from run().
type Checker struct {
	client       *Client
	interval     time.Duration
	initialDelay time.Duration
	now          func() time.Time
	log          *slog.Logger

	stop      chan struct{}
	stopped   chan struct{}
	kick      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	mu      sync.Mutex
	enabled bool
	state   State
}

// CheckerOption configures a Checker at construction.
type CheckerOption func(*Checker)

// WithCheckerClock overrides the clock CheckedAt is stamped from, so tests do
// not race the wall clock.
func WithCheckerClock(now func() time.Time) CheckerOption {
	return func(ck *Checker) { ck.now = now }
}

// WithCheckerLogger overrides the logger. Nil (the default) uses slog.Default.
func WithCheckerLogger(log *slog.Logger) CheckerOption {
	return func(ck *Checker) { ck.log = log }
}

// WithInitialDelay overrides the wait before the first check.
func WithInitialDelay(d time.Duration) CheckerOption {
	return func(ck *Checker) { ck.initialDelay = d }
}

// NewChecker builds a Checker over c. enabled is update.checkEnabled and can
// be changed later through SetEnabled; interval is update.checkIntervalHours
// and cannot, which is why it is reported as restart-gated in view.Applied.
func NewChecker(c *Client, enabled bool, interval time.Duration, opts ...CheckerOption) *Checker {
	if interval <= 0 {
		interval = defaultInterval
	}
	ck := &Checker{
		client:       c,
		interval:     interval,
		initialDelay: defaultInitialDelay,
		now:          time.Now,
		log:          slog.Default(),
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
		kick:         make(chan struct{}, 1),
		enabled:      enabled,
		state:        State{Current: c.Current(), Disabled: !enabled},
	}
	for _, o := range opts {
		o(ck)
	}
	return ck
}

// Start begins the background loop. Like prober.Prober.Start it always spawns
// its goroutine, even when checking is disabled, so Stop is always safe
// afterward and SetEnabled(true) has a loop to wake. Guarded by a sync.Once:
// a duplicate call is a no-op rather than a second loop racing the first.
//
// Stop must follow Start — the same contract prober.Prober documents.
func (ck *Checker) Start() {
	ck.startOnce.Do(func() { go ck.loop() })
}

// Stop halts the loop and waits for it to finish.
func (ck *Checker) Stop() {
	ck.stopOnce.Do(func() {
		close(ck.stop)
		<-ck.stopped
	})
}

// State returns the cached answer. It never performs I/O: this is what
// ServerStatus calls, on the TUI's two-second cadence.
func (ck *Checker) State() State {
	ck.mu.Lock()
	defer ck.mu.Unlock()
	return ck.state
}

// SetEnabled turns checking on or off live, which is what makes
// update.checkEnabled a live-tunable setting rather than a restart-gated one.
// Turning it on kicks an immediate check: an operator who just enabled it
// should not wait up to a day for the first answer. Turning it off clears
// Available, so the UI stops offering something the operator just declined to
// look for.
func (ck *Checker) SetEnabled(on bool) {
	ck.mu.Lock()
	changed := ck.enabled != on
	ck.enabled = on
	ck.state.Disabled = !on
	if !on {
		ck.state.Available = false
	}
	ck.mu.Unlock()

	if changed && on {
		select {
		case ck.kick <- struct{}{}:
		default: // a kick is already pending; one is enough
		}
	}
}

// Apply runs one check-and-install cycle and folds the outcome back into the
// cache, so a successful install stops the UI offering the same update. The
// remaining action after this returns is a restart, and that is the caller's
// message to deliver.
//
// Serializing concurrent callers is the seam's job, not this method's; see
// view.Local.ApplyUpdate and ErrUpdateInProgress.
func (ck *Checker) Apply(ctx context.Context) (Result, error) {
	rel, err := ck.client.Check(ctx)
	if err != nil {
		return Result{}, err
	}
	res, err := ck.client.Apply(ctx, rel)
	if err != nil {
		return res, err
	}

	ck.mu.Lock()
	ck.state.Latest = res.Version
	ck.state.Available = false
	ck.state.Err = ""
	ck.mu.Unlock()

	ck.log.Info("updated in place", "from", res.PreviousVersion, "to", res.Version, "path", res.Path)
	return res, nil
}

func (ck *Checker) loop() {
	defer close(ck.stopped)

	// One context for the whole loop, cancelled by Stop, so a check in flight
	// at shutdown is abandoned rather than holding the process open.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-ck.stop
		cancel()
	}()

	timer := time.NewTimer(ck.initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ck.stop:
			return
		case <-ck.kick:
			if !timer.Stop() {
				// Drain a timer that already fired, so the Reset below is not
				// immediately satisfied by a stale value.
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
		timer.Reset(ck.interval)

		ck.mu.Lock()
		enabled := ck.enabled
		ck.mu.Unlock()
		if !enabled {
			// The whole of property 5: a disabled checker reaches no further
			// than this line.
			continue
		}
		ck.checkOnce(ctx)
	}
}

// checkOnce performs one check and folds the result into the cache. A failure
// records the error WITHOUT clearing a previous good answer: a flaky link
// must not make an available update disappear.
func (ck *Checker) checkOnce(ctx context.Context) {
	rel, err := ck.client.Check(ctx)

	ck.mu.Lock()
	defer ck.mu.Unlock()
	ck.state.CheckedAt = ck.now().UnixMilli()

	switch {
	case errors.Is(err, ErrDevBuild):
		// Not a failure, and not something to retry differently: reported as
		// its own state so the UI can say "dev build" rather than "up to date".
		ck.state.DevBuild = true
		ck.state.Available = false
		ck.state.Err = ""
	case errors.Is(err, ErrNoReleases):
		ck.state.Available = false
		ck.state.Err = ""
	case err != nil:
		ck.state.Err = err.Error()
		ck.log.Debug("update check failed", "err", err)
	default:
		ck.state.Err = ""
		ck.state.Latest = rel.Version
		ck.state.PageURL = rel.PageURL
		ck.state.Available = Compare(rel.Version, ck.state.Current) > 0
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/updater -race -v`
Expected: PASS with no data race. `-race` matters here: the loop writes the cache while `State()` reads it.

- [ ] **Step 5: Commit**

```bash
git add internal/updater/checker.go internal/updater/checker_test.go
git commit -m "feat(updater): cache update availability on a background cadence"
```

---

### Task 5: Config block and settings plumbing

**Files:**
- Modify: `internal/config/config.go` (the `Update` struct, the `Config` field, `Default()`)
- Modify: `internal/config/store.go` (`loadLocked`'s sanity guard)
- Modify: `internal/view/types.go` (two `Settings` fields)
- Modify: `internal/view/settings.go` (`Validate`)
- Modify: `internal/view/local.go` (`settingsFromConfig`, `UpdateSettings`, `diffSettings`, `restartSettingsFields`)
- Modify: `internal/tui/settings.go` (two rows in `settingFields()`)
- Modify: `internal/tui/frames_test.go` (the two new fields in the settings fixture)
- Test: `internal/config/config_test.go` (add), `internal/view/local_test.go` (add)

**Interfaces:**
- Consumes: nothing from Tasks 1–4.
- Produces:
  - `config.Update{CheckEnabled bool "json:checkEnabled"; CheckIntervalHours int "json:checkIntervalHours"}`, reachable as `cfg.Update`
  - `view.Settings.UpdateCheckEnabled bool "json:updateCheckEnabled"` and `view.Settings.UpdateCheckIntervalHours int "json:updateCheckIntervalHours"`

Both new settings names go into `restartSettingsFields` in this task. Task 6 moves `updateCheckEnabled` to `liveSettingsFields` at the same moment it wires the live apply, which is exactly the one-line change that slice's doc comment describes — so no commit ever claims a field is live before it is.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
// A config file predating the update block must decode to the defaults, not
// to a disabled checker with a zero interval that would spin the loop.
func TestUpdateDefaultsFillInForAnOlderConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":{"addr":"127.0.0.1:9999","apiKey":"ap-x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Update.CheckEnabled {
		t.Error("CheckEnabled should default to true")
	}
	if cfg.Update.CheckIntervalHours != 24 {
		t.Errorf("CheckIntervalHours = %d, want 24", cfg.Update.CheckIntervalHours)
	}
}

// An explicit opt-out survives a reload — the whole point of it being a
// setting rather than a flag.
func TestUpdateOptOutIsHonoured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"update":{"checkEnabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Update.CheckEnabled {
		t.Error("CheckEnabled should stay false")
	}
	if cfg.Update.CheckIntervalHours != 24 {
		t.Errorf("an absent interval inside a present block should still default: got %d", cfg.Update.CheckIntervalHours)
	}
}

// A hand-edited zero or negative interval is corrected on load rather than
// handed to a ticker.
func TestNonPositiveUpdateIntervalFallsBackToTheDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"update":{"checkEnabled":true,"checkIntervalHours":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Update.CheckIntervalHours != 24 {
		t.Errorf("CheckIntervalHours = %d, want 24", cfg.Update.CheckIntervalHours)
	}
}
```

(If `internal/config/config_test.go` does not already import `os`, `path/filepath`, and `testing`, add them.)

Add to `internal/view/local_test.go`:

```go
// The update settings round-trip through the seam like every other field, and
// the interval is restart-gated because the checker's ticker is built once.
func TestUpdateSettingsRoundTripsTheUpdateBlock(t *testing.T) {
	local := newHarness(t).local
	s, err := local.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.UpdateCheckEnabled || s.UpdateCheckIntervalHours != 24 {
		t.Fatalf("defaults not surfaced: %+v", s)
	}

	s.UpdateCheckIntervalHours = 6
	applied, err := local.UpdateSettings(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied.NeedsRestart, "updateCheckIntervalHours") {
		t.Errorf("NeedsRestart = %v, want updateCheckIntervalHours", applied.NeedsRestart)
	}

	back, err := local.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if back.UpdateCheckIntervalHours != 6 {
		t.Errorf("interval did not persist: %d", back.UpdateCheckIntervalHours)
	}
}

// A zero interval is refused before it is written: a bad value on disk
// survives a restart, which is worse than a rejected call.
func TestValidateRejectsANonPositiveUpdateInterval(t *testing.T) {
	s := Settings{
		SwitchThreshold: 0.9, RetryBudgetMS: 1000, HeaderTimeoutMS: 1000, BodyIdleMS: 1000,
		QuotaProbeIntervalSeconds: 300, MetricsRetentionDays: 90,
		UpdateCheckEnabled: true, UpdateCheckIntervalHours: 0,
	}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a zero updateCheckIntervalHours")
	}
}
```

`newHarness(t, accts ...config.Account) *testHarness` already exists in `internal/view/local_test.go` (line 43) and its `.local` field is the `*Local` under test; `slices` is already imported there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config ./internal/view`
Expected: FAIL — `cfg.Update` undefined, `s.UpdateCheckEnabled` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/config/config.go`, add the struct after `MITM`:

```go
// Update controls in-app update checking. CheckEnabled is opt-out rather than
// opt-in because a proxy that silently runs months behind a security fix is
// the worse default — but it IS an outbound request that tells github.com
// this installation's IP and version, so turning it off must be one setting,
// documented, and honoured absolutely (no request is made when it is false).
//
// CheckIntervalHours is a day by default: a release cadence measured in weeks
// does not reward polling, and the answer is cached besides.
type Update struct {
	CheckEnabled       bool `json:"checkEnabled"`
	CheckIntervalHours int  `json:"checkIntervalHours"`
}
```

Add the field to `Config`:

```go
type Config struct {
	Listen     Listen     `json:"listen"`
	Accounts   []Account  `json:"accounts"`
	Routing    Routing    `json:"routing"`
	Retry      Retry      `json:"retry"`
	QuotaProbe QuotaProbe `json:"quotaProbe"`
	Metrics    Metrics    `json:"metrics"`
	MITM       MITM       `json:"mitm"`
	Update     Update     `json:"update"`
}
```

Add to `Default()`'s literal:

```go
		Update:     Update{CheckEnabled: true, CheckIntervalHours: 24},
```

In `internal/config/store.go`, extend `loadLocked`'s post-decode guards, immediately after the `Listen.APIKey` guard:

```go
	// A hand-edited 0 (or a negative) would be handed to a ticker; correct it
	// here rather than making every consumer defend against it.
	if cfg.Update.CheckIntervalHours <= 0 {
		cfg.Update.CheckIntervalHours = Default().Update.CheckIntervalHours
	}
```

In `internal/view/types.go`, add to `Settings` (and extend that type's doc comment's list of fields to mention update checking):

```go
	// UpdateCheckEnabled and UpdateCheckIntervalHours control the background
	// release check. Enabled is live-tunable; the interval is not, because the
	// checker's ticker is built once at startup.
	UpdateCheckEnabled       bool `json:"updateCheckEnabled"`
	UpdateCheckIntervalHours int  `json:"updateCheckIntervalHours"`
```

In `internal/view/settings.go`, add to `Validate` before the `BlockedModels` loop:

```go
	if s.UpdateCheckIntervalHours <= 0 {
		return fmt.Errorf("updateCheckIntervalHours must be positive, got %d", s.UpdateCheckIntervalHours)
	}
```

In `internal/view/local.go`:

```go
// settingsFromConfig — add the two fields
		UpdateCheckEnabled:        c.Update.CheckEnabled,
		UpdateCheckIntervalHours:  c.Update.CheckIntervalHours,
```

```go
// restartSettingsFields — append both names for now; Task 6 moves
// updateCheckEnabled into liveSettingsFields when the apply is wired.
	restartSettingsFields = []string{
		"blockedModels", "retryBudgetMs", "inlineAbsorbMaxMs",
		"headerTimeoutMs", "bodyIdleMs", "quotaProbeIntervalSeconds", "metricsRetentionDays",
		"updateCheckEnabled", "updateCheckIntervalHours",
	}
```

```go
// diffSettings — two entries in the changed map
		"updateCheckEnabled":        before.UpdateCheckEnabled != after.UpdateCheckEnabled,
		"updateCheckIntervalHours":  before.UpdateCheckIntervalHours != after.UpdateCheckIntervalHours,
```

```go
// UpdateSettings — two writes inside the config.Store.Update callback
		c.Update.CheckEnabled = s.UpdateCheckEnabled
		c.Update.CheckIntervalHours = s.UpdateCheckIntervalHours
```

In `internal/tui/settings.go`, append two rows to `settingFields()` after `metricsRetentionDays`:

```go
		{
			name: "updateCheckEnabled", boolean: true,
			desc: "check github for newer aiproxy releases in the background",
			get: func(s view.Settings) string {
				if s.UpdateCheckEnabled {
					return "on"
				}
				return "off"
			},
			set: func(s *view.Settings, v string) error {
				s.UpdateCheckEnabled = v == "on"
				return nil
			},
		},
		{
			name: "updateCheckIntervalHours", unit: "h",
			desc: "how often to check for a newer release",
			get:  func(s view.Settings) string { return strconv.Itoa(s.UpdateCheckIntervalHours) },
			set:  func(s *view.Settings, v string) error { return setInt(&s.UpdateCheckIntervalHours)(s, v) },
		},
```

In `internal/tui/frames_test.go`, add the two fields to `fixtureModel`'s `m.settings.current` literal so the settings frame renders them with realistic values:

```go
		MetricsRetentionDays: 90,
		UpdateCheckEnabled:   true, UpdateCheckIntervalHours: 24,
```

- [ ] **Step 4: Run the tests and regenerate the settings goldens**

```bash
go test ./internal/config ./internal/view
go test ./internal/tui -run TestGoldenFrames -update
git diff --stat internal/tui/testdata
git diff internal/tui/testdata
go test ./...
```

Expected: the config and view tests PASS. The golden regeneration touches only `settings_*.golden` (three files); **read the diff** and confirm it adds exactly the two new rows and changes nothing else. If any non-settings golden changed, stop — the header or footer moved unintentionally.

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/view internal/tui
git commit -m "feat(config): add the update block and surface it in settings"
```

---

### Task 6: The presentation seam

**Files:**
- Modify: `internal/view/types.go` (`UpdateStatus`, `UpdateResult`, `Status.Update`)
- Modify: `internal/view/source.go` (the `ApplyUpdate` method)
- Modify: `internal/view/local.go` (`updates` field, `NewLocal` parameter, `updateStatus`, `ApplyUpdate`, `updMu`, moving `updateCheckEnabled` to live)
- Modify: `internal/proxy/handler_test.go:196` and `internal/view/local_test.go:93` (pass `nil` for the new `NewLocal` parameter)
- Test: `internal/view/local_test.go` (add)

**Interfaces:**
- Consumes: `updater.Checker`, `updater.State`, `updater.Result`, `updater.ErrUpToDate`, `updater.ErrNoReleases`, `updater.ErrUpdateInProgress` (Tasks 1–4); `view.Settings.UpdateCheckEnabled` (Task 5).
- Produces:
  - `view.UpdateStatus` (eight fields, listed below) on `view.Status.Update`
  - `view.UpdateResult{Updated bool; PreviousVersion, Version, Path, Message string}`
  - `view.Source.ApplyUpdate(ctx context.Context) (UpdateResult, error)`
  - `view.NewLocal(mgr, ms, cs, listenAddr, dropped, pb, upd *updater.Checker, opts ...option) *Local` — the checker is a new positional parameter after `pb`, and may be nil.

- [ ] **Step 1: Write the failing tests**

Add to `internal/view/local_test.go`:

```go
// Update availability rides on Status, exactly as Probe does, so the TUI's
// existing status poll renders it with no second poll and no new route.
func TestServerStatusReportsUpdateAvailability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.9.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := updater.New("owner/repo", "0.1.0",
		updater.WithBaseURL(srv.URL), updater.WithHTTPClient(srv.Client()))
	ck := updater.NewChecker(c, true, time.Hour, updater.WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	local := newLocalWithUpdater(t, ck) // see note below

	deadline := time.Now().Add(2 * time.Second)
	var st Status
	for time.Now().Before(deadline) {
		var err error
		st, err = local.ServerStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Update.Available {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !st.Update.Available {
		t.Fatalf("Update = %+v, want Available", st.Update)
	}
	if st.Update.CurrentVersion != "0.1.0" || st.Update.LatestVersion != "0.9.0" {
		t.Errorf("Update = %+v", st.Update)
	}
	if st.Update.ReleaseURL == "" || st.Update.CheckedAt == 0 {
		t.Errorf("Update = %+v", st.Update)
	}
}

// Property 4: reading Status never touches the network. The checker polls on
// its own cadence and ServerStatus reads a cache, so a hundred status reads
// cost exactly the requests the checker already made — which is what keeps a
// TUI frame and a proxied request from ever waiting on github.com.
func TestServerStatusNeverHitsTheNetwork(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.9.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := updater.New("owner/repo", "0.1.0",
		updater.WithBaseURL(srv.URL), updater.WithHTTPClient(srv.Client()))
	// A one-hour interval means the loop checks once and then sleeps well past
	// the end of this test, so any growth in hits came from ServerStatus.
	ck := updater.NewChecker(c, true, time.Hour, updater.WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	local := newLocalWithUpdater(t, ck)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := local.ServerStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if st.Update.Available {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := hits
	for i := 0; i < 100; i++ {
		if _, err := local.ServerStatus(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if hits != before {
		t.Errorf("100 status reads made %d extra requests, want 0", hits-before)
	}
}

// A Local built without a checker reports a disabled update status rather than
// panicking — the same courtesy probeStatus already extends to a nil prober.
func TestServerStatusWithoutAnUpdaterReportsDisabled(t *testing.T) {
	local := newLocalWithUpdater(t, nil)
	st, err := local.ServerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Update.Disabled || st.Update.Available {
		t.Errorf("Update = %+v, want Disabled and not Available", st.Update)
	}
}

func TestApplyUpdateWithoutAnUpdaterFails(t *testing.T) {
	local := newLocalWithUpdater(t, nil)
	if _, err := local.ApplyUpdate(context.Background()); err == nil {
		t.Fatal("ApplyUpdate should fail with no updater configured")
	}
}

// Nothing-to-do outcomes are results, not errors: a caller must be able to
// render "already on 0.1.0" without inspecting an error string.
func TestApplyUpdateReportsUpToDateAsAResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.1.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := updater.New("owner/repo", "0.1.0",
		updater.WithBaseURL(srv.URL), updater.WithHTTPClient(srv.Client()))
	ck := updater.NewChecker(c, true, time.Hour, updater.WithInitialDelay(time.Hour))
	ck.Start()
	defer ck.Stop()

	res, err := newLocalWithUpdater(t, ck).ApplyUpdate(context.Background())
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if res.Updated {
		t.Error("Updated should be false")
	}
	if res.Message == "" {
		t.Error("Message should say what happened")
	}
}

// updateCheckEnabled is live: toggling it through the seam reaches the running
// checker, which is why it belongs in liveSettingsFields.
func TestUpdateSettingsAppliesCheckEnabledLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.9.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := updater.New("owner/repo", "0.1.0",
		updater.WithBaseURL(srv.URL), updater.WithHTTPClient(srv.Client()))
	ck := updater.NewChecker(c, true, time.Hour, updater.WithInitialDelay(time.Millisecond))
	ck.Start()
	defer ck.Stop()

	local := newLocalWithUpdater(t, ck)
	s, err := local.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.UpdateCheckEnabled = false
	applied, err := local.UpdateSettings(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied.Live, "updateCheckEnabled") {
		t.Errorf("Live = %v, want updateCheckEnabled", applied.Live)
	}
	st, err := local.ServerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Update.Disabled {
		t.Error("disabling the check should show up in Status immediately")
	}
}
```

Add this helper to the same file. These tests live in package `view`, so they
can assign the unexported field directly and no new constructor is needed:

```go
// newLocalWithUpdater reuses the existing harness and attaches ck. Setting the
// field rather than adding a constructor parameter keeps newHarness — used by
// every other test in this file — passing nil for it, unchanged.
func newLocalWithUpdater(t *testing.T, ck *updater.Checker) *Local {
	t.Helper()
	local := newHarness(t).local
	local.updates = ck
	return local
}
```

`internal/view/local_test.go` already imports `net/http`, `slices`, `testing`,
and `time`; add `net/http/httptest` and
`github.com/nicko170/aiproxy/internal/updater`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/view`
Expected: FAIL — `st.Update` undefined, `local.ApplyUpdate` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/view/types.go`, add the field to `Status` (after `Probe`):

```go
	// Update reports whether a newer release exists, read from the background
	// checker's cache. It is never a live network call: this rides on Status
	// for the same reason Probe does — the TUI's existing status poll renders
	// it for free, with no second poll cycle and no new route — and the check's
	// own cadence is decoupled from that poll entirely.
	Update UpdateStatus `json:"update"`
```

And the two new types:

```go
// UpdateStatus is what the running instance knows about newer releases.
// Disabled and DevBuild are distinct states from "nothing available": a UI
// that cannot tell them apart ends up saying "up to date" to someone whose
// check is switched off.
type UpdateStatus struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion,omitempty"`
	Available      bool   `json:"available"`
	ReleaseURL     string `json:"releaseUrl,omitempty"`
	// CheckedAt is unix ms of the last completed check, 0 if none.
	CheckedAt int64 `json:"checkedAt,omitempty"`
	// CheckError is the last check's failure. It does not clear
	// LatestVersion or Available: a transient network failure must not make an
	// available update disappear from the UI.
	CheckError string `json:"checkError,omitempty"`
	Disabled   bool   `json:"disabled"`
	DevBuild   bool   `json:"devBuild"`
}

// UpdateResult is what an ApplyUpdate call did.
//
// Updated is false, with a nil error, for the two nothing-to-do outcomes —
// already on the latest release, and no releases published. Those are states,
// not failures, and a UI must be able to render them without reading an error
// string.
type UpdateResult struct {
	Updated         bool   `json:"updated"`
	PreviousVersion string `json:"previousVersion,omitempty"`
	Version         string `json:"version,omitempty"`
	Path            string `json:"path,omitempty"`
	// Message is what the TUI flashes and the CLI prints — the one place
	// "restart to apply" is worded, so both say it identically.
	Message string `json:"message"`
}
```

In `internal/view/source.go`, add to the interface after `ProbeNow`:

```go
	// ApplyUpdate downloads, verifies, and installs the latest release over
	// the running binary, and reports what it did. It deliberately does not
	// restart anything: the running process keeps its open inode and serves
	// the old code until the operator quits it, so the caller shows
	// UpdateResult.Message ("restart to apply") rather than acting on it.
	//
	// Availability is NOT read here — it rides on ServerStatus's Update field,
	// off a background cache, so no UI ever blocks a frame on github.com.
	ApplyUpdate(ctx context.Context) (UpdateResult, error)
```

In `internal/view/local.go`:

```go
// import "github.com/nicko170/aiproxy/internal/updater"

// Local — new field beside probe
	updates *updater.Checker

// Local — a second mutex, deliberately not l.mu
	// updMu serializes ApplyUpdate against itself, and only against itself.
	// It is not l.mu because an install downloads several megabytes over a
	// possibly slow link: holding the mutation lock across that would block
	// SetPriority, SetAccountEnabled, and UpdateSettings for the duration, and
	// there is nothing in an install that could interleave badly with them
	// anyway (it touches no config and no Manager state). What must not happen
	// is two installs racing to rename over the same path, which is exactly
	// what this prevents.
	updMu sync.Mutex
```

`NewLocal` gains `upd *updater.Checker` after `pb`, assigns `updates: upd`, and its doc comment gains:

```go
// upd is the background update checker ServerStatus reports from and
// ApplyUpdate installs through; it may be nil (a Local built without one
// reports update checking as disabled rather than panicking, matching how pb
// is handled).
```

`ServerStatus` gains `Update: l.updateStatus(),` in its returned literal, and:

```go
// updateStatus converts the checker's cached state into the view-level shape
// ServerStatus reports. This never performs I/O — that is the entire reason
// the checker caches — so a status poll costs the same whether or not a check
// is due. A nil checker reports Disabled rather than an empty "up to date",
// which would be a lie about a question that was never asked.
func (l *Local) updateStatus() UpdateStatus {
	if l.updates == nil {
		return UpdateStatus{Disabled: true}
	}
	st := l.updates.State()
	return UpdateStatus{
		CurrentVersion: st.Current,
		LatestVersion:  st.Latest,
		Available:      st.Available,
		ReleaseURL:     st.PageURL,
		CheckedAt:      st.CheckedAt,
		CheckError:     st.Err,
		Disabled:       st.Disabled,
		DevBuild:       st.DevBuild,
	}
}

// ApplyUpdate installs the latest release over the running binary.
//
// The two nothing-to-do outcomes — already current, and no releases published
// — come back as a Result with Updated false and a nil error, because a
// settings screen showing them in red would be wrong. Everything else is a
// real error, wrapped by internal/updater's sentinels so the control API can
// map it to a status code that means something (see writeUpdateError).
func (l *Local) ApplyUpdate(ctx context.Context) (UpdateResult, error) {
	if l.updates == nil {
		return UpdateResult{}, fmt.Errorf("update checking is not configured")
	}
	if !l.updMu.TryLock() {
		return UpdateResult{}, updater.ErrUpdateInProgress
	}
	defer l.updMu.Unlock()

	res, err := l.updates.Apply(ctx)
	switch {
	case errors.Is(err, updater.ErrUpToDate):
		return UpdateResult{
			PreviousVersion: res.PreviousVersion,
			Version:         res.Version,
			Message:         "already on " + res.Version,
		}, nil
	case errors.Is(err, updater.ErrNoReleases):
		return UpdateResult{Message: "no releases published yet"}, nil
	case err != nil:
		return UpdateResult{}, err
	}
	return UpdateResult{
		Updated:         true,
		PreviousVersion: res.PreviousVersion,
		Version:         res.Version,
		Path:            res.Path,
		Message:         "updated to " + res.Version + " — restart to apply",
	}, nil
}
```

Add `"errors"` to `local.go`'s imports if it is not already there.

Move `updateCheckEnabled` from `restartSettingsFields` to `liveSettingsFields`, and apply it at the end of `UpdateSettings` beside the two Manager calls:

```go
	liveSettingsFields    = []string{"switchThreshold", "sessionAffinity", "updateCheckEnabled"}
```

```go
	l.mgr.SetSwitchThreshold(s.SwitchThreshold)
	l.mgr.SetSessionAffinity(s.SessionAffinity)
	if l.updates != nil {
		l.updates.SetEnabled(s.UpdateCheckEnabled)
	}
	return applied, nil
```

Finally, update the two existing `NewLocal` call sites to pass `nil` for the new parameter:
- `internal/proxy/handler_test.go:196`
- `internal/view/local_test.go:93`

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/view -race
go test ./...
```

Expected: the view tests PASS. `go test ./...` FAILS in `internal/proxy` with `TestEveryViewSourceMethodHasAControlRoute` reporting that `ApplyUpdate` has no route mapping — that is the lockstep test doing its job, and Task 7 fixes it. Everything else must pass.

- [ ] **Step 5: Commit**

```bash
git add internal/view internal/proxy/handler_test.go
git commit -m "feat(view): carry update availability on Status and add ApplyUpdate"
```

---

### Task 7: The control route

**Files:**
- Modify: `internal/proxy/control.go` (`applyUpdateHandler`, `writeUpdateError`)
- Modify: `internal/proxy/router.go` (register the route)
- Modify: `internal/proxy/lockstep_test.go` (the `routeFor` entry)
- Test: `internal/proxy/control_api_test.go` (add)

**Interfaces:**
- Consumes: `view.Source.ApplyUpdate`, `view.UpdateResult` (Task 6); `updater`'s sentinels (Tasks 2–3).
- Produces: `POST /_aiproxy/api/v1/update`.

Status mapping: 409 `ErrUpdateInProgress`, 412 `ErrDevBuild`, 403 `ErrNotWritable`, 502 `ErrChecksumMismatch`, 500 otherwise. The nothing-to-do outcomes are 200 with `updated: false`.

- [ ] **Step 1: Write the failing test**

Add to `internal/proxy/control_api_test.go`, following that file's existing harness conventions:

```go
// The update route distinguishes its failures, because "500" tells an
// operator nothing about whether to re-run the installer, wait, or worry.
func TestUpdateRouteMapsFailuresToDistinctStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"in progress", updater.ErrUpdateInProgress, http.StatusConflict},
		{"dev build", updater.ErrDevBuild, http.StatusPreconditionFailed},
		{"not writable", fmt.Errorf("%w: /usr/local/bin", updater.ErrNotWritable), http.StatusForbidden},
		{"checksum", fmt.Errorf("%w: bad digest", updater.ErrChecksumMismatch), http.StatusBadGateway},
		{"other", errors.New("network is unreachable"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeUpdateError(rec, tc.err)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// The route is reachable, gated like every other control endpoint, and
// answers with JSON rather than falling through to the proxy. The harness's
// view.Local has no checker attached, so this asserts the wiring and the
// error path; the happy path is covered in internal/updater and internal/view.
func TestControlAPIUpdateRouteIsWiredAndNotProxied(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})

	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+ReservedPrefix+"/api/v1/update", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	// 500 because no checker is configured — never 404 (unrouted) and never a
	// proxied upstream answer.
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if !strings.Contains(string(body), "not configured") {
		t.Errorf("body = %s, want it to say why", body)
	}
}

// A GET on the update path must not be forwarded upstream with a credential
// attached — the same hazard router.go's MethodNotAllowed comment describes.
func TestControlAPIUpdateRejectsGet(t *testing.T) {
	h := newRouterHarness(t, nil, testutil.Script{Status: 200, Body: `{}`})
	res, err := http.Get(h.srv.URL + ReservedPrefix + "/api/v1/update")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}
```

`internal/proxy/control_api_test.go` already imports `io`, `net/http`, `strings`, `testing`, and `testutil`; add `errors`, `fmt`, and `net/http/httptest` for the mapping table, plus `github.com/nicko170/aiproxy/internal/updater`. The harness is `newRouterHarness(t, nil, testutil.Script{...})` and its live server is `h.srv`, exactly as every other test in that file uses it. `TestEveryViewSourceMethodHasAControlRoute` in `internal/proxy/lockstep_test.go` is the other test for this task and needs no new code beyond its `routeFor` entry.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/proxy -run 'TestUpdateRoute|TestEveryViewSource'`
Expected: FAIL — `undefined: writeUpdateError`, and the lockstep test still reporting `ApplyUpdate` unmapped.

- [ ] **Step 3: Write the implementation**

In `internal/proxy/control.go`, add `"github.com/nicko170/aiproxy/internal/updater"` to the imports and append:

```go
// applyUpdateHandler installs the latest release over the running binary. It
// answers before any restart happens, because none does: the response says
// what was installed and that a restart is needed, which is all this endpoint
// can honestly promise (see view.Source.ApplyUpdate).
func applyUpdateHandler(o HandlerOptions) http.HandlerFunc {
	return controlHandler(o, func(src view.Source, w http.ResponseWriter, r *http.Request) {
		res, err := src.ApplyUpdate(r.Context())
		if err != nil {
			writeUpdateError(w, err)
			return
		}
		writeJSON(w, res)
	})
}

// writeUpdateError maps internal/updater's sentinels to statuses that mean
// something distinct to a client. Collapsing all of these into 500 would tell
// an operator to file a bug when the real answer is "re-run the installer"
// (403), "this is a dev build" (412), or "one is already running" (409). A
// failed checksum is 502 rather than 500: the upstream release served bytes
// that did not match its own manifest, and nothing local is wrong.
func writeUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, updater.ErrUpdateInProgress):
		writeError(w, http.StatusConflict, "invalid_request_error", err.Error())
	case errors.Is(err, updater.ErrDevBuild):
		writeError(w, http.StatusPreconditionFailed, "invalid_request_error", err.Error())
	case errors.Is(err, updater.ErrNotWritable):
		writeError(w, http.StatusForbidden, "permission_error", err.Error())
	case errors.Is(err, updater.ErrChecksumMismatch):
		writeError(w, http.StatusBadGateway, "api_error", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}
```

In `internal/proxy/router.go`, register it beside the probe route:

```go
		cp.Post("/api/v1/probe", probeNowHandler(o))
		cp.Post("/api/v1/update", applyUpdateHandler(o))
```

In `internal/proxy/lockstep_test.go`, add to `routeFor`:

```go
	"ApplyUpdate":         {http.MethodPost, ReservedPrefix + "/api/v1/update"},
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/proxy -race
go test ./...
```
Expected: PASS everywhere.

- [ ] **Step 5: Commit**

```bash
git add internal/proxy
git commit -m "feat(api): route POST /_aiproxy/api/v1/update"
```

---

### Task 8: Wiring the checker into the process lifecycle

**Files:**
- Modify: `cmd/aiproxy/main.go` (`buildHandler` constructs and returns the checker; `run` starts and stops it)
- Modify: `cmd/aiproxy/main_test.go`, `cmd/aiproxy/metrics_wiring_test.go` (the `buildHandler` call sites gain a return value)
- Test: `cmd/aiproxy/main_test.go` (add)

**Interfaces:**
- Consumes: `updater.New`, `updater.NewChecker`, `updater.DefaultRepo`, `updater.WithCheckerLogger` (Tasks 2–4); `cfg.Update` (Task 5); `view.NewLocal`'s new parameter (Task 6).
- Produces: `buildHandler(cfg, store, log, ing) (http.Handler, *prober.Prober, *view.Local, *updater.Checker, error)` — the checker is returned for the same reason the prober is: its background loop has its own `Start`/`Stop` lifecycle, owned by `run`, not by the handler.

- [ ] **Step 1: Write the failing test**

Add to `cmd/aiproxy/main_test.go`:

```go
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
```

Match this file's existing helpers (`quiet()`, `testIngester(t)`) and imports rather than introducing new ones.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/aiproxy -run TestStatusReportsTheRunningVersion`
Expected: FAIL — `buildHandler` returns 4 values, not 5.

- [ ] **Step 3: Write the implementation**

In `cmd/aiproxy/main.go`, add `"github.com/nicko170/aiproxy/internal/updater"` to the imports.

Change `buildHandler`'s signature and its doc comment's second paragraph:

```go
// buildHandler wires config into a serving handler. Kept separate from run so
// tests exercise the real composition without binding a port. The returned
// *prober.Prober and *updater.Checker are separate from the handler because
// each owns a background loop with its own lifecycle (Start/Stop), exactly
// like the roller and pruner run constructs alongside them; a caller that
// only wants the handler is free to ignore them.
func buildHandler(cfg config.Config, store *config.Store, log *slog.Logger, ing *metrics.Ingester) (http.Handler, *prober.Prober, *view.Local, *updater.Checker, error) {
```

Every `return ..., err` inside it gains a `nil` for the new value; the final return becomes `}), pb, vl, upd, nil`.

Construct the checker next to the prober, before `view.NewLocal`:

```go
	// In-app update checking. The Client is told the version this binary was
	// stamped with (see main.version); an unstamped "dev" build is never
	// offered an update, and never makes a request to find one. The Checker's
	// lifecycle matches the prober's above: constructed here, started and
	// stopped by run().
	upd := updater.NewChecker(
		updater.New(updater.DefaultRepo, version),
		cfg.Update.CheckEnabled,
		time.Duration(cfg.Update.CheckIntervalHours)*time.Hour,
		updater.WithCheckerLogger(log),
	)
```

Pass it into the seam:

```go
	vl := view.NewLocal(mgr, ing.Store(), store, cfg.Listen.Addr, ing.Dropped, pb, upd)
```

In `run`, take the new value and give it the same treatment as `pb`:

```go
	handler, pb, vl, upd, err := buildHandler(cfg, store, log, ing)
	if err != nil {
		return err
	}
	// The background loop is a no-op when quotaProbe.intervalSeconds is 0
	// (see prober.New's doc comment); ProbeNow still works either way, so
	// Start/Stop are unconditional exactly like the roller and pruner above.
	pb.Start()
	defer pb.Stop()

	// Same shape for the update checker: Start always spawns its goroutine so
	// Stop is always safe, and a disabled check simply never reaches the
	// network (see updater.Checker.Start).
	upd.Start()
	defer upd.Stop()
```

Update the eight existing `buildHandler` call sites in `cmd/aiproxy/main_test.go` and `cmd/aiproxy/metrics_wiring_test.go` to take the extra value:

```bash
# The four-underscore forms in those two files become five.
sed -i '' 's/h, _, _, err := buildHandler(/h, _, _, _, err := buildHandler(/' \
    cmd/aiproxy/main_test.go cmd/aiproxy/metrics_wiring_test.go
sed -i '' 's/if _, _, _, err := buildHandler(/if _, _, _, _, err := buildHandler(/' \
    cmd/aiproxy/main_test.go cmd/aiproxy/metrics_wiring_test.go
grep -n "buildHandler(" cmd/aiproxy/*_test.go   # confirm every call site took the edit
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./cmd/aiproxy -race
go test ./...
```
Expected: PASS. If any test in `cmd/aiproxy` now makes an outbound request, the checker's 30-second initial delay is why it does not — but set `cfg.Update.CheckEnabled = false` in any test that constructs a handler and lives longer than that.

- [ ] **Step 5: Commit**

```bash
git add cmd/aiproxy
git commit -m "feat(cmd): start the update checker with the proxy"
```

---

### Task 9: The TUI

**Files:**
- Modify: `internal/tui/app.go` (header segment, `u` key, `applyUpdate` command, `updateAppliedMsg`, `updateInstalled` field, footer, help)
- Modify: `internal/tui/frames_test.go` (two new frame cases)
- Test: `internal/tui/behavior_test.go` (add)
- Create: six golden files under `internal/tui/testdata/` (generated, not hand-written)

**Interfaces:**
- Consumes: `view.Status.Update`, `view.UpdateResult`, `view.Source.ApplyUpdate` (Task 6).
- Produces: nothing other packages consume.

**One addition beyond the spec's TUI section, called out deliberately:** the spec has the flash report `updated to 0.2.0 — restart to apply`, but a flash expires after five seconds and then nothing on screen says a restart is pending. So `Model` gains an `updateInstalled` field and the header shows `^ 0.2.0 installed · restart` in its place once an update lands. This is TUI-local state — no seam change, no new field on `UpdateStatus` — and it is why `Checker.Apply` clears `Available` (Task 4): the two states are mutually exclusive and the header renders exactly one of them.

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/behavior_test.go`:

```go
// The header must offer an update when one exists and say nothing at all when
// none does — a segment that is always present, reading "up to date", is
// noise on every frame for the sake of one.
func TestHeaderShowsAnAvailableUpdate(t *testing.T) {
	m := fixtureModel(120, 28)
	if strings.Contains(m.viewHeader(), "available") {
		t.Fatal("header mentions an update when none is available")
	}

	m.status.Update = view.UpdateStatus{
		CurrentVersion: "0.1.0", LatestVersion: "0.2.0", Available: true,
	}
	if !strings.Contains(m.viewHeader(), "0.2.0 available") {
		t.Errorf("header = %q, want it to offer 0.2.0", m.viewHeader())
	}
}

// Once installed, the header stops offering the update and starts asking for a
// restart: the flash that said so has five seconds, and the pending restart
// outlives it.
func TestHeaderAsksForARestartAfterInstalling(t *testing.T) {
	m := fixtureModel(120, 28)
	m.status.Update = view.UpdateStatus{CurrentVersion: "0.1.0", LatestVersion: "0.2.0"}
	m.updateInstalled = "0.2.0"
	h := m.viewHeader()
	if !strings.Contains(h, "0.2.0 installed") || !strings.Contains(h, "restart") {
		t.Errorf("header = %q, want it to ask for a restart", h)
	}
	if strings.Contains(h, "available") {
		t.Errorf("header = %q, must not offer and report the same update at once", h)
	}
}

// u with nothing available explains itself instead of failing.
func TestUWithNoUpdateAvailableFlashesAnExplanation(t *testing.T) {
	m := fixtureModel(80, 28)
	m.status.Update = view.UpdateStatus{CurrentVersion: "0.1.0", LatestVersion: "0.1.0"}
	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	got, _ := mustModel(next, cmd)
	if got.flash.text == "" {
		t.Error("u should say why it did nothing")
	}
	if cmd != nil {
		t.Error("u must not call the seam when there is nothing to install")
	}
}

// A dev build is told what it is, not that it is up to date.
func TestUOnADevBuildSaysSo(t *testing.T) {
	m := fixtureModel(80, 28)
	m.status.Update = view.UpdateStatus{CurrentVersion: "dev", DevBuild: true}
	next, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	got, _ := mustModel(next, cmd)
	if !strings.Contains(got.flash.text, "dev build") {
		t.Errorf("flash = %q, want it to mention a dev build", got.flash.text)
	}
}

// The result's Message is rendered verbatim: the wording of "restart to
// apply" lives in one place (view.Local.ApplyUpdate) so the TUI and the CLI
// cannot drift apart on it.
func TestUpdateAppliedMessageIsShownAndRemembered(t *testing.T) {
	m := fixtureModel(80, 28)
	next, _ := m.Update(updateAppliedMsg{res: view.UpdateResult{
		Updated: true, Version: "0.2.0", Message: "updated to 0.2.0 — restart to apply",
	}})
	got, _ := mustModel(next, nil)
	if got.flash.text != "updated to 0.2.0 — restart to apply" {
		t.Errorf("flash = %q", got.flash.text)
	}
	if got.updateInstalled != "0.2.0" {
		t.Errorf("updateInstalled = %q, want 0.2.0", got.updateInstalled)
	}
}

func TestUpdateFailureFlashesTheError(t *testing.T) {
	m := fixtureModel(80, 28)
	next, _ := m.Update(updateAppliedMsg{err: errFake("checksum mismatch")})
	got, _ := mustModel(next, nil)
	if !strings.Contains(got.flash.text, "checksum mismatch") {
		t.Errorf("flash = %q", got.flash.text)
	}
	if got.flash.sev != sevBad {
		t.Error("a failed update should read as bad")
	}
	if got.updateInstalled != "" {
		t.Error("a failed update must not claim a restart is pending")
	}
}
```

Add the two frame cases to `frameCases()` in `internal/tui/frames_test.go`. They are separate cases rather than a change to `fixtureModel` precisely so the existing 36 goldens stay byte-identical:

```go
		"overview_update_available": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.status.Update = view.UpdateStatus{
				CurrentVersion: "0.1.0", LatestVersion: "0.2.0", Available: true,
				ReleaseURL: "https://github.com/nicko170/aiproxy/releases/tag/v0.2.0",
				CheckedAt:  fixtureNow.Add(-2 * time.Hour).UnixMilli(),
			}
			return m
		},
		"overview_update_installed": func(w, h int) Model {
			m := fixtureModel(w, h)
			m.status.Update = view.UpdateStatus{CurrentVersion: "0.1.0", LatestVersion: "0.2.0"}
			m.updateInstalled = "0.2.0"
			return m
		},
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'TestHeaderShows|TestHeaderAsks|TestU|TestUpdate'`
Expected: FAIL — `m.updateInstalled` undefined, `updateAppliedMsg` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/tui/app.go`, add the field to `Model` beside `statusErr`:

```go
	// updateInstalled is the version a successful in-app update wrote to disk
	// this session, or "". It is TUI-local rather than a Status field because
	// it is a fact about this UI's own action, not about the running proxy:
	// the flash announcing it expires in five seconds and the pending restart
	// does not, so the header keeps saying so.
	updateInstalled string
```

Add the message type beside `openedMsg`:

```go
// updateAppliedMsg reports an in-app update attempt. It is not an actionMsg:
// the success wording comes from view.UpdateResult.Message (one place words
// "restart to apply", shared with the CLI) rather than from a past-tense verb
// chosen here.
type updateAppliedMsg struct {
	res view.UpdateResult
	err error
}
```

Add the command beside `probeNow`:

```go
// applyUpdate installs the latest release. The download runs in a command, on
// its own goroutine, with a timeout of its own: fetching several megabytes has
// nothing to do with fetchTimeout, which bounds a status read.
func (m Model) applyUpdate() tea.Cmd {
	src := m.src
	parent := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
		defer cancel()
		res, err := src.ApplyUpdate(ctx)
		return updateAppliedMsg{res: res, err: err}
	}
}
```

Handle the message in `Update`, after the `openedMsg` case:

```go
	case updateAppliedMsg:
		if msg.err != nil {
			m.flash = m.newFlash(sevBad, "update failed: "+msg.err.Error())
			return m, nil
		}
		if msg.res.Updated {
			m.updateInstalled = msg.res.Version
		}
		m.flash = m.newFlash(sevOK, msg.res.Message)
		return m, nil
```

Add the key, beside `p` and `o` in `handleKey`'s global switch:

```go
	case "u":
		return m.startUpdate()
```

And the guard it dispatches through:

```go
// startUpdate applies an available update, or explains why it will not. The
// three no-op cases are distinguished on purpose: "nothing available", "your
// check is switched off", and "this is a dev build" are different situations,
// and one shared "nothing to do" would leave two of them looking like a bug.
func (m Model) startUpdate() (tea.Model, tea.Cmd) {
	switch {
	case m.updateInstalled != "":
		m.flash = m.newFlash(sevOK, "already updated to "+m.updateInstalled+" — restart to apply")
	case m.status.Update.DevBuild:
		m.flash = m.newFlash(sevWarn, "dev build — install a release to update in place")
	case m.status.Update.Disabled:
		m.flash = m.newFlash(sevWarn, "update checking is off — enable it in settings")
	case !m.status.Update.Available:
		m.flash = m.newFlash(sevOK, "no newer release available")
	default:
		m.flash = m.newFlash(sevOK, "updating to "+m.status.Update.LatestVersion+"…")
		return m, m.applyUpdate()
	}
	return m, nil
}
```

(`internal/tui/theme.go` defines `sevOK`, `sevWarn`, and `sevBad`, and the `theme` helpers `ok`, `warn`, `bad`, `accent`, `dim`, `bold` — all three severities and both colour helpers used above already exist.)

In `viewHeader`, add the segment after the `statusErr` segment so it is the last thing shed by a narrow terminal:

```go
	// One segment, never both: Checker.Apply clears Available the moment an
	// update is installed, so "available" and "installed" cannot both be true.
	switch {
	case m.updateInstalled != "":
		seg := "↑ " + m.updateInstalled + " installed · restart"
		if th.mode == modeNone {
			seg = "^ " + m.updateInstalled + " installed - restart"
		}
		segs = append(segs, th.warn(seg))
	case m.status.Update.Available:
		seg := "↑ " + m.status.Update.LatestVersion + " available"
		if th.mode == modeNone {
			seg = "^ " + m.status.Update.LatestVersion + " available"
		}
		segs = append(segs, th.accent(seg))
	}
```

In `viewFooter`, add the key to the Overview hints (last, so it sheds first):

```go
		case screenOverview:
			keys = []string{"l login", "p probe", "o dashboard", "u update"}
```

In `viewHelp`, add a row after `{"o", "open the dashboard"}`:

```go
		{"u", "install the latest release"},
```

- [ ] **Step 4: Run the tests and generate the new goldens**

```bash
go test ./internal/tui -run 'TestHeader|TestU' -v
go test ./internal/tui -run TestGoldenFrames -update
git status --short internal/tui/testdata
git diff internal/tui/testdata
go test ./internal/tui -race
```

Expected: six NEW files (`overview_update_available_{60,80,120}.golden`, `overview_update_installed_{60,80,120}.golden`) and **exactly one modified** existing file group — `help_*.golden` and `overview_*.golden` change because the help row and the Overview footer hint were added. No `activity_*`, `usage_*`, `accounts_*`, `login_*`, or `settings_*` golden may change. Read the diff and confirm that; a change anywhere else means the header segment is rendering when it should not.

Then check the narrow widths behaved:

```bash
grep -c available internal/tui/testdata/overview_update_available_60.golden
```
At 60 columns the segment may legitimately be shed. Either outcome is fine — what must hold is `TestGoldenFrames`' own width and height invariants, plus `TestFramesSurviveExtremeWidths` at 40 and 500 columns.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): show available updates and install them with u"
```

---

### Task 10: The `aiproxy update` subcommand

**Files:**
- Create: `cmd/aiproxy/update.go`
- Modify: `cmd/aiproxy/main.go` (dispatch on `flag.Arg(0)`)
- Test: `cmd/aiproxy/update_test.go`

**Interfaces:**
- Consumes: `updater.New`, `updater.DefaultRepo`, the sentinels, `Client.Check`, `Client.Apply`, `Client.Newer` (Tasks 2–3).
- Produces: `func runUpdate(args []string, current string, out io.Writer, newClient func(current string) *updater.Client) int` — the exit code. `main` passes `os.Args`' tail, `version`, `os.Stdout`, and a real-client constructor; the test passes a fake one pointed at an `httptest.Server`.

Exit codes, which a script can branch on:

| Code | Meaning |
|---|---|
| 0 | up to date, no releases published, or the update was installed |
| 1 | `--check` only: an update is available |
| 2 | an error occurred |

The spec's §10 has exit 0 / exit 1 for `--check`; 2 for errors is added here so a script cannot mistake a network failure for "an update is available".

**One deliberate simplification:** the CLI does not read `update.checkEnabled` at all. That setting governs the *background* check — an outbound request nobody asked for. Typing `aiproxy update` IS the ask, and refusing it because a background poll is switched off would be obtuse. The CLI therefore opens no config store, which also means it works before a config file exists.

- [ ] **Step 1: Write the failing test**

Create `cmd/aiproxy/update_test.go`:

```go
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
	dir := t.TempDir()
	exe := filepath.Join(dir, "aiproxy")
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
```

Add one test to `cmd/aiproxy/main_test.go` asserting the dispatch itself refuses typos rather than silently booting a proxy:

```go
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

func TestNoSubcommandRunsTheServer(t *testing.T) {
	var out bytes.Buffer
	if code := dispatchSubcommand(nil, &out); code != -1 {
		t.Errorf("exit code = %d, want -1 (meaning: not a subcommand, run the server)", code)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/aiproxy -run 'TestUpdate|TestUnknownSubcommand|TestNoSubcommand'`
Expected: FAIL — `undefined: runUpdate`, `undefined: dispatchSubcommand`.

- [ ] **Step 3: Write the implementation**

Create `cmd/aiproxy/update.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/nicko170/aiproxy/internal/updater"
)

// Exit codes for the update subcommand. 1 means "an update is available" for
// --check, which is what makes it scriptable; 2 is reserved for errors so a
// wrapper cannot mistake a network failure for good news.
const (
	updateExitOK        = 0
	updateExitAvailable = 1
	updateExitError     = 2
)

// realUpdateClient is the constructor main passes to runUpdate. It is injected
// rather than called directly so a test can point the whole flow at an
// httptest.Server and a temp file without a network or a real install.
func realUpdateClient(current string) *updater.Client {
	return updater.New(updater.DefaultRepo, current)
}

// dispatchSubcommand handles the argument forms that are not "run the proxy".
// It returns -1 when args name no subcommand, meaning main should carry on and
// start the server; any other value is an exit code.
//
// This is a check of the first argument rather than a subcommand framework:
// there is exactly one subcommand, and a framework would be more machinery
// than the feature. An unrecognized first argument is an error rather than a
// silently ignored one — a typo must not boot a proxy.
func dispatchSubcommand(args []string, out io.Writer) int {
	if len(args) == 0 {
		return -1
	}
	switch args[0] {
	case "update":
		return runUpdate(args[1:], version, out, realUpdateClient)
	default:
		fmt.Fprintf(out, "aiproxy: unknown command %q\nthe only subcommand is: update\nrun aiproxy --help for flags\n", args[0])
		return updateExitError
	}
}

// runUpdate implements "aiproxy update" and "aiproxy update --check".
//
// It deliberately does not read the config file. update.checkEnabled governs
// the BACKGROUND check — an outbound request nobody asked for. Running this
// command is the ask, so honouring that setting here would only make an
// explicit instruction fail for a reason the operator did not intend. Not
// touching the config store also means this works before one exists.
func runUpdate(args []string, current string, out io.Writer, newClient func(string) *updater.Client) int {
	fs := flag.NewFlagSet("aiproxy update", flag.ContinueOnError)
	fs.SetOutput(out)
	checkOnly := fs.Bool("check", false, "report whether an update is available without installing it")
	if err := fs.Parse(args); err != nil {
		return updateExitError
	}

	c := newClient(current)
	ctx := context.Background()

	rel, err := c.Check(ctx)
	switch {
	case errors.Is(err, updater.ErrDevBuild):
		fmt.Fprintln(out, "aiproxy: this is a dev build, not a release, so there is nothing to compare against")
		fmt.Fprintln(out, "install a release first: https://github.com/"+updater.DefaultRepo+"/releases")
		return updateExitError
	case errors.Is(err, updater.ErrNoReleases):
		fmt.Fprintln(out, "aiproxy: no releases published yet")
		return updateExitOK
	case err != nil:
		fmt.Fprintln(out, "aiproxy:", err)
		return updateExitError
	}

	if !c.Newer(rel) {
		fmt.Fprintf(out, "aiproxy %s is the latest release\n", current)
		return updateExitOK
	}
	if *checkOnly {
		fmt.Fprintf(out, "aiproxy %s is available (running %s)\n%s\n", rel.Version, current, rel.PageURL)
		return updateExitAvailable
	}

	fmt.Fprintf(out, "downloading aiproxy %s…\n", rel.Version)
	res, err := c.Apply(ctx, rel)
	switch {
	case errors.Is(err, updater.ErrNotWritable):
		fmt.Fprintln(out, "aiproxy:", err)
		fmt.Fprintln(out, "re-run the installer, or update through whichever package manager owns that path:")
		fmt.Fprintln(out, "  curl -fsSL https://raw.githubusercontent.com/"+updater.DefaultRepo+"/main/install.sh | sh")
		return updateExitError
	case errors.Is(err, updater.ErrChecksumMismatch):
		fmt.Fprintln(out, "aiproxy:", err)
		fmt.Fprintln(out, "nothing was changed. try again; if it persists, report it.")
		return updateExitError
	case err != nil:
		fmt.Fprintln(out, "aiproxy:", err)
		return updateExitError
	}

	fmt.Fprintf(out, "updated %s → %s at %s\n", res.PreviousVersion, res.Version, res.Path)
	fmt.Fprintln(out, "restart aiproxy to run the new version")
	return updateExitOK
}
```

In `cmd/aiproxy/main.go`, dispatch before any flag is parsed, at the very top of `main`:

```go
func main() {
	// Subcommands are dispatched before flag.Parse so that "aiproxy update
	// --check" reaches the subcommand's own FlagSet: the top-level flag package
	// stops at the first non-flag argument, and --check is not a server flag.
	if code := dispatchSubcommand(os.Args[1:], os.Stdout); code >= 0 {
		os.Exit(code)
	}

	var (
		configPath = flag.String("config", "", "path to config.json (default: XDG config dir)")
		// ... unchanged
```

Careful: `os.Args[1:]` begins with a flag (`-addr`, `--headless`, …) in the normal case, and `dispatchSubcommand` would report it as an unknown command. Guard that in `dispatchSubcommand` — replace its length check with:

```go
	// A leading "-" is a flag, not a subcommand: the server's own flags are
	// parsed by main, and this function must not intercept them.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return -1
	}
```

and add `"strings"` to `update.go`'s imports.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./cmd/aiproxy -race
go test ./...
go build -o /tmp/aiproxy ./cmd/aiproxy
/tmp/aiproxy update --check; echo "exit: $?"
/tmp/aiproxy nonsense; echo "exit: $?"
/tmp/aiproxy --version
```
Expected: the tests PASS. `/tmp/aiproxy update --check` on an unstamped build prints the dev-build explanation and exits 2. `nonsense` exits 2 naming `update`. `--version` still prints `aiproxy dev`, proving the dispatch did not swallow flags.

- [ ] **Step 5: Commit**

```bash
git add cmd/aiproxy
git commit -m "feat(cmd): add the aiproxy update subcommand"
```

---

### Task 11: Documentation

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing.

- [ ] **Step 1: Add an Updating section**

Place it directly after the existing Install section, since it answers the question Install raises next:

```markdown
## Updating

aiproxy checks for a newer release once a day and shows it in the TUI header:

```
● aiproxy  ·  on 127.0.0.1:3456  ·  up 4h 12m  ·  2 in flight  ·  ↑ 0.2.0 available
```

Press `u` to install it, or from a shell:

```sh
aiproxy update           # install the latest release
aiproxy update --check   # report only; exits 1 when an update is available
```

Either way the download is verified against the release's `checksums.txt`
before anything is replaced, and the running process keeps serving on the old
binary until you restart it — an update never interrupts a request in flight.

If aiproxy was installed somewhere it cannot write (a Homebrew or root-owned
path), it says so and points you back at the installer rather than reaching for
`sudo`.

**Turning it off.** The check is one HTTPS request to github.com per day, which
tells GitHub this installation's IP address and version. To stop it, set it in
the Settings screen (`5`) or in `config.json`:

```json
"update": {
  "checkEnabled": false,
  "checkIntervalHours": 24
}
```

With `checkEnabled` false, aiproxy makes no outbound request at all —
`aiproxy update` still works, because running it is an explicit request.

**What is verified, and what is not.** `checksums.txt` is fetched over TLS from
the same origin as the release asset, so it defends against a corrupted or
truncated download. It is not a signature: anyone who could publish a release
could publish its checksum too. Release signing is not implemented yet.
```

- [ ] **Step 2: Update the Configuration section**

`README.md`'s Configuration section (line 107) has a JSON block listing every
config key and a bullet list explaining the load-bearing ones. Add the new key
to the JSON, after `"mitm"`:

```json
  "mitm": { "enabled": true },
  "update": { "checkEnabled": true, "checkIntervalHours": 24 }
```

and a bullet after the `quotaProbe.intervalSeconds` one:

```markdown
- **`update.checkEnabled`** turns the daily release check on or off and takes
  effect immediately; **`checkIntervalHours`** takes effect on restart, because
  the checker's ticker is built once at startup. See [Updating](#updating).
```

Also add a row to the Flags table's neighbourhood — the table lists flags, and
`update` is a subcommand rather than a flag, so put it immediately below the
table as a line of prose:

```markdown
One subcommand: `aiproxy update` (see [Updating](#updating)). Everything else is
flags.
```

- [ ] **Step 3: Update the Development section**

The release notes there already say asset naming is load-bearing because
`install.sh` constructs it. Extend that sentence: `internal/updater` constructs
the same string, so the name is now depended on in three places —
`.github/workflows/release.yml`, `install.sh`, and
`internal/updater/updater.go`'s `release` method.

- [ ] **Step 4: Verify every claim against the code**

```bash
grep -n "checkEnabled\|checkIntervalHours" internal/config/config.go
grep -n '"u"' internal/tui/app.go
grep -n "case \"update\"" cmd/aiproxy/update.go
go test ./...
```
Every command, key, flag, exit code, and default named in the README must be
confirmed here rather than assumed. If the README shows a header line, render a
real one and copy it:

```bash
go test ./internal/tui -run TestGoldenFrames -update >/dev/null 2>&1
head -1 internal/tui/testdata/overview_update_available_120.golden
```

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document in-app updates and the opt-out"
```

---

## Done when

```bash
go test ./... -race
go vet ./...
gofmt -l .        # must print nothing
```

All green, and:

- `aiproxy update --check` exits 1 against a repo with a newer release, 0 when current, 2 on error.
- The TUI header offers an available update, `u` installs it, and the header then asks for a restart.
- `update.checkEnabled: false` produces zero requests to github.com.
- A checksum mismatch leaves the installed binary byte-identical.

After this lands, the held commits go up and `v0.1.0` is cut (`git tag v0.1.0 && git push origin v0.1.0`), so the first release is self-updateable. Then `install.sh` gets run for real against the published release and the installed binary must report `aiproxy 0.1.0`.
