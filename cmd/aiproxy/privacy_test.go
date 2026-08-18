package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/config"
	"github.com/nicko170/aiproxy/internal/privacy/ner"
)

// TestHumanBytesUsesDecimalMegabytes pins humanBytes to the decimal
// convention (1 MB = 1e6 bytes) that download and disk sizes are quoted in,
// and that the README's "~850 MB" figure assumes. Using 1<<20 (MiB) while
// printing the label "MB" would understate the total by about 5% — the
// review finding this test guards against.
func TestHumanBytesUsesDecimalMegabytes(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{1_000_000, "~1 MB"},
		{847_099_252, "~847 MB"}, // darwin/arm64 total, per Assets
		{809_061_992, "~809 MB"}, // model_q4f16.onnx_data alone
	}
	for _, c := range cases {
		if got := humanBytes(c.bytes); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

// TestPrivacyInstallReportsSizeMatchingTheDocumentedFootprint proves the size
// line "aiproxy privacy install" actually prints, for every platform Assets
// supports, lands in the ~840-850 MB range the README documents five times —
// not the ~805-809 MB that printing MiB-computed bytes under an "MB" label
// would produce.
func TestPrivacyInstallReportsSizeMatchingTheDocumentedFootprint(t *testing.T) {
	platforms := [][2]string{
		{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"},
	}
	for _, p := range platforms {
		goos, goarch := p[0], p[1]
		assets, err := ner.Assets(goos, goarch)
		if err != nil {
			t.Fatalf("Assets(%s, %s): %v", goos, goarch, err)
		}
		var total int64
		for _, a := range assets {
			total += a.Bytes
		}
		got := humanBytes(total)
		mb := float64(total) / 1e6
		if mb < 840 || mb > 850 {
			t.Fatalf("%s/%s: total = %.1f MB, want 840-850 (README says ~850 MB)", goos, goarch, mb)
		}
		// Parse the number humanBytes actually printed and check it against
		// the same 840-850 range, rather than trusting the arithmetic in
		// isolation: this is what would have caught the MiB-under-MB bug,
		// since that bug prints ~805-809, outside the range.
		var printedMB float64
		if _, err := fmt.Sscanf(got, "~%f MB", &printedMB); err != nil {
			t.Fatalf("%s/%s: humanBytes(%d) = %q, not parseable as \"~N MB\": %v", goos, goarch, total, got, err)
		}
		if printedMB < 840 || printedMB > 850 {
			t.Errorf("%s/%s: humanBytes printed %q (%.1f MB), want 840-850 to match the README's ~850 MB", goos, goarch, got, printedMB)
		}
	}
}

// TestPrivacyInstallDownloadsAssets proves "aiproxy privacy install" fetches
// and verifies assets against an arbitrary URL and digest, without touching
// the real Hugging Face/GitHub hosts or ner.Dir.
func TestPrivacyInstallDownloadsAssets(t *testing.T) {
	body := []byte("model bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	sum := sha256.Sum256(body)
	assets := []ner.Asset{{Name: "m.onnx", URL: srv.URL + "/m", SHA256: hex.EncodeToString(sum[:])}}
	dir := t.TempDir()

	var out bytes.Buffer
	if code := runPrivacy([]string{"install"}, &out, dir, assets); code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "m.onnx")); err != nil {
		t.Errorf("asset not installed: %v", err)
	}
	if !strings.Contains(out.String(), dir) {
		t.Errorf("output should name where it installed: %s", out.String())
	}
}

// TestPrivacyStatusReportsMissingAssets proves "aiproxy privacy status" exits
// non-zero and names the fix when assets are absent.
func TestPrivacyStatusReportsMissingAssets(t *testing.T) {
	assets := []ner.Asset{{Name: "m.onnx", URL: "https://example.invalid/m", SHA256: "00"}}
	var out bytes.Buffer
	if code := runPrivacy([]string{"status"}, &out, t.TempDir(), assets); code != 2 {
		t.Errorf("exit code = %d, want 2 when assets are missing", code)
	}
	if !strings.Contains(out.String(), "install") {
		t.Errorf("output should name the fix: %s", out.String())
	}
}

// TestPrivacyRejectsAnUnknownAction proves a typo in the action is an error,
// not a silent no-op.
func TestPrivacyRejectsAnUnknownAction(t *testing.T) {
	var out bytes.Buffer
	if code := runPrivacy([]string{"frobnicate"}, &out, t.TempDir(), nil); code != 2 {
		t.Error("an unknown action must be rejected")
	}
}

// TestPrivacySubcommandIsDispatched proves dispatchSubcommand routes
// "privacy" to runPrivacy rather than treating it as an unknown command.
func TestPrivacySubcommandIsDispatched(t *testing.T) {
	var out bytes.Buffer
	// "privacy" with no action prints usage and exits 2, which proves dispatch
	// reached it rather than treating it as an unknown command.
	code := dispatchSubcommand([]string{"privacy"}, &out)
	if code != 2 || strings.Contains(out.String(), "unknown command") {
		t.Errorf("privacy was not dispatched: code=%d out=%s", code, out.String())
	}
}

// TestPrivacyRulesEntropyToggleControlsDetection proves that
// privacy.rules.entropy is not a config field with no effect: turning it off
// must stop the entropy-qualified "assigned-credential" rule from firing at
// all, not merely drop its floor (see nonEntropyRules's doc comment for why
// dropping only the floor would be worse than leaving entropy on — it would
// turn the most false-positive-prone rule in the table into an unconditional
// one).
func TestPrivacyRulesEntropyToggleControlsDetection(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const secret = "s3cr3t-hunter2-correct-horse"
	body := []byte(`{"messages":[{"role":"user","content":"DATABASE_PASSWORD=` + secret + `"}]}`)

	cfg := config.Default()
	cfg.Privacy.Enabled = true
	cfg.Privacy.Rules.BuiltinSecrets = true
	cfg.Privacy.Rules.Entropy = true

	withEntropy, err := buildPrivacy(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildPrivacy (entropy on): %v", err)
	}
	redactedOn, _, err := withEntropy.Redact(context.Background(), body)
	if err != nil {
		t.Fatalf("Redact (entropy on): %v", err)
	}
	if strings.Contains(string(redactedOn), secret) {
		t.Errorf("with entropy on, the assigned-credential rule should have redacted the value; got %s", redactedOn)
	}

	cfg.Privacy.Rules.Entropy = false
	withoutEntropy, err := buildPrivacy(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildPrivacy (entropy off): %v", err)
	}
	redactedOff, _, err := withoutEntropy.Redact(context.Background(), body)
	if err != nil {
		t.Fatalf("Redact (entropy off): %v", err)
	}
	if !strings.Contains(string(redactedOff), secret) {
		t.Errorf("with entropy off, the assigned-credential rule must not fire at all; got %s", redactedOff)
	}
}

// The NER detector is registered only when it is both enabled and given at
// least one category, and when it is, Filter.ModelState reports the detector's
// own state instead of "off". Nothing here loads the model: New validates
// config and the session is built lazily on the first scan that could produce a
// finding.
func TestBuildPrivacyWiresTheNERModelState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	log := slog.New(slog.DiscardHandler)

	cfg := config.Default()
	cfg.Privacy.Enabled = true

	off, err := buildPrivacy(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if got := off.ModelState(); got != "off" {
		t.Errorf("ModelState with no NER config = %q, want off", got)
	}

	// Enabled but with no categories is still off: the detector would find
	// nothing and must not claim otherwise.
	cfg.Privacy.NER.Enabled = true
	noLabels, err := buildPrivacy(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if got := noLabels.ModelState(); got != "off" {
		t.Errorf("ModelState with no NER labels = %q, want off", got)
	}

	cfg.Privacy.NER.Labels = []string{"private_person"}
	on, err := buildPrivacy(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if got := on.ModelState(); got == "off" {
		t.Error("ModelState with the NER model configured must not be off")
	}
}

// A typo in privacy.ner.labels must fail the build rather than silently
// disabling protection the operator believes is on.
func TestBuildPrivacyRejectsAnUnknownNERLabel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Privacy.Enabled = true
	cfg.Privacy.NER.Enabled = true
	cfg.Privacy.NER.Labels = []string{"private_persn"}
	if _, err := buildPrivacy(cfg, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("an unknown NER label was accepted")
	}
}
