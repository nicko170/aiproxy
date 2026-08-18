// Package rules is the deterministic tier of the privacy filter: a table of
// credential patterns, an entropy qualifier, and the allowlist that keeps their
// false-positive rate low enough to be usable.
//
// Deterministic detection beats a model for credentials in both directions.
// Precision, because a key format is a format — there is nothing to infer. And
// recall, because a model's single "secret" class has never seen the key format
// a vendor shipped last week, while a regex for it is one table row.
package rules

import (
	"fmt"
	"regexp"
	"strings"
)

// allowRes are shapes that look high-entropy but are not secrets. Every one of
// them is common in source code and terminal output, and redacting one costs
// the agent real context: a placeholder where a commit SHA belongs makes git
// history unreadable to it.
var allowRes = []*regexp.Regexp{
	// UUID
	regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
	// git SHA-1 and SHA-256 object ids, and bare content digests
	regexp.MustCompile(`(?i)^[0-9a-f]{40}$`),
	regexp.MustCompile(`(?i)^[0-9a-f]{64}$`),
	// Algorithm-prefixed digests, e.g. sha256:e3b0c... `checksum/secret:
	// sha256:...` is a routine Kubernetes annotation used to trigger a pod
	// restart when a ConfigMap or Secret changes; the assigned-credential rule's
	// value class (anything 12+ chars with no comma/quote/space/semicolon) does
	// not exclude the colon, so this shape is otherwise indistinguishable from a
	// real assignment. Redacting it destroys real context for no privacy gain,
	// since a digest is not a secret.
	//
	// The prefix MUST be an enumerated set of algorithm names, never an open
	// character class: hex digits are a subset of [a-z0-9_-], so an open class
	// would also allowlist any hex:hex or word:hex pair of the right lengths —
	// which is exactly the shape of an NTLM LM:NT hash pair (directly usable for
	// pass-the-hash) and of several vendors' id:secret Basic-Auth tokens (e.g.
	// Twilio's ACCOUNT_SID:AUTH_TOKEN). Anchored at both ends so this cannot
	// swallow anything else.
	regexp.MustCompile(`(?i)^(?:md5|sha1|sha224|sha256|sha384|sha512|sha3-256|sha3-512|blake2b|blake2s|blake3|crc32|xxh64):[0-9a-f]{32,128}$`),
	// semver, with optional prerelease and build metadata
	regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`),
	// RFC 2606 / RFC 6761 reserved names, and RFC 5737 documentation addresses
	regexp.MustCompile(`(?i)^([a-z0-9-]+\.)*(example\.(com|net|org)|example|test|invalid|localhost)$`),
	regexp.MustCompile(`^(192\.0\.2|198\.51\.100|203\.0\.113)\.\d{1,3}$`),
	// Obvious stand-ins. Anything that is all one repeated character is in the
	// same family and is covered by the runes check in Allowed.
	regexp.MustCompile(`(?i)^(your[_-]?|my[_-]?|the[_-]?)?(api[_-]?key|secret|token|password|passwd|pwd|credential)s?$`),
	regexp.MustCompile(`(?i)^(changeme|change[_-]?me|placeholder|redacted|removed|hidden|none|null|nil|todo|fixme|dummy|sample|test)$`),
	regexp.MustCompile(`(?i)^<[^>]*>$`),            // <redacted>, <your key here>
	regexp.MustCompile(`(?i)^\$\{?[a-z0-9_]+\}?$`), // $VAR, ${VAR} — a reference, not a value
}

// Allowed reports whether s is a known non-secret and must never produce a
// finding.
func Allowed(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if isRepeatedRune(s) {
		return true // xxxxxxxx, ********, 00000000
	}
	for _, re := range allowRes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// isRepeatedRune reports whether s is one rune repeated, which is how every
// hand-written placeholder in every README looks.
func isRepeatedRune(s string) bool {
	rs := []rune(s)
	if len(rs) < 4 {
		return false
	}
	for _, r := range rs[1:] {
		if r != rs[0] {
			return false
		}
	}
	return true
}

// compileExtraAllow turns operator-supplied entries into matchers. A bare
// string is a literal; /.../ is a regex, matching the denylist's own syntax so
// an operator learns one convention rather than two.
func compileExtraAllow(entries []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, e := range entries {
		if len(e) >= 2 && strings.HasPrefix(e, "/") && strings.HasSuffix(e, "/") {
			re, err := regexp.Compile(e[1 : len(e)-1])
			if err != nil {
				return nil, fmt.Errorf("rules: allow pattern %s: %w", e, err)
			}
			out = append(out, re)
			continue
		}
		out = append(out, regexp.MustCompile(`^`+regexp.QuoteMeta(e)+`$`))
	}
	return out, nil
}
