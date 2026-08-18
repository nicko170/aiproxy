package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
