package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"errors"
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
