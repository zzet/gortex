#!/bin/bash
# Run INSIDE one recorded goreleaser-cross image digest, with src read-only
# at /go/src/gortex and owned empty output at /out. Source is an owned clean Git checkout at exact SHA.
set -euo pipefail
: "${SOURCE_SHA:?}" "${SOURCE_DATE:?}" "${SOURCE_VERSION:?}"
[ "$(go env GOVERSION)" = go1.27.3 ]
cd /go/src/gortex
command -v python3 readelf git >/dev/null
go version > /out/go-version.txt
go env -json > /out/go-env.json
sha256sum .goreleaser.yml > /out/goreleaser-config.sha256
export GOTOOLCHAIN=local GIT_OPTIONAL_LOCKS=0
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0=/go/src/gortex
[ "$(git rev-parse HEAD)" = "$SOURCE_SHA" ]
[ -z "$(git status --porcelain)" ]
git show -s --format='%H %T %cI' HEAD > /out/source.txt
export GIT_OPTIONAL_LOCKS=0
for arch in amd64 arm64; do
  case "$arch" in
    amd64) triple=x86_64-linux-gnu ;;
    arm64) triple=aarch64-linux-gnu ;;
  esac
  export CGO_ENABLED=1 GOOS=linux GOARCH="$arch" CC="$triple-gcc" CXX="$triple-g++"
  export CGO_CFLAGS='-O2 -g -fno-strict-aliasing' CGO_CXXFLAGS='-O2 -g -fno-strict-aliasing'
  "$CC" --version > "/out/cc-$arch.txt"
  "$CXX" --version > "/out/cxx-$arch.txt"
  # Published baseline buildinfo has no trimpath and retains VCS metadata.
  go build -p=2 -tags netgo,osusergo,static_link \
    -ldflags "-s -w -X main.version=$SOURCE_VERSION -X main.commit=${SOURCE_SHA:0:8} -X main.date=$SOURCE_DATE -extldflags \"-static\"" \
    -o "/out/gortex-linux-$arch" ./cmd/gortex
  sh scripts/verify-static-elf.sh "/out/gortex-linux-$arch"
  go version -m "/out/gortex-linux-$arch" > "/out/buildinfo-$arch.txt"
  python3 - "/out/buildinfo-$arch.txt" "$arch" "$SOURCE_SHA" "$SOURCE_VERSION" "$SOURCE_DATE" <<'PYMETA'
import json, sys
path, arch, sha, version, date = sys.argv[1:]
def require(condition, message):
    if not condition:
        raise ValueError(message)
lines = open(path).read().splitlines()
require(lines[0].endswith(': go1.27.3'), lines[0])
settings = {}
for line in lines[1:]:
    parts = line.strip().split('\t')
    if len(parts) == 2 and parts[0] == 'build' and '=' in parts[1]:
        k, v = parts[1].split('=', 1)
        settings[k] = json.loads(v) if v.startswith('"') else v
expected = {'GOOS': 'linux', 'GOARCH': arch, 'CGO_ENABLED': '1',
            'CGO_CPPFLAGS': '', 'CGO_LDFLAGS': '',
            'CGO_CFLAGS': '-O2 -g -fno-strict-aliasing',
            'CGO_CXXFLAGS': '-O2 -g -fno-strict-aliasing',
            '-tags': 'netgo,osusergo,static_link',
            'vcs': 'git', 'vcs.revision': sha, 'vcs.modified': 'false'}
for key, value in expected.items():
    require(settings.get(key) == value, (key, settings.get(key), value))
require('-trimpath' not in settings, settings.get('-trimpath'))
require(settings.get('vcs.time'), 'missing exact source commit time')
expected_ldflags = f'-s -w -X main.version={version} -X main.commit={sha[:8]} -X main.date={date} -extldflags \"-static\"'
require(settings.get('-ldflags') == expected_ldflags, settings.get('-ldflags'))
print('release-shape metadata checks passed', arch, sha)
PYMETA
  sha256sum "/out/gortex-linux-$arch" > "/out/sha256-$arch.txt"
  stat -c '%s' "/out/gortex-linux-$arch" > "/out/bytes-$arch.txt"
done
