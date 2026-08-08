#!/usr/bin/env bash
# Phoenix release dry-run cleanup — remove local artifacts only. Never publish.
#
# Usage:
#   ./scripts/release/clean.sh                    # all dist/release-* + matching local images
#   VERSION=0.0.0-snapshot.1 ./scripts/release/clean.sh
#   ./scripts/release/clean.sh --all-dist         # entire dist/ tree
#   ./scripts/release/clean.sh --images-only      # local uptime-phoenix* dry-run tags only
#   ./scripts/release/clean.sh --no-images        # filesystem only
#   ./scripts/release/clean.sh --dry-run          # print actions, delete nothing
#
# Environment:
#   VERSION     Optional. When set, only that snapshot's dir and image tags are removed.
#               When unset, every dist/release-* directory is removed (and related images).
#   OUT_DIR     Optional explicit artifact directory (overrides VERSION path).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

ALL_DIST=0
IMAGES_ONLY=0
NO_IMAGES=0
DRY=0

usage() {
  sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

for arg in "$@"; do
  case "$arg" in
    -h|--help) usage 0 ;;
    --all-dist) ALL_DIST=1 ;;
    --images-only) IMAGES_ONLY=1 ;;
    --no-images) NO_IMAGES=1 ;;
    --dry-run) DRY=1 ;;
    *)
      echo "unknown flag: $arg" >&2
      usage 2
      ;;
  esac
done

if [[ "$IMAGES_ONLY" -eq 1 && "$NO_IMAGES" -eq 1 ]]; then
  echo "cannot combine --images-only and --no-images" >&2
  exit 2
fi

run() {
  if [[ "$DRY" -eq 1 ]]; then
    echo "    [dry-run] $*"
  else
    "$@"
  fi
}

echo "==> Release artifact cleanup"
[[ "$DRY" -eq 1 ]] && echo "    mode: dry-run (no deletes)"

# ── Filesystem ──────────────────────────────────────────────────────────────
if [[ "$IMAGES_ONLY" -eq 0 ]]; then
  targets=()

  if [[ -n "${OUT_DIR:-}" ]]; then
    targets+=("$OUT_DIR")
  elif [[ "$ALL_DIST" -eq 1 ]]; then
    if [[ -d dist ]]; then
      targets+=("dist")
    fi
  elif [[ -n "${VERSION:-}" ]]; then
    targets+=("dist/release-${VERSION}")
  else
    # Default: every snapshot directory under dist/
    if [[ -d dist ]]; then
      while IFS= read -r -d '' d; do
        targets+=("$d")
      done < <(find dist -mindepth 1 -maxdepth 1 -type d -name 'release-*' -print0 2>/dev/null || true)
      # Drop empty dist/ after removals (handled below)
    fi
  fi

  if [[ ${#targets[@]} -eq 0 ]]; then
    echo "    no filesystem targets (dist clean or nothing matched)"
  else
    echo "==> Removing filesystem artifacts"
    for t in "${targets[@]}"; do
      if [[ -e "$t" ]]; then
        echo "    rm -rf ${t}"
        run rm -rf "$t"
      else
        echo "    skip (missing): ${t}"
      fi
    done
    # Remove empty dist/ if we cleared all release dirs (not when OUT_DIR was custom elsewhere)
    if [[ -d dist ]] && [[ -z "$(find dist -mindepth 1 -maxdepth 1 2>/dev/null | head -1)" ]]; then
      echo "    rmdir dist (empty)"
      run rmdir dist 2>/dev/null || true
    fi
  fi
fi

# ── Local Docker images (tags only — never remote) ──────────────────────────
if [[ "$NO_IMAGES" -eq 0 ]]; then
  if ! command -v docker >/dev/null 2>&1; then
    echo "    docker not available; skip local image cleanup"
  else
    echo "==> Removing local dry-run image tags"
    # Patterns created by dry-run.sh:
    #   uptime-phoenix:<version>-linux-<arch>
    #   uptime-phoenix-api|worker|web:<version>-linux-<arch>
    filter=()
    if [[ -n "${VERSION:-}" ]]; then
      filter=(--filter "reference=uptime-phoenix*:${VERSION}-*")
    else
      # Snapshot-style and any uptime-phoenix* tag with -linux- (dry-run load tags)
      filter=(--filter "reference=uptime-phoenix*:*")
    fi

    # Collect tags carefully; only remove dry-run style tags (portable: no mapfile).
    imgs=()
    while IFS= read -r line; do
      [[ -n "$line" ]] && imgs+=("$line")
    done < <(
      docker images "${filter[@]}" --format '{{.Repository}}:{{.Tag}}' 2>/dev/null \
        | grep -E '^uptime-phoenix(-api|-worker|-web)?:.+-linux-(amd64|arm64)$' \
        || true
    )

    if [[ ${#imgs[@]} -eq 0 ]]; then
      echo "    no matching local uptime-phoenix dry-run image tags"
    else
      for img in "${imgs[@]}"; do
        echo "    docker rmi ${img}"
        run docker rmi "$img" 2>/dev/null || echo "    warn: could not remove ${img}" >&2
      done
    fi
  fi
fi

echo "==> Cleanup complete"
echo "    No remote registries, git tags, or GHCR packages were touched."
