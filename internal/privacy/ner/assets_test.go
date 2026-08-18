package ner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestEnsureDownloadsAndVerifies(t *testing.T) {
	body := []byte("pretend this is a model")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{Name: "model.onnx", URL: srv.URL + "/model.onnx", SHA256: digest(body)}}
	if Present(dir, assets) {
		t.Fatal("Present reported assets before any download")
	}
	if err := Ensure(context.Background(), dir, assets, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "model.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("content = %q", got)
	}
	if !Present(dir, assets) {
		t.Error("Present is false after a successful download")
	}
}

// A wrong digest must leave nothing behind that could later be loaded.
func TestEnsureRefusesAMismatchedDigestAndLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("tampered"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{Name: "model.onnx", URL: srv.URL + "/m", SHA256: digest([]byte("expected"))}}
	err := Ensure(context.Background(), dir, assets, nil)
	if err == nil {
		t.Fatal("Ensure accepted a mismatched digest")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error should name the digest mismatch: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("left behind %s; nothing unverified may remain where it could be loaded", e.Name())
	}
}

// An already-present, correct asset is not re-downloaded — 800MB is not
// something to fetch twice.
func TestEnsureSkipsAVerifiedAsset(t *testing.T) {
	body := []byte("already here")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{Name: "m.onnx", URL: srv.URL + "/m", SHA256: digest(body)}}
	if err := Ensure(context.Background(), dir, assets, nil); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(context.Background(), dir, assets, nil); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("downloaded %d times, want 1", hits)
	}
}

func TestEnsureReportsProgress(t *testing.T) {
	body := make([]byte, 1<<16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "65536")
		w.Write(body)
	}))
	defer srv.Close()

	var lastDone int64
	err := Ensure(context.Background(), t.TempDir(),
		[]Asset{{Name: "m", URL: srv.URL + "/m", SHA256: digest(body), Bytes: int64(len(body))}},
		func(_ string, done, _ int64) { lastDone = done })
	if err != nil {
		t.Fatal(err)
	}
	if lastDone != int64(len(body)) {
		t.Errorf("final progress = %d, want %d", lastDone, len(body))
	}
}

func TestAssetsRejectAnUnsupportedPlatform(t *testing.T) {
	if _, err := Assets("windows", "amd64"); err == nil {
		t.Error("windows must be rejected; the release pipeline ships darwin and linux only")
	}
	for _, p := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}} {
		got, err := Assets(p[0], p[1])
		if err != nil {
			t.Errorf("Assets(%q,%q): %v", p[0], p[1], err)
		}
		if len(got) == 0 {
			t.Errorf("Assets(%q,%q) returned nothing", p[0], p[1])
		}
		for _, a := range got {
			if a.SHA256 == "" || a.URL == "" || a.Name == "" {
				t.Errorf("incomplete asset: %+v", a)
			}
		}
	}
}

// buildTarGz writes a .tar.gz containing the given entries in order. A
// symlink entry is represented by a target starting with "-> ".
func buildTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		if target, ok := strings.CutPrefix(content, "-> "); ok {
			if err := tw.WriteHeader(&tar.Header{
				Name:     name,
				Typeflag: tar.TypeSymlink,
				Linkname: target,
			}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Typeflag: tar.TypeReg,
			Mode:     0o755,
			Size:     int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
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

// Ensure must resolve a multi-hop symlink chain inside the archive — exactly
// what the real ONNX Runtime Linux tarball does:
// lib/libonnxruntime.so -> libonnxruntime.so.1 -> libonnxruntime.so.1.23.2.
func TestEnsureExtractsAnArchiveMemberThroughASymlinkChain(t *testing.T) {
	realData := []byte("pretend this is libonnxruntime.so.1.23.2")
	tgz := buildTarGz(t, map[string]string{
		"ort-linux-x64-1.23.2/lib/libonnxruntime.so.1.23.2": string(realData),
		"ort-linux-x64-1.23.2/lib/libonnxruntime.so.1":      "-> libonnxruntime.so.1.23.2",
		"ort-linux-x64-1.23.2/lib/libonnxruntime.so":        "-> libonnxruntime.so.1",
		"ort-linux-x64-1.23.2/include/onnxruntime_c_api.h":  "irrelevant header content",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tgz)
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{
		Name:          "libonnxruntime.so",
		URL:           srv.URL + "/ort.tgz",
		ArchiveSHA256: digest(tgz),
		ExtractMember: "libonnxruntime.so",
		SHA256:        digest(realData),
	}}
	if err := Ensure(context.Background(), dir, assets, nil); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "libonnxruntime.so"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(realData) {
		t.Errorf("content = %q, want %q", got, realData)
	}
	// The tarball itself must never land in dir under any name.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want exactly the extracted library: %v", len(entries), entries)
	}
	if !Present(dir, assets) {
		t.Error("Present is false after a successful extraction")
	}
}

// A correct archive digest does not excuse a tampered or mismatched member:
// the extracted file is verified independently, and a failure here must also
// leave nothing behind.
func TestEnsureRefusesAMismatchedExtractedMemberAndLeavesNoFile(t *testing.T) {
	tgz := buildTarGz(t, map[string]string{
		"ort/lib/libonnxruntime.so": "not what SHA256 expects",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tgz)
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{
		Name:          "libonnxruntime.so",
		URL:           srv.URL + "/ort.tgz",
		ArchiveSHA256: digest(tgz),
		ExtractMember: "libonnxruntime.so",
		SHA256:        digest([]byte("something else entirely")),
	}}
	err := Ensure(context.Background(), dir, assets, nil)
	if err == nil {
		t.Fatal("Ensure accepted a mismatched extracted-member digest")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error should name the digest mismatch: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("left behind %s; nothing unverified may remain where it could be loaded", e.Name())
	}
}

// A tampered archive must be rejected before anything is extracted from it at
// all — the archive digest gate must fire first.
func TestEnsureRefusesATamperedArchiveAndLeavesNoFile(t *testing.T) {
	tgz := buildTarGz(t, map[string]string{
		"ort/lib/libonnxruntime.so": "real library bytes",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tgz)
	}))
	defer srv.Close()

	dir := t.TempDir()
	assets := []Asset{{
		Name:          "libonnxruntime.so",
		URL:           srv.URL + "/ort.tgz",
		ArchiveSHA256: digest([]byte("this is not the archive that will be served")),
		ExtractMember: "libonnxruntime.so",
		SHA256:        digest([]byte("real library bytes")),
	}}
	err := Ensure(context.Background(), dir, assets, nil)
	if err == nil {
		t.Fatal("Ensure accepted a mismatched archive digest")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error should name the digest mismatch: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("left behind %s; nothing unverified may remain where it could be loaded", e.Name())
	}
}
