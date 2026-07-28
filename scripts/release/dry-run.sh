#!/usr/bin/env bash
# Phoenix release dry-run — produce local artifacts only. Never publish.
#
# Usage:
#   VERSION=0.0.0-snapshot.1 ./scripts/release/dry-run.sh
#   VERSION=0.0.0-snapshot.1 SKIP_DOCKER=1 ./scripts/release/dry-run.sh
#
# Environment:
#   VERSION       Required snapshot version stamped into binaries/images/chart
#   OUT_DIR       Artifact directory (default: dist/release-${VERSION})
#   SKIP_DOCKER   Set to 1 to skip image builds (binaries + helm only)
#   SKIP_SBOM     Set to 1 to skip Syft SBOM generation
#   PLATFORMS     Override binary matrix (default: full matrix below)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:-}"
if [[ -z "$VERSION" ]]; then
  echo "VERSION is required (e.g. VERSION=0.0.0-snapshot.1)" >&2
  exit 2
fi

OUT_DIR="${OUT_DIR:-dist/release-${VERSION}}"
BIN_DIR="${OUT_DIR}/binaries"
IMG_DIR="${OUT_DIR}/images"
CHART_DIR="${OUT_DIR}/charts"
SBOM_DIR="${OUT_DIR}/sbom"
mkdir -p "$BIN_DIR" "$IMG_DIR" "$CHART_DIR" "$SBOM_DIR"

LDFLAGS="-s -w -X github.com/fiztoz/uptime-phoenix/internal/version.Version=${VERSION}"
export CGO_ENABLED=0

echo "==> Release dry-run VERSION=${VERSION}"
echo "    output: ${OUT_DIR}"

# ── Ensure web/dist exists for //go:embed and Docker USE_PREBUILT_WEB=1 ──────
if [[ ! -f web/dist/index.html ]]; then
  echo "==> web/dist missing — building frontend (or writing placeholder)"
  if command -v bun >/dev/null 2>&1 && [[ -f web/package.json ]]; then
    (cd web && bun install --frozen-lockfile && bun run build)
  else
    mkdir -p web/dist
    printf '%s\n' '<!doctype html><title>phoenix</title>' > web/dist/index.html
    echo "    wrote placeholder web/dist/index.html (bun unavailable)"
  fi
else
  echo "==> web/dist present (host-built frontend for embed + Docker prebuilt)"
fi

# ── Binary matrix (only combinations that actually cross-compile) ───────────
# Default matrix: linux/darwin/windows × amd64/arm64 for cmd/app + helpers.
DEFAULT_PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

if [[ -n "${PLATFORMS:-}" ]]; then
  # shellcheck disable=SC2206
  MATRIX=(${PLATFORMS})
else
  MATRIX=("${DEFAULT_PLATFORMS[@]}")
fi

built_bins=()
failed_bins=()

build_one() {
  local goos="$1" goarch="$2" pkg="$3" name="$4"
  local ext=""
  if [[ "$goos" == "windows" ]]; then
    ext=".exe"
  fi
  local out="${BIN_DIR}/${name}_${VERSION}_${goos}_${goarch}${ext}"
  echo "    build ${name} ${goos}/${goarch}"
  if GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags="$LDFLAGS" -o "$out" "$pkg"; then
    built_bins+=("$out")
    # Prove architecture where file(1) is available.
    if command -v file >/dev/null 2>&1; then
      file "$out" | tee -a "${OUT_DIR}/binary-arch.txt" >/dev/null || true
    fi
  else
    echo "    WARN: cross-compile failed for ${name} ${goos}/${goarch}" >&2
    failed_bins+=("${name}_${goos}_${goarch}")
    rm -f "$out"
  fi
}

echo "==> Cross-compiling CGO-free binaries"
for plat in "${MATRIX[@]}"; do
  goos="${plat%/*}"
  goarch="${plat#*/}"
  build_one "$goos" "$goarch" ./cmd/app "phoenix"
  build_one "$goos" "$goarch" ./cmd/kuma-import "kuma-import"
  build_one "$goos" "$goarch" ./cmd/phoenix-config "phoenix-config"
  build_one "$goos" "$goarch" ./cmd/api "phoenix-api"
  build_one "$goos" "$goarch" ./cmd/worker "phoenix-worker"
done

