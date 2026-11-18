#!/bin/sh
# Keep the tree-sitter aliasing workaround in every controlled gcc build (#821).
# Match assignments, so a comment or this guard cannot hide a removed flag.
set -eu

check_flags() {
	file="$1"
	prefix="$2"
	separator="$3"
	for variable in CGO_CFLAGS CGO_CXXFLAGS; do
		if ! grep -Eq "^${prefix}${variable}${separator}-O2 -g -fno-strict-aliasing[[:space:]]*$" "$file"; then
			echo "FATAL: $file must set $variable to -O2 -g -fno-strict-aliasing" >&2
			exit 1
		fi
	done
}

check_flags .goreleaser.yml '      - ' '='
check_flags .github/workflows/release.yml '          ' ': '
check_flags .github/workflows/ci.yml '  ' ': '
check_flags Makefile 'export ' ' [ ?]*= '

echo 'ok: C and C++ aliasing flags are present in all build configurations'
