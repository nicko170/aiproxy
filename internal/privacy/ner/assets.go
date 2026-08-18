// Package ner is the model tier of the privacy filter: openai/privacy-filter run
// in-process through a vendored purego binding to ONNX Runtime.
package ner

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
	"path"
	"path/filepath"
	"time"

	"github.com/nicko170/aiproxy/internal/config"
)

// modelRevision pins the Hugging Face commit every asset URL resolves against.
// Immutable by construction, so a digest mismatch always means the download
// broke — never that upstream replaced a file under us.
const modelRevision = "7ffa9a043d54d1be65afb281eddf0ffbe629385b"

// ortVersion pins the ONNX Runtime release. A native library is being loaded into
// this process; a segfault in it is not recoverable in Go, so the version is
// pinned to one that has been exercised rather than tracking latest.
//
// v1.23.2 rather than latest: v1.29.0 ships only osx-arm64, having dropped
// macOS x86_64, so it cannot satisfy the four-platform table below. v1.23.x
// ships all four, and is the version the vendored purego binding
// (internal/privacy/onnxrt) states it targets — pinning anything else would
// mix ABI versions between the Go struct layout and the loaded library.
const ortVersion = "1.23.2"

// Asset is one file to fetch and verify.
type Asset struct {
	Name   string
	URL    string
	SHA256 string
	// Bytes is the expected size, used only for progress reporting.
	Bytes int64

	// ArchiveSHA256 and ExtractMember are set together, only for assets whose
	// URL points at a .tar.gz rather than at Name directly — the ONNX Runtime
	// release tarballs, which bundle headers, cmake files, and the shared
	// library together.
	//
	// The sequence still verifies before it trusts: the whole archive is
	// downloaded and ArchiveSHA256 is checked against it BEFORE anything is
	// unpacked, exactly as SHA256 gates a plain download. Only then is
	// ExtractMember located inside the archive (by base filename, following
	// any symlink chain — ONNX Runtime ships libonnxruntime.so as a chain of
	// two symlinks down to the real libonnxruntime.so.1.23.2, and
	// libonnxruntime.dylib as one) and the regular file it resolves to is
	// checked against SHA256 before being renamed into place at Name. Both
	// digests are independent facts recorded in Step 1: GitHub does not
	// publish per-asset digests, so the archive digest was computed locally
	// from the downloaded release asset, and the library digest from the
	// file inside it once extracted.
	ArchiveSHA256 string
	ExtractMember string
}

// Dir is where assets live: beside the config, not in the binary. Neither the
// runtime library nor the ~800MB of weights ships in the release tarball, which
// is what keeps it around 13MB.
func Dir() string { return filepath.Join(config.Dir(), "models", "privacy-filter") }

// Assets is the fetch list for one platform.
func Assets(goos, goarch string) ([]Asset, error) {
	hf := func(p string) string {
		return "https://huggingface.co/openai/privacy-filter/resolve/" + modelRevision + "/" + p
	}
	out := []Asset{
		{
			Name:   "tokenizer.json",
			URL:    hf("tokenizer.json"),
			SHA256: "0614fe83cadab421296e664e1f48f4261fa8fef6e03e63bb75c20f38e37d07d3",
			Bytes:  27868174,
		},
		{
			Name:   "config.json",
			URL:    hf("config.json"),
			SHA256: "b2b26a4a4a000639ad30b0c264adbefe365bdb567fbd7bb27303b8c438375bd1",
			Bytes:  3039,
		},
		{
			Name:   "viterbi_calibration.json",
			URL:    hf("viterbi_calibration.json"),
			SHA256: "bbc8611ef08a55ed72d64856cbbbb9a91db8dfa881f0a92e2afbad6e4bbc775a",
			Bytes:  372,
		},
		{
			Name:   "model_q4f16.onnx",
			URL:    hf("onnx/model_q4f16.onnx"),
			SHA256: "eaae4e83cf1345a60abe333ed882b55fe5775d1dfbf34b9b269e5e5416f45e5b",
			Bytes:  165744,
		},
		// The external-data sidecar: model_q4f16.onnx is an external-data
		// export, so the weights live here rather than in the .onnx file
		// itself. Both are required — the model will not load without it.
		{
			Name:   "model_q4f16.onnx_data",
			URL:    hf("onnx/model_q4f16.onnx_data"),
			SHA256: "6d4dde787e03ace283c45d4e32a94eec32b6cfcc242e7219bea96f5b4c13569d",
			Bytes:  809061992,
		},
	}

	lib, err := runtimeAsset(goos, goarch)
	if err != nil {
		return nil, err
	}
	return append(out, lib), nil
}

