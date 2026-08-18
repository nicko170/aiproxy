package main

import (
	"context"
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

	withEntropy, err := buildPrivacy(cfg)
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
	withoutEntropy, err := buildPrivacy(cfg)
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
