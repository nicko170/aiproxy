package rules

import (
	"context"
	"regexp"

	"github.com/nicko170/aiproxy/internal/privacy"
)

// Rule is one credential pattern.
//
// Group selects which capture group is the sensitive span: 0 is the whole
// match. That is how an assignment rule redacts the value and leaves the key
// name readable, so the agent can still tell a DATABASE_PASSWORD from an
// API_KEY.
//
// MinEntropy, when non-zero, requires the captured span to clear that many bits
// per character before a finding is produced. It is what keeps the generic
// assignment rule from firing on `password = "changeme"`.
type Rule struct {
	Name       string
	Label      privacy.Label
	Re         *regexp.Regexp
	Group      int
	MinEntropy float64
}

// Builtin is the shipped rule table. Adding a credential format is adding a
// row, which is the whole reason this is data rather than code.
//
// Patterns are anchored on the credential's own fixed prefix wherever the
// vendor provides one, because a prefix is worth more than any amount of
// entropy heuristics: it is precise by construction.
func Builtin() []Rule {
	return []Rule{
		{
			Name: "aws-access-key-id", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16})\b`), Group: 1,
		},
		{
			Name: "github-token", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b((?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{82})\b`), Group: 1,
		},
		{
			Name: "anthropic-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_-]{20,})`), Group: 1,
		},
		{
			Name: "openai-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(sk-(?:proj-)?[A-Za-z0-9_-]{20,})`), Group: 1,
		},
		{
			Name: "google-api-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{35})\b`), Group: 1,
		},
		{
			Name: "slack-token", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(xox[baprs]-[A-Za-z0-9-]{10,})`), Group: 1,
		},
		{
			Name: "stripe-key", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b((?:sk|rk)_live_[A-Za-z0-9]{16,})\b`), Group: 1,
		},
		{
			Name: "jwt", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`\b(eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`), Group: 1,
		},
		{
			// (?s) so the block spans newlines; the lazy body stops at the first
			// END line rather than swallowing everything to the last one.
			Name: "private-key-block", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`(?s)(-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----)`), Group: 1,
		},
		{
			// Credentials in a URL: the password only, so the scheme, user, and
			// host stay legible.
			Name: "url-credentials", Label: privacy.LabelSecret,
			Re: regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s/:@]+:([^\s/:@]+)@`), Group: 1,
		},
		{
			// The generic catch-all, and the only rule that leans on entropy.
			// Without the entropy floor it fires on every `password = "changeme"`
			// in every example config in every repository.
			Name: "assigned-credential", Label: privacy.LabelSecret,
			// The leading alternation is (start-of-string | non-alphanumeric)
			// rather than \b: \b treats '_' as a word character, so it would never
			// match the "password" in DATABASE_PASSWORD, which is how most of
			// these actually appear.
			Re:    regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:api[_-]?key|secret|token|password|passwd|pwd|credential)s?\b\s*[:=]\s*["']?([^\s"',;]{12,})`),
			Group: 1, MinEntropy: 3.0,
		},
	}
}

// Detector scans with a rule table, suppressing anything the allowlist claims.
type Detector struct {
	rules []Rule
	extra []*regexp.Regexp
}

// New builds a Detector. extraAllow entries are operator-supplied allowlist
// additions — a literal, or /regex/ — and an invalid one is reported here
// rather than being discovered mid-request.
func New(rules []Rule, extraAllow []string) (*Detector, error) {
	extra, err := compileExtraAllow(extraAllow)
	if err != nil {
		return nil, err
	}
	return &Detector{rules: rules, extra: extra}, nil
}

func (d *Detector) Name() string { return "rules" }

// Scan applies every rule and returns unresolved findings; the pipeline's
// Resolve handles overlaps between them.
//
// Safe for concurrent use: regexp.Regexp is, and nothing here is mutated after
// New.
func (d *Detector) Scan(_ context.Context, text string) ([]privacy.Finding, error) {
	if len(text) < privacy.MinScanBytes {
		return nil, nil
	}
	var out []privacy.Finding
	for _, r := range d.rules {
		for _, m := range r.Re.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[0], m[1]
			if r.Group > 0 && len(m) > 2*r.Group+1 {
				start, end = m[2*r.Group], m[2*r.Group+1]
			}
			if start < 0 || end <= start {
				continue // the group did not participate in the match
			}
			span := text[start:end]
			if d.allowed(span) {
				continue
			}
			if r.MinEntropy > 0 && ShannonBits(span) < r.MinEntropy {
				continue
			}
			out = append(out, privacy.Finding{
				Start: start, End: end, Label: r.Label, Rule: r.Name, Confidence: 1.0,
			})
		}
	}
	return out, nil
}

func (d *Detector) allowed(span string) bool {
	if Allowed(span) {
		return true
	}
	for _, re := range d.extra {
		if re.MatchString(span) {
			return true
		}
	}
	return false
}
