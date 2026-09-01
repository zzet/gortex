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
cleanup() { rm -rf "$(brew --repository)/Library/Taps/$tap_user"; }
trap cleanup EXIT

cleanup
mkdir -p "$tap_dir/Casks"
printf '%s\n' "$rendered" >"$tap_dir/Casks/gortex.rb"

echo "==> loading the rendered cask through brew"
# `brew info --cask` evaluates every stanza, so an unknown one (the #639 bug)
# fails here. Output is kept: a reviewer should see the artifact list change
# when the template changes.
HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ENV_HINTS=1 \
	brew info --cask "$tap_user/tap/gortex"

echo "==> cask loads cleanly"
