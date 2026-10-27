#!/usr/bin/env bash
# Static guard (no Docker needed): fail when a Dockerfile gives BuildKit's
# automatic platform ARGs a default, or a build call site overrides them.
#
# `ARG TARGETARCH=amd64` inside a stage replaces the value buildx derives from
# --platform, so every platform gets the same binary. That shipped x86-64
# binaries in the linux/arm64 images of v0.5.1 and v0.6.0. Passing
# --build-arg TARGETARCH=... hides the bug in one-arch builds (the release
# dry-run did exactly that), so call sites must rely on --platform only.
#
# Usage: check-dockerfile-arch-args.sh [REPO_ROOT]
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
cd "$root"
# The guard and its self-test quote the forbidden patterns on purpose.
skip=" scripts/release/check-dockerfile-arch-args.sh scripts/release/test-dockerfile-arch-args.sh "
names='(TARGET|BUILD)(PLATFORM|OS|ARCH|VARIANT)'
fail=0
indent() { while IFS= read -r line; do echo "  ${line}" >&2; done <<<"$1"; }

mapfile -t dockerfiles < <(find . -path ./web/node_modules -prune -o -path ./.git -prune -o \
  -type f \( -name 'Dockerfile' -o -name 'Dockerfile.*' -o -name '*.Dockerfile' \) -print | sort)
if ((${#dockerfiles[@]} == 0)); then
  echo "check-dockerfile-arch-args: no Dockerfiles found under ${root}" >&2
  exit 1
fi
for f in "${dockerfiles[@]}"; do
  if hits="$(grep -nE "^[[:space:]]*ARG[[:space:]]+${names}[[:space:]]*=" "$f")"; then
    echo "ERROR: ${f#./} gives a BuildKit platform ARG a default (it overrides --platform):" >&2
    indent "$hits"
    fail=1
  fi
done

shopt -s nullglob
callsites=()
for f in .github/workflows/*.yml .github/workflows/*.yaml Makefile scripts/release/*.sh; do
  [[ -f "$f" && "$skip" != *" $f "* ]] && callsites+=("$f")
done
shopt -u nullglob
for f in "${callsites[@]}"; do
  # --build-arg TARGETARCH=..., --build-arg "TARGETOS=..." and build-push-action
  # `build-args:` list entries (indented TARGETARCH=... lines).
  if hits="$(grep -nE -- "--build-arg[= ]+\"?${names}=|^[[:space:]]+${names}=" "$f")"; then
    echo "ERROR: ${f} overrides a BuildKit platform ARG; use --platform instead:" >&2
    indent "$hits"
    fail=1
  fi
done

if ((fail)); then
  exit 1
fi
echo "check-dockerfile-arch-args: OK (${#dockerfiles[@]} Dockerfiles, ${#callsites[@]} build call sites)"
