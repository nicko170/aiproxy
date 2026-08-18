#!/bin/sh
#
# Install the latest aiproxy release.
#
#   curl -fsSL https://raw.githubusercontent.com/nicko170/aiproxy/main/install.sh | sh
#
# Honours two overrides:
#   AIPROXY_VERSION=0.1.0   install that version instead of the latest
#   BINDIR=~/bin            install there instead of the default location
#
# POSIX sh on purpose: piping into `sh` gets whatever /bin/sh is, which on
# Debian and friends is dash. No bashisms, no `pipefail`.
set -eu

REPO="nicko170/aiproxy"

die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl || die "curl is required"
have tar || die "tar is required"

sha256_of() {
	if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
	elif have shasum; then shasum -a 256 "$1" | cut -d' ' -f1
	else die "need sha256sum or shasum to verify the download"
	fi
}

case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported OS $(uname -s); build from source with: go install github.com/$REPO/cmd/aiproxy@latest" ;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m)" ;;
esac

version="${AIPROXY_VERSION:-}"
if [ -z "$version" ]; then
	# Resolve the latest tag from the redirect rather than the API: no token,
	# no 60-per-hour rate limit, no JSON parsing in POSIX sh.
	url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "could not reach github.com to resolve the latest release"
	version="${url##*/tag/}"
	[ "$version" != "$url" ] || die "$REPO has no published releases yet"
fi
version="${version#v}"

asset="aiproxy_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/v${version}"

tmp=$(mktemp -d)
# shellcheck disable=SC2064 # $tmp is expanded now on purpose; it cannot change
trap "rm -rf '$tmp'" EXIT INT TERM

printf 'downloading aiproxy %s (%s/%s)\n' "$version" "$os" "$arch"
# -S dropped on purpose: a 404 here already has a better message below than
# curl's own transport-level complaint.
curl -fsL -o "$tmp/$asset" "$base/$asset" ||
	die "no release asset $asset — check https://github.com/$REPO/releases"
curl -fsL -o "$tmp/checksums.txt" "$base/checksums.txt" ||
	die "release $version has no checksums.txt; refusing to install unverified"

expected=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || die "$asset is not listed in checksums.txt"
actual=$(sha256_of "$tmp/$asset")
[ "$expected" = "$actual" ] ||
	die "checksum mismatch for $asset
  expected $expected
  got      $actual"

tar -xzf "$tmp/$asset" -C "$tmp" aiproxy || die "could not unpack $asset"

# Prefer /usr/local/bin, but never silently escalate: an unwritable system
# path falls back to the user's own bin rather than reaching for sudo.
if [ -n "${BINDIR:-}" ]; then
	bindir="$BINDIR"
elif [ -w /usr/local/bin ]; then
	bindir=/usr/local/bin
else
	bindir="$HOME/.local/bin"
fi
mkdir -p "$bindir" || die "could not create $bindir"

# Install by rename, so a running aiproxy keeps its open binary and nothing
# ever observes a half-written file at the destination path.
mv "$tmp/aiproxy" "$bindir/aiproxy.new" || die "could not write to $bindir"
chmod 755 "$bindir/aiproxy.new"
mv "$bindir/aiproxy.new" "$bindir/aiproxy"

printf 'installed aiproxy %s to %s/aiproxy\n' "$version" "$bindir"

# shellcheck disable=SC2016 # the $PATH below is literal advice to paste, not an expansion
case ":$PATH:" in
	*":$bindir:"*) printf 'run: aiproxy\n' ;;
	*) printf '\n%s is not on your PATH. Add it:\n  export PATH="%s:$PATH"\n' "$bindir" "$bindir" ;;
esac