// runtimeAsset is the ONNX Runtime shared library for a platform. Windows is
// absent deliberately: the release pipeline ships darwin and linux only, and
// replacing a running binary by rename — which the updater relies on — is not
// possible there anyway.
//
// Each entry names a .tar.gz (ArchiveSHA256, computed locally per Step 1 since
// GitHub release assets carry no published digest) and the library member to
// extract from it (ExtractMember, resolved by following symlinks — see
// Asset.ExtractMember). SHA256 is the digest of that extracted library file,
// not of the archive; that is what Present and Ensure check against what
// ends up on disk at Name, and what makes an already-installed library
// recognized as such without re-downloading and re-extracting the archive.
func runtimeAsset(goos, goarch string) (Asset, error) {
	key := goos + "/" + goarch
	base := "https://github.com/microsoft/onnxruntime/releases/download/v" + ortVersion + "/"
	table := map[string]Asset{
		"darwin/arm64": {
			Name:          "libonnxruntime.dylib",
			URL:           base + "onnxruntime-osx-arm64-" + ortVersion + ".tgz",
			ArchiveSHA256: "b4d513ab2b26f088c66891dbbc1408166708773d7cc4163de7bdca0e9bbb7856",
			ExtractMember: "libonnxruntime.dylib",
			SHA256:        "d306d2bc768540766c7ed8a1e0ff05d2870c77a934ebeee4a7bafa1b732ef299",
			Bytes:         9999931,
		},
		"darwin/amd64": {
			Name:          "libonnxruntime.dylib",
			URL:           base + "onnxruntime-osx-x86_64-" + ortVersion + ".tgz",
			ArchiveSHA256: "d10359e16347b57d9959f7e80a225a5b4a66ed7d7e007274a15cae86836485a6",
			ExtractMember: "libonnxruntime.dylib",
			SHA256:        "8c9c78de65ea3786f987c0d980e9c1b13a3a5fbc6b3e2965ba05b450e6e4c054",
			Bytes:         11676322,
		},
		"linux/amd64": {
			Name:          "libonnxruntime.so",
			URL:           base + "onnxruntime-linux-x64-" + ortVersion + ".tgz",
			ArchiveSHA256: "1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b",
			ExtractMember: "libonnxruntime.so",
			SHA256:        "13ab8084954fa4a47c777880180b90810d6020f021441395712b48a75b74c68b",
			Bytes:         8309231,
		},
		"linux/arm64": {
			Name:          "libonnxruntime.so",
			URL:           base + "onnxruntime-linux-aarch64-" + ortVersion + ".tgz",
			ArchiveSHA256: "7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c",
			ExtractMember: "libonnxruntime.so",
			SHA256:        "648ffa64fbe027ae27139109410900cf776a030dec2dbbac51053318cc44c286",
			Bytes:         7254068,
		},
	}
	a, ok := table[key]
	if !ok {
		return Asset{}, fmt.Errorf("ner: no ONNX Runtime build for %s", key)
	}
	return a, nil
}

// Present reports whether every asset is on disk with the right digest.
func Present(dir string, assets []Asset) bool {
	for _, a := range assets {
		if !verified(filepath.Join(dir, a.Name), a.SHA256) {
			return false
		}
	}
	return true
}

// Ensure downloads whatever is missing or wrong, verifying each file before it is
// put where it could be loaded.
//
// Same sequence as internal/updater.Apply, and for the same reasons: download to
// a temp file IN THE TARGET DIRECTORY so the rename is atomic on one filesystem,
// verify, then rename. Nothing unverified is ever moved into place, and a partial
// download cannot be mistaken for a complete one. For archive assets the same
// discipline applies one layer deeper: the archive is verified before anything
// is extracted from it, and the extracted member is itself verified before its
// rename.
func Ensure(ctx context.Context, dir string, assets []Asset, progress func(name string, done, total int64)) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ner: create %s: %w", dir, err)
	}
	client := &http.Client{Timeout: 2 * time.Hour} // 800MB on a slow link
	for _, a := range assets {
		final := filepath.Join(dir, a.Name)
		if verified(final, a.SHA256) {
			continue
		}
		if err := fetch(ctx, client, dir, final, a, progress); err != nil {
			return err
		}
	}
	return nil
}

