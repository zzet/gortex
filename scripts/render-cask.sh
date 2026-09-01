#!/usr/bin/env bash
# Render .github/homebrew/gortex.rb.tmpl into a concrete Homebrew cask on
# stdout. The template lives in its own file (rather than a heredoc inside
# release.yml) so it can be reviewed as Ruby and, crucially, parsed by a real
# `brew` before a release pushes it to zzet/homebrew-tap — see
# scripts/validate-cask.sh.
#
# Usage:
#   render-cask.sh <version> <sha_darwin_amd64> <sha_darwin_arm64> \
#                  <sha_linux_amd64> <sha_linux_arm64>
#
# `version` is the bare version with no leading "v" (e.g. 0.63.8).
set -euo pipefail

if [ "$#" -ne 5 ]; then
	echo "usage: $0 <version> <sha_darwin_amd64> <sha_darwin_arm64> <sha_linux_amd64> <sha_linux_arm64>" >&2
	exit 2
fi

version="$1"
sha_darwin_amd64="$2"
sha_darwin_arm64="$3"
sha_linux_amd64="$4"
sha_linux_arm64="$5"

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
tmpl="$repo_root/.github/homebrew/gortex.rb.tmpl"

[ -f "$tmpl" ] || {
	echo "FATAL: cask template not found at $tmpl" >&2
	exit 1
}

case "$version" in
"")
	echo "FATAL: version is empty" >&2
	exit 1
	;;
v*)
	echo "FATAL: version must not carry a leading 'v': '$version'" >&2
	exit 1
	;;
esac

# Every sha must be a full sha256. An empty or truncated value would publish a
# cask whose downloads all fail a checksum check, and the error the user sees
# ("SHA256 mismatch") points nowhere near the cause.
for pair in "darwin_amd64=$sha_darwin_amd64" "darwin_arm64=$sha_darwin_arm64" \
	"linux_amd64=$sha_linux_amd64" "linux_arm64=$sha_linux_arm64"; do
	sha="${pair#*=}"
	if ! printf '%s' "$sha" | grep -Eq '^[0-9a-f]{64}$'; then
		echo "FATAL: sha256 for ${pair%%=*} is not 64 lowercase hex chars: '$sha'" >&2
		exit 1
	fi
done

# The template also contains `#{version}` — Ruby interpolation brew evaluates
# at install time. It must survive verbatim, which is why substitution is
# keyed on @@…@@ placeholders rather than shell expansion.
rendered="$(sed \
	-e "s|@@VERSION@@|$version|g" \
	-e "s|@@SHA_DARWIN_AMD64@@|$sha_darwin_amd64|g" \
	-e "s|@@SHA_DARWIN_ARM64@@|$sha_darwin_arm64|g" \
	-e "s|@@SHA_LINUX_AMD64@@|$sha_linux_amd64|g" \
	-e "s|@@SHA_LINUX_ARM64@@|$sha_linux_arm64|g" \
	"$tmpl")"

# A placeholder that survived rendering means the template grew a field the
# renderer doesn't know about — publishing that would ship a literal @@…@@
# into the tap.
if leftover="$(printf '%s\n' "$rendered" | grep -n '@@[A-Z0-9_]*@@')"; then
	echo "FATAL: unsubstituted placeholder left in the rendered cask:" >&2
	printf '%s\n' "$leftover" >&2
	exit 1
fi

printf '%s\n' "$rendered"
