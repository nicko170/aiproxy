// Package updater checks for newer aiproxy releases and replaces the running
// binary with one.
//
// It is deliberately a leaf: it imports the standard library and nothing else
// in this module, which is what lets the whole download-verify-swap path be
// tested end to end against an httptest.Server with no proxy running. The
// packages above it (internal/view for the presentation seam, cmd/aiproxy for
// the CLI) depend on it; it depends on none of them.
package updater

import (
	"strconv"
	"strings"
)

// devVersion is what main.version holds in a build that was not stamped by
// the release workflow. Such a build has no defensible comparison point, so
// it is never offered an update (see ErrDevBuild).
const devVersion = "dev"

// version is a parsed major.minor.patch with an optional prerelease suffix.
// ok is false for anything this project does not produce.
type version struct {
	major, minor, patch int
	pre                 string
	ok                  bool
}

// parseVersion reads "0.2.0", "v0.2.0", or "1.0.0-rc1". Build metadata
// ("+sha") is not accepted: the release workflow never produces it, and
// treating an unknown shape as unparseable is safer than guessing at an
// ordering for it.
func parseVersion(s string) version {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return version{}
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return version{}
		}
		nums[i] = n
	}
	return version{major: nums[0], minor: nums[1], patch: nums[2], pre: pre, ok: true}
}

// Compare orders two aiproxy versions: negative when a sorts below b, zero
// when they are equal, positive when a sorts above.
//
// This is a bounded comparator for the versions this project's release
// workflow produces, not a general semver implementation, and it exists so
// that adding update checking takes no new dependency. Two rules are
// load-bearing: numeric fields compare numerically (so 0.10.0 > 0.9.0, which
// a string comparison gets backwards), and a prerelease sorts BELOW its
// release (1.0.0-rc1 < 1.0.0). Anything unparseable — "dev" above all —
// sorts below anything parseable, so an unstamped build is never told it is
// ahead of a real release.
func Compare(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	switch {
	case !pa.ok && !pb.ok:
		return 0
	case !pa.ok:
		return -1
	case !pb.ok:
		return 1
	}
	for _, d := range [...]int{pa.major - pb.major, pa.minor - pb.minor, pa.patch - pb.patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0
	case pa.pre == "":
		return 1
	case pb.pre == "":
		return -1
	}
	return sign(strings.Compare(pa.pre, pb.pre))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
