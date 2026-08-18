package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/config"
)

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
