#!/bin/bash
# Invoked by the dispatch-only workflow, outside the measured source checkout.
set -euo pipefail
src=$(realpath "$1")
out=$(realpath -m "$2")
control=$(realpath "$3")
: "${SOURCE_SHA:?}" "${SOURCE_DATE:?}" "${SOURCE_VERSION:?}"
image=ghcr.io/goreleaser/goreleaser-cross@sha256:33b3cc035e333460086a631f87b3b898e86e2cd9971ea0250e012d46d8b0c643
mkdir -p "$out/tooling"
cp "$control/scripts/size-proof-build.sh" "$control/scripts/size-proof-driver.sh" "$out/tooling/"
{
  date -u +%FT%TZ
  uname -a
  printf 'workflow_source=%s\nrun_id=%s\nrun_attempt=%s\n' "$GITHUB_SHA" "$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT"
  printf 'image=%s\nplatform=linux/amd64\nversion=%s\ndate=%s\nsource=%s\n' "$image" "$SOURCE_VERSION" "$SOURCE_DATE" "$SOURCE_SHA"
  (cd "$control" && sha256sum scripts/size-proof-build.sh scripts/size-proof-driver.sh)
} > "$out/driver-provenance.txt"
docker version --format '{{json .}}' > "$out/docker-version.json"
docker pull --platform linux/amd64 "$image" 2>&1 | tee "$out/image-pull.log"
docker image inspect "$image" --format '{"Id":{{json .Id}},"RepoDigests":{{json .RepoDigests}},"Architecture":{{json .Architecture}},"Os":{{json .Os}},"Created":{{json .Created}},"Labels":{{json .Config.Labels}}}' > "$out/image-inspect.json"
[ "$(docker image inspect "$image" --format '{{.Architecture}}/{{.Os}}')" = amd64/linux ]
docker run --rm --platform linux/amd64 --entrypoint /bin/bash \
  -v "$src:/go/src/gortex:ro" -v "$out:/out" \
  -v "$control/scripts/size-proof-build.sh:/size-proof-build.sh:ro" \
  -w /go/src/gortex -e SOURCE_SHA -e SOURCE_DATE -e SOURCE_VERSION \
  -e GOTOOLCHAIN=local -e GOMAXPROCS=3 -e GOMEMLIMIT=8GiB \
  "$image" /size-proof-build.sh 2>&1 | tee "$out/build.log"
