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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		"aiproxy":   []byte(body),
		"LICENSE":   []byte("MIT"),
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
// there is no point spending an operator's bandwidth on a swap that cannot
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
