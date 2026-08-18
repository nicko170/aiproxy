package rules

import (
	"context"
	"strings"
	"testing"

	"github.com/nicko170/aiproxy/internal/privacy"
)

func scan(t *testing.T, text string) []privacy.Finding {
	t.Helper()
	d, err := New(Builtin(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return got
}

// found reports whether any finding covers exactly want.
func found(text string, findings []privacy.Finding, want string) bool {
	for _, f := range findings {
		if text[f.Start:f.End] == want {
			return true
		}
	}
	return false
}

func TestRulesDetectCommonCredentials(t *testing.T) {
	cases := []struct{ name, text, want string }{
		{"aws access key", `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`, "AKIAIOSFODNN7EXAMPLE"},
		{"github pat", `token: ghp_1234567890abcdefghijklmnopqrstuvwx`, "ghp_1234567890abcdefghijklmnopqrstuvwx"},
		{"anthropic key", `sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789`, "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
		{"google api key", `key=AIzaSyA1234567890abcdefghijklmnopqrstuv`, "AIzaSyA1234567890abcdefghijklmnopqrstuv"},
		{"slack token", `xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx`, "xoxb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx"},
		{"stripe key", `sk_live_1234567890abcdefghijklmn`, "sk_live_1234567890abcdefghijklmn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scan(t, c.text)
			if !found(c.text, got, c.want) {
				t.Errorf("did not find %q in %q; findings = %+v", c.want, c.text, got)
			}
		})
	}
}

// The whole PEM block is one finding: redacting only the header would leave the
// key material behind, which is the opposite of the point.
func TestRulesRedactAWholePrivateKeyBlock(t *testing.T) {
	text := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nqqqq\n-----END RSA PRIVATE KEY-----\nafter"
	got := scan(t, text)
	if len(got) == 0 {
		t.Fatal("no finding for a PEM private key block")
	}
	span := text[got[0].Start:got[0].End]
	if !strings.HasPrefix(span, "-----BEGIN") || !strings.HasSuffix(span, "-----") {
		t.Errorf("span is not the whole block: %q", span)
	}
	if strings.Contains(span, "before") || strings.Contains(span, "after") {
		t.Errorf("span swallowed surrounding text: %q", span)
	}
}

// Only the value is redacted, so the agent can still see WHICH setting it is.
func TestRulesRedactOnlyTheValueOfAnAssignment(t *testing.T) {
	text := `DATABASE_PASSWORD=s3cr3t-hunter2-correct-horse`
	got := scan(t, text)
	if len(got) == 0 {
		t.Fatal("no finding")
	}
	span := text[got[0].Start:got[0].End]
	if strings.Contains(span, "DATABASE_PASSWORD") {
		t.Errorf("the key name was redacted too: %q", span)
	}
	if !strings.Contains(span, "s3cr3t") {
		t.Errorf("the value was not redacted: %q", span)
	}
}

func TestRulesDetectCredentialsInAURL(t *testing.T) {
	text := `postgres://admin:sup3rs3cret@db.internal:5432/app`
	got := scan(t, text)
	if len(got) == 0 {
		t.Fatal("no finding for a connection string with a password")
	}
}

// False positives are the failure mode that makes the filter unusable: a
// redacted commit SHA breaks the agent's reasoning about history for no gain.
func TestRulesDoNotFireOnBenignHighEntropyStrings(t *testing.T) {
	for _, text := range []string{
		"commit 9edb09c1f4a3b2c5d6e7f8091a2b3c4d5e6f7a8b",
		"digest sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"id 550e8400-e29b-41d4-a716-446655440000",
		"version 1.26.5-rc1+build.42",
		"see https://docs.example.com/api/v1/reference",
		`API_KEY = "YOUR_API_KEY"`,
		`password = "changeme"`,
		`token: "<redacted>"`,
		// checksum/secret: sha256:... is the standard Kubernetes annotation for
		// triggering a pod restart when a ConfigMap or Secret changes. The
		// assigned-credential rule's value class does not exclude ':', so an
		// algorithm-prefixed digest assigned to a credential-shaped key must be
		// caught by the allowlist instead.
		`checksum/secret: sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		`SECRET=sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`,
		// Exercise the digest-length range at both ends, not only at sha256's 64.
		`SECRET=md5:9dd4e461268c8034f5c8564e155c67a6`,
		`SECRET=sha1:11f6ad8ec52a2984abaafd7c3b516503785c2072`,
	} {
		if got := scan(t, text); len(got) != 0 {
			t.Errorf("false positive on %q: %+v", text, got)
		}
	}
}

func TestAllowedRecognisesBenignShapes(t *testing.T) {
	for _, s := range []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"9edb09c1f4a3b2c5d6e7f8091a2b3c4d5e6f7a8b",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"YOUR_API_KEY", "changeme", "xxxxxxxxxxxx", "<redacted>",
		"example.com", "1.26.5",
	} {
		if !Allowed(s) {
			t.Errorf("Allowed(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_1234567890abcdefghijklmnopqrstuvwx"} {
		if Allowed(s) {
			t.Errorf("Allowed(%q) = true; a real credential must not be allowlisted", s)
		}
	}
}

// An operator-supplied allow entry suppresses a rule that would otherwise fire.
func TestExtraAllowSuppressesAFinding(t *testing.T) {
	const text = `AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`
	if got := scan(t, text); len(got) == 0 {
		t.Fatal("precondition: the rule should fire without an allow entry")
	}
	d, err := New(Builtin(), []string{"AKIAIOSFODNN7EXAMPLE"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Scan(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("extra allow entry did not suppress the finding: %+v", got)
	}
}

func TestNewRejectsABadExtraAllowPattern(t *testing.T) {
	if _, err := New(Builtin(), []string{"/(unclosed/"}); err == nil {
		t.Fatal("New accepted an invalid regex; it must be reported at construction, not at scan time")
	}
}

func TestDetectorNameIsStable(t *testing.T) {
	d, _ := New(Builtin(), nil)
	if d.Name() != "rules" {
		t.Errorf("Name = %q, want rules — it appears in the Activity feed and in cache keys", d.Name())
	}
}
