#!/usr/bin/env bash
# Prove the generated Homebrew cask actually loads before a release pushes it
# to zzet/homebrew-tap.
#
# This exists because issue #639 shipped a cask that was valid Ruby but used
# `pre_install` / `post_install` — formula-only DSL methods that the cask DSL
# rejects at load time. Nothing in the Go test suite or a YAML lint could see
# it, and the tap has no CI of its own, so every macOS user got
#
#   Error: Cask 'gortex' definition is invalid: undefined method 'pre_install'
#
# on install AND upgrade. Only a real `brew` evaluating the DSL catches that
# class of bug, which is why this renders the template and loads it through
# brew rather than checking it with a linter.
#
# macOS only: Homebrew on Linux cannot load casks at all.
#
# Usage: validate-cask.sh   (no arguments — renders with dummy values)
set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
	echo "SKIP: casks can only be loaded by Homebrew on macOS (got $(uname -s))"
	exit 0
fi

command -v brew >/dev/null 2>&1 || {
	echo "FATAL: brew not on PATH; cannot validate the cask" >&2
	exit 1
}

repo_root="$(cd "$(dirname "$0")/.." && pwd)"

# Dummy but well-formed inputs. The DSL is evaluated, not downloaded, so the
# shas only have to satisfy render-cask.sh's shape check.
DUMMY_SHA="$(printf '0%.0s' $(seq 1 64))"
rendered="$("$repo_root/scripts/render-cask.sh" 0.0.0 \
	"$DUMMY_SHA" "$DUMMY_SHA" "$DUMMY_SHA" "$DUMMY_SHA")"

# Load the cask the way a user's machine will: from a tap on disk. A scratch
# tap keeps the real zzet/tap (which may be installed on a dev machine)
# untouched, and the trap removes it even when brew fails.
tap_user="gortexcaskcheck"
tap_dir="$(brew --repository)/Library/Taps/$tap_user/homebrew-tap"
trust_home=""
cleanup() {
	rm -rf "$(brew --repository)/Library/Taps/$tap_user"
	if [ -n "$trust_home" ]; then rm -rf "$trust_home"; fi
}
trap cleanup EXIT

cleanup
mkdir -p "$tap_dir/Casks"
printf '%s\n' "$rendered" >"$tap_dir/Casks/gortex.rb"

# Current Homebrew refuses to load casks from a tap nobody has trusted, and
# `brew info` does not trust it on the way like `brew install user/tap/x`
# does. Trust the scratch tap in a throwaway trust store (brew keeps it under
# $XDG_CONFIG_HOME/homebrew when that is set), so the developer's own trust
# store and brew.env are neither read nor changed. HOMEBREW_NO_REQUIRE_TAP_TRUST
# is deprecated, and its warning would trip the check below, so it is unset
# too in case a developer exported it to get past this same error. Older brew
# has no `trust` command and needs none.
trust_home="$(mktemp -d)"
export XDG_CONFIG_HOME="$trust_home"
unset HOMEBREW_NO_REQUIRE_TAP_TRUST HOMEBREW_REQUIRE_TAP_TRUST
if brew trust --help >/dev/null 2>&1; then
	brew trust --tap "$tap_user/tap"
fi

echo "==> loading the rendered cask through brew"
# `brew info --cask` evaluates every stanza, so an unknown one (the #639 bug)
# fails here. Output is kept: a reviewer should see the artifact list change
# when the template changes.
log="$(mktemp)"
trap 'cleanup; rm -f "$log"' EXIT
HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ENV_HINTS=1 \
	brew info --cask "$tap_user/tap/gortex" 2>&1 | tee "$log"

# A deprecated stanza still loads, so the check above passes, but Homebrew
# warns every user who installs or upgrades and later turns the deprecation
# into a load error, which is #639 again on a delay. The legacy `postflight`
# block went this way. Fail on the warning instead of waiting for the error.
if grep -qi "deprecated" "$log"; then
	echo "FATAL: the rendered cask uses a stanza Homebrew has deprecated (see above)" >&2
	exit 1
fi

echo "==> cask loads cleanly"