echo "==> Checksums"
(
  cd "$BIN_DIR"
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 ./* > SHA256SUMS 2>/dev/null || true
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./* > SHA256SUMS 2>/dev/null || true
  else
    echo "no sha256 tool available" > SHA256SUMS
  fi
)

# ── SBOM for binaries ───────────────────────────────────────────────────────
if [[ "${SKIP_SBOM:-0}" != "1" ]]; then
  echo "==> SBOM (syft) for binaries"
  if command -v syft >/dev/null 2>&1; then
    for bin in "${built_bins[@]}"; do
      base="$(basename "$bin")"
      syft "$bin" -o spdx-json > "${SBOM_DIR}/${base}.spdx.json" || \
        echo "syft failed for $bin" >&2
    done
  else
    echo "    syft not installed; skipping local binary SBOMs (CI installs it)"
  fi
fi

# ── Helm chart package ──────────────────────────────────────────────────────
echo "==> Helm chart package"
# Stamp appVersion for this snapshot without permanently editing the chart in
# git when run from a dirty tree: work on a copy.
CHART_SRC="${OUT_DIR}/chart-src"
rm -rf "$CHART_SRC"
cp -R charts/phoenix "$CHART_SRC"
# Portable in-place appVersion stamp.
python3 - "$CHART_SRC/Chart.yaml" "$VERSION" <<'PY'
import pathlib, sys, re
path = pathlib.Path(sys.argv[1])
version = sys.argv[2]
text = path.read_text(encoding="utf-8")
text2, n = re.subn(r'(?m)^appVersion:\s*.*$', f'appVersion: "{version}"', text, count=1)
if n != 1:
    raise SystemExit(f"failed to stamp appVersion in {path}")
path.write_text(text2, encoding="utf-8")
print(f"stamped appVersion={version}")
PY

if command -v helm >/dev/null 2>&1; then
  helm lint "$CHART_SRC"
  helm package "$CHART_SRC" --destination "$CHART_DIR"
  helm template phoenix "$CHART_SRC" > "${OUT_DIR}/helm-template.yaml"
else
  echo "    helm not installed; skipped package/lint/template" >&2
fi

# ── Multi-arch images (load/export only — never push) ───────────────────────
if [[ "${SKIP_DOCKER:-0}" != "1" ]]; then
  echo "==> Docker multi-arch builds (no push)"
  if ! command -v docker >/dev/null 2>&1; then
    echo "    docker not available; skip images" >&2
  else
    # All-in-one image for linux/amd64 and linux/arm64 separately so we can
    # inspect the binary architecture of each, then optionally combine.
    for arch in amd64 arm64; do
      tag="phoenix:${VERSION}-linux-${arch}"
      echo "    buildx build linux/${arch} -> ${tag}"
      docker buildx build \
        --platform "linux/${arch}" \
        --build-arg "VERSION=${VERSION}" \
        --build-arg "TARGETOS=linux" \
        --build-arg "TARGETARCH=${arch}" \
        --build-arg "USE_PREBUILT_WEB=1" \
        -t "$tag" \
        --load \
        -f Dockerfile \
        . || { echo "    WARN: all-in-one linux/${arch} build failed" >&2; continue; }

      # Prove the image contains the matching architecture binary.
      # distroless has no shell; copy binary out via docker create/cp.
      cid="$(docker create "$tag")"
      docker cp "${cid}:/phoenix" "${IMG_DIR}/phoenix-from-image-linux-${arch}" || true
      docker rm "$cid" >/dev/null
      if command -v file >/dev/null 2>&1 && [[ -f "${IMG_DIR}/phoenix-from-image-linux-${arch}" ]]; then
        file "${IMG_DIR}/phoenix-from-image-linux-${arch}" | tee -a "${OUT_DIR}/image-arch-proof.txt"
        # Expect arm64 binary for arm64 image, x86-64 for amd64.
        got="$(file "${IMG_DIR}/phoenix-from-image-linux-${arch}" || true)"
        case "$arch" in
          arm64)
            if ! grep -qiE 'arm64|aarch64' <<<"$got"; then
              echo "ERROR: arm64 image binary is not arm64: $got" >&2
              exit 1
            fi
            ;;
          amd64)
            if ! grep -qiE 'x86-64|x86_64|amd64' <<<"$got"; then
              echo "ERROR: amd64 image binary is not amd64: $got" >&2
              exit 1
            fi
            ;;
        esac
      fi

      if [[ "${SKIP_SBOM:-0}" != "1" ]] && command -v syft >/dev/null 2>&1; then
        syft "$tag" -o spdx-json > "${SBOM_DIR}/phoenix_${VERSION}_linux_${arch}.image.spdx.json" || true
      fi
    done

    # Split targets (amd64 only for local load convenience; CI builds both).
    for target in api worker web; do
      tag="phoenix-${target}:${VERSION}-linux-amd64"
      echo "    buildx build split/${target} linux/amd64 -> ${tag}"
      docker buildx build \
        --platform linux/amd64 \
        --target "$target" \
        --build-arg "VERSION=${VERSION}" \
        --build-arg "TARGETOS=linux" \
        --build-arg "TARGETARCH=amd64" \
        --build-arg "USE_PREBUILT_WEB=1" \
        -t "$tag" \
        --load \
        -f Dockerfile.split \
        . || echo "    WARN: split ${target} build failed" >&2
    done
  fi
else
  echo "==> SKIP_DOCKER=1 — skipping image builds"
fi

# ── Inventory ───────────────────────────────────────────────────────────────
{
  echo "# Phoenix release dry-run inventory"
  echo "version: ${VERSION}"
  echo "generated_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "## Binaries"
  ls -la "$BIN_DIR" 2>/dev/null || true
  echo
  echo "## Failed cross-compiles"
  if [[ ${#failed_bins[@]} -eq 0 ]]; then
    echo "(none)"
  else
    printf '%s\n' "${failed_bins[@]}"
  fi
  echo
  echo "## Charts"
  ls -la "$CHART_DIR" 2>/dev/null || true
  echo
  echo "## SBOMs"
  ls -la "$SBOM_DIR" 2>/dev/null || true
  echo
  echo "## Image arch proof"
  cat "${OUT_DIR}/image-arch-proof.txt" 2>/dev/null || echo "(none)"
  echo
  echo "## Binary arch listing"
  cat "${OUT_DIR}/binary-arch.txt" 2>/dev/null || echo "(none)"
  echo
  echo "## Publication status"
  echo "NO artifacts were pushed to GHCR, Helm OCI, GitHub Releases, or any registry."
  echo "NO git tag was created."
  if [[ ! -f LICENSE ]]; then
    echo "BLOCKER: LICENSE file is missing — publishing remains forbidden."
  fi
} | tee "${OUT_DIR}/INVENTORY.md"

echo "==> Dry-run complete. Artifacts under ${OUT_DIR}"
echo "    See docs/RELEASING.md for promotion blockers and no-publish rules."
