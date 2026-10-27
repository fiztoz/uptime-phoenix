#!/usr/bin/env bash
# Fixture text is written literally on purpose.
# shellcheck disable=SC2016
# Self-test for check-dockerfile-arch-args.sh: it must reject platform ARG
# defaults and TARGET* build-arg overrides, and accept bare ARGs.
set -euo pipefail
guard="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-dockerfile-arch-args.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

setup() {
  rm -rf "$tmp/repo" && mkdir -p "$tmp/repo/.github/workflows" "$tmp/repo/scripts/release"
  printf 'FROM --platform=$BUILDPLATFORM golang AS b\nARG TARGETOS\nARG TARGETARCH\nRUN GOARCH=${TARGETARCH} go build\n' > "$tmp/repo/Dockerfile"
  printf 'jobs:\n  b:\n    steps:\n      - run: docker buildx build --platform linux/arm64 .\n' > "$tmp/repo/.github/workflows/ci.yml"
}
expect() { # pass|fail NAME
  if bash "$guard" "$tmp/repo" >"$tmp/out" 2>&1; then got=pass; else got=fail; fi
  if [[ "$got" != "$1" ]]; then
    echo "FAIL: $2: expected $1, got $got" >&2; cat "$tmp/out" >&2; exit 1
  fi
  echo "ok: $2 ($1)"
}

setup; expect pass "bare ARGs, --platform only"
setup; sed -i 's/^ARG TARGETARCH$/ARG TARGETARCH=amd64/' "$tmp/repo/Dockerfile"; expect fail "ARG TARGETARCH=amd64 default"
setup; sed -i 's/^ARG TARGETOS$/  ARG TARGETOS=linux/' "$tmp/repo/Dockerfile"; expect fail "indented ARG TARGETOS=linux default"
setup; printf 'FROM x\nARG BUILDPLATFORM=linux/amd64\n' > "$tmp/repo/Dockerfile.probe"; expect fail "BUILDPLATFORM default in Dockerfile.probe"
setup; printf '#!/bin/sh\ndocker buildx build --build-arg "TARGETARCH=${arch}" .\n' > "$tmp/repo/scripts/release/dry-run.sh"; expect fail "--build-arg TARGETARCH override in script"
setup; printf '        with:\n          build-args: |\n            VERSION=ci\n            TARGETARCH=amd64\n' >> "$tmp/repo/.github/workflows/ci.yml"; expect fail "build-push-action build-args TARGETARCH"
echo "test-dockerfile-arch-args: all cases passed"