func fetch(ctx context.Context, client *http.Client, dir, final string, a Asset,
	progress func(string, int64, int64)) error {

	tmp, err := os.CreateTemp(dir, "."+a.Name+"-*")
	if err != nil {
		return fmt.Errorf("ner: %w", err)
	}
	defer os.Remove(tmp.Name())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("ner: fetch %s: %w", a.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("ner: fetch %s: status %d", a.Name, resp.StatusCode)
	}

	total := a.Bytes
	if total == 0 {
		total = resp.ContentLength
	}
	h := sha256.New()
	var done int64
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				return werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil {
				progress(a.Name, done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return fmt.Errorf("ner: download %s: %w", a.Name, rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// The digest gating what happens next: an archive's own digest before
	// anything is unpacked from it, or the digest of the file that lands
	// directly at Name.
	wantDigest := a.SHA256
	if a.ExtractMember != "" {
		wantDigest = a.ArchiveSHA256
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantDigest {
		return fmt.Errorf("ner: %s failed verification: expected sha256 %s, got %s",
			a.Name, wantDigest, got)
	}

	if a.ExtractMember == "" {
		return os.Rename(tmp.Name(), final)
	}
	return extractAndRename(dir, final, tmp.Name(), a)
}

// extractAndRename pulls a.ExtractMember out of the now-verified archive at
// archivePath, verifies IT against a.SHA256, and only then renames it into
// place at final. The archive itself is never renamed into place — final
// only ever holds the library, never the tarball.
func extractAndRename(dir, final, archivePath string, a Asset) error {
	data, err := extractArchiveMember(archivePath, a.ExtractMember)
	if err != nil {
		return fmt.Errorf("ner: extract %s from %s: %w", a.ExtractMember, a.Name, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != a.SHA256 {
		return fmt.Errorf("ner: %s failed verification: expected sha256 %s, got %s",
			a.Name, a.SHA256, got)
	}

	out, err := os.CreateTemp(dir, "."+a.Name+"-*")
	if err != nil {
		return fmt.Errorf("ner: %w", err)
	}
	defer os.Remove(out.Name())
	if _, err := out.Write(data); err != nil {
		out.Close()
		return fmt.Errorf("ner: write %s: %w", a.Name, err)
	}
	// Shared libraries need the execute bit; a plain download would not have
	// it, since ONNX Runtime's tarball is the only asset that is executable.
	if err := out.Chmod(0o755); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), final)
}

// maxArchiveMemberBytes bounds any single file read out of a runtime tarball.
// The archives here are pinned, locally-verified releases (tens of MB), not
// untrusted input, but the cap is cheap insurance against a corrupt archive
// claiming an absurd size.
const maxArchiveMemberBytes = 512 << 20

// extractArchiveMember reads a .tar.gz archive and returns the contents of
// the regular file that member ultimately names, following any symlink chain.
// ONNX Runtime's release tarballs ship the shared library this way — e.g. on
// Linux, lib/libonnxruntime.so -> libonnxruntime.so.1 -> libonnxruntime.so.1.23.2
// — so member is resolved by base filename only: the archive's top-level
// directory embeds the platform triple and version
// (onnxruntime-osx-arm64-1.23.2/lib/...), which is not worth threading
// through the caller, and symlink targets inside the tarball are themselves
// bare filenames relative to the same directory.
func extractArchiveMember(archivePath, member string) ([]byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()

	type entry struct {
		symlink bool
		target  string // base name of the symlink target
		data    []byte
	}
	found := make(map[string]entry)

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		base := path.Base(h.Name)
		switch h.Typeflag {
		case tar.TypeSymlink:
			found[base] = entry{symlink: true, target: path.Base(h.Linkname)}
		case tar.TypeReg:
			data, err := io.ReadAll(io.LimitReader(tr, maxArchiveMemberBytes+1))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", h.Name, err)
			}
			if len(data) > maxArchiveMemberBytes {
				return nil, fmt.Errorf("%s in the archive exceeds the %d-byte limit",
					h.Name, int64(maxArchiveMemberBytes))
			}
			found[base] = entry{data: data}
		}
	}

	// Follow the symlink chain to the regular file it ultimately names. A
	// bound on the number of hops turns a cyclic or absurdly long chain into
	// an error instead of a hang.
	name := member
	for range 8 {
		e, ok := found[name]
		if !ok {
			return nil, fmt.Errorf("archive does not contain %q", name)
		}
		if !e.symlink {
			return e.data, nil
		}
		name = e.target
	}
	return nil, fmt.Errorf("symlink chain for %q is too long or cyclic", member)
}

func verified(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}
