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
#   STAGES        Comma-separated subset of stages to run (default: all).
#                 Valid: binaries, checksums, sbom, chart, images, inventory.
#                 CI fans these out into parallel jobs; a bare local run leaves
#                 STAGES unset and executes every stage in order (unchanged).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:-}"
if [[ -z "$VERSION" ]]; then
  echo "VERSION is required (e.g. VERSION=0.0.0-snapshot.1)" >&2
  exit 2
fi
if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  echo "VERSION is not SemVer-compatible: ${VERSION}" >&2
  exit 2
fi

# ── Stage selection ─────────────────────────────────────────────────────────
# Default "all" preserves the single-shot local behavior. CI passes e.g.
# STAGES=binaries,checksums,sbom for one job and STAGES=images for another,
# then STAGES=inventory in an aggregation job after downloading the partials.
STAGES="${STAGES:-all}"
want_stage() {
  local s="$1"
  [[ "$STAGES" == "all" ]] && return 0
  case ",${STAGES}," in
    *",${s},"*) return 0 ;;
    *) return 1 ;;
  esac
}

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
# Only the stages that compile Go or build images need the embedded frontend.
if { want_stage binaries || want_stage images; } && [[ ! -f web/dist/index.html ]]; then
  echo "==> web/dist missing — building frontend"
  if command -v bun >/dev/null 2>&1 && [[ -f web/package.json ]]; then
    (cd web && bun install --frozen-lockfile && bun run build)
  else
    echo "ERROR: Bun and web/package.json are required to build release artifacts" >&2
    exit 1
  fi
elif want_stage binaries || want_stage images; then
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
if want_stage binaries; then
  for plat in "${MATRIX[@]}"; do
    goos="${plat%/*}"
    goarch="${plat#*/}"
    build_one "$goos" "$goarch" ./cmd/app "uptime-phoenix"
    build_one "$goos" "$goarch" ./cmd/kuma-import "uptime-phoenix-kuma-import"
    build_one "$goos" "$goarch" ./cmd/phoenix-config "uptime-phoenix-config"
    build_one "$goos" "$goarch" ./cmd/api "uptime-phoenix-api"
    build_one "$goos" "$goarch" ./cmd/worker "uptime-phoenix-worker"
    # Remote edge and the two explicit hub operator tools ship with the same
    # stamped version as the hub. The probe is supported on Linux only.
    if [[ "$goos" == "linux" ]]; then
      build_one "$goos" "$goarch" ./cmd/probe "uptime-phoenix-probe"
    fi
    build_one "$goos" "$goarch" ./cmd/phoenix-probe-admin "phoenix-probe-admin"
    build_one "$goos" "$goarch" ./cmd/phoenix-probe-key "phoenix-probe-key"
  done

  # Persist the failed-cross-compile list so a separate inventory stage/job
  # (which does not share this shell's arrays) can still report it.
  if [[ ${#failed_bins[@]} -eq 0 ]]; then
    : > "${OUT_DIR}/failed-bins.txt"
  else
    printf '%s\n' "${failed_bins[@]}" > "${OUT_DIR}/failed-bins.txt"
    echo "ERROR: one or more release binaries failed to build" >&2
    exit 1
  fi
else
  echo "    (skipped: 'binaries' not in STAGES=${STAGES})"
fi

if want_stage checksums; then
  echo "==> Checksums"
  if [[ -z "$(find "$BIN_DIR" -maxdepth 1 -type f ! -name SHA256SUMS -print -quit)" ]]; then
    echo "ERROR: no binaries available for checksums in ${BIN_DIR}" >&2
    exit 1
  fi
  (
    cd "$BIN_DIR"
    rm -f SHA256SUMS
    if command -v shasum >/dev/null 2>&1; then
      shasum -a 256 ./* > SHA256SUMS
    elif command -v sha256sum >/dev/null 2>&1; then
      sha256sum ./* > SHA256SUMS
    else
      echo "no sha256 tool available" > SHA256SUMS
    fi
  )
fi

# ── SBOM for binaries ───────────────────────────────────────────────────────
if want_stage sbom && [[ "${SKIP_SBOM:-0}" != "1" ]]; then
  echo "==> SBOM (syft) for binaries"
  if command -v syft >/dev/null 2>&1; then
    # A separately selected stage has no in-memory build list. Discover only
    # this version's artifacts, excluding checksums and older candidates.
    sbom_count=0
    for bin in "${BIN_DIR}"/*_"${VERSION}"_*; do
      [[ -f "$bin" ]] || continue
      base="$(basename "$bin")"
      sbom_tmp="$(mktemp "${SBOM_DIR}/.sbom.XXXXXX")"
      if ! syft "$bin" -o spdx-json > "$sbom_tmp" || [[ ! -s "$sbom_tmp" ]]; then
        rm -f "$sbom_tmp"
        echo "ERROR: syft failed or produced an empty SBOM for $bin" >&2
        exit 1
      fi
      mv "$sbom_tmp" "${SBOM_DIR}/${base}.spdx.json"
      sbom_count=$((sbom_count + 1))
    done
    if ((sbom_count == 0)); then
      echo "ERROR: no ${VERSION} binaries available for SBOMs" >&2
      exit 1
    fi
  else
    echo "    syft not installed; skipping local binary SBOMs (CI installs it)"
  fi
fi

# ── Helm chart package ──────────────────────────────────────────────────────
if want_stage chart; then
echo "==> Helm chart package"
# Stamp both chart version fields for this snapshot without permanently editing the chart in
# git when run from a dirty tree: work on a copy.
CHART_SRC="${OUT_DIR}/chart-src"
rm -rf "$CHART_SRC"
cp -R charts/uptime-phoenix "$CHART_SRC"
# Portable in-place appVersion stamp.
python3 - "$CHART_SRC/Chart.yaml" "$VERSION" <<'PY'
import pathlib, sys, re
path = pathlib.Path(sys.argv[1])
version = sys.argv[2]
text = path.read_text(encoding="utf-8")
text2, version_count = re.subn(r'(?m)^version:\s*.*$', f'version: "{version}"', text, count=1)
text2, app_count = re.subn(r'(?m)^appVersion:\s*.*$', f'appVersion: "{version}"', text2, count=1)
if version_count != 1 or app_count != 1:
    raise SystemExit(f"failed to stamp version metadata in {path}")
path.write_text(text2, encoding="utf-8")
print(f"stamped version={version} appVersion={version}")
PY

if command -v helm >/dev/null 2>&1; then
  helm lint "$CHART_SRC"
  helm package "$CHART_SRC" --version "$VERSION" --app-version "$VERSION" --destination "$CHART_DIR"
  helm template uptime-phoenix "$CHART_SRC" > "${OUT_DIR}/helm-template.yaml"
else
  echo "    helm not installed; skipped package/lint/template" >&2
fi
fi

prove_image_binary() {
  local tag="$1" arch="$2" family="$3" binary_path="$4"
  local cid proof elf_proof
  # Prove the image contains the matching architecture binary.
  # distroless has no shell; copy binary out via docker create/cp.
  if ! cid="$(docker create "$tag")" || [[ -z "$cid" ]]; then
    echo "ERROR: could not create ${tag} to verify its binary architecture" >&2
    exit 1
  fi
  proof="${IMG_DIR}/${family}-from-image-linux-${arch}"
  if ! docker cp "${cid}:${binary_path}" "$proof"; then
    docker rm "$cid" >/dev/null 2>&1 || true
    echo "ERROR: could not extract the binary from ${tag}" >&2
    exit 1
  fi
  if ! docker rm "$cid" >/dev/null; then
    echo "ERROR: could not remove verification container for ${tag}" >&2
    exit 1
  fi
  if [[ ! -s "$proof" ]]; then
    echo "ERROR: extracted binary is missing or empty for ${tag}" >&2
    exit 1
  fi
  if ! command -v python3 >/dev/null 2>&1; then
    echo "ERROR: Python 3 is required to verify image binary architectures" >&2
    exit 1
  fi
  elf_proof="$(python3 - "$proof" "$arch" <<'PY'
import pathlib, struct, sys
path = pathlib.Path(sys.argv[1])
arch = sys.argv[2]
header = path.read_bytes()[:20]
if len(header) < 20 or header[:4] != b"\x7fELF":
    raise SystemExit(f"{path}: extracted file is not an ELF executable")
if header[4] != 2 or header[5] not in (1, 2):
    raise SystemExit(f"{path}: expected an ELF64 binary")
endian = "<" if header[5] == 1 else ">"
machine = struct.unpack(endian + "H", header[18:20])[0]
expected = {"amd64": 62, "arm64": 183}.get(arch)
if expected is None or machine != expected:
    raise SystemExit(f"{path}: ELF machine {machine} does not match linux/{arch}")
print(f"{path}: ELF64 machine={machine} linux/{arch}")
PY
  )" || {
    echo "ERROR: extracted binary architecture proof failed for ${tag}" >&2
    exit 1
  }
  printf '%s\n' "$elf_proof" | tee -a "${OUT_DIR}/image-arch-proof.txt"
  # file(1) is optional human-readable corroboration; the ELF header check
  # above is the required architecture proof.
  if command -v file >/dev/null 2>&1; then
    file "$proof" | tee -a "${OUT_DIR}/image-arch-proof.txt"
  fi

}

# ── Multi-arch images (load/export only — never push) ───────────────────────
if want_stage images && [[ "${SKIP_DOCKER:-0}" != "1" ]]; then
  echo "==> Docker multi-arch builds (no push)"
  if ! command -v docker >/dev/null 2>&1; then
    echo "ERROR: Docker is required when the images stage is selected" >&2
    exit 1
  else
    # All-in-one image for linux/amd64 and linux/arm64 separately so we can
    # inspect the binary architecture of each, then optionally combine.
    for arch in amd64 arm64; do
      tag="uptime-phoenix:${VERSION}-linux-${arch}"
      echo "    buildx build linux/${arch} -> ${tag}"
      if ! docker buildx build \
        --platform "linux/${arch}" \
        --build-arg "VERSION=${VERSION}" \
        --build-arg "TARGETOS=linux" \
        --build-arg "TARGETARCH=${arch}" \
        --build-arg "USE_PREBUILT_WEB=1" \
        -t "$tag" \
        --load \
        -f Dockerfile \
        .; then
        echo "ERROR: all-in-one linux/${arch} image build failed" >&2
        exit 1
      fi

      prove_image_binary "$tag" "$arch" uptime-phoenix /uptime-phoenix

      if [[ "${SKIP_SBOM:-0}" != "1" ]] && command -v syft >/dev/null 2>&1; then
        syft "$tag" -o spdx-json > "${SBOM_DIR}/uptime-phoenix_${VERSION}_linux_${arch}.image.spdx.json" || true
      fi
    done

    # The published image matrix includes both supported Linux architectures
    # for the probe runtime and every split target; gate both locally too.
    for arch in amd64 arm64; do
      probe_tag="uptime-phoenix-probe:${VERSION}-linux-${arch}"
      echo "    buildx build probe linux/${arch} -> ${probe_tag}"
      if ! docker buildx build --platform "linux/${arch}" \
        --build-arg "VERSION=${VERSION}" --build-arg "TARGETOS=linux" --build-arg "TARGETARCH=${arch}" \
        -t "$probe_tag" --load -f Dockerfile.probe .; then
        echo "ERROR: probe linux/${arch} image build failed" >&2
        exit 1
      fi
      prove_image_binary "$probe_tag" "$arch" uptime-phoenix-probe /uptime-phoenix-probe

      for target in api worker web; do
        tag="uptime-phoenix-${target}:${VERSION}-linux-${arch}"
        echo "    buildx build split/${target} linux/${arch} -> ${tag}"
        if ! docker buildx build \
          --platform "linux/${arch}" \
          --target "$target" \
          --build-arg "VERSION=${VERSION}" \
          --build-arg "TARGETOS=linux" \
          --build-arg "TARGETARCH=${arch}" \
          --build-arg "USE_PREBUILT_WEB=1" \
          -t "$tag" \
          --load \
          -f Dockerfile.split \
          .; then
          echo "ERROR: split ${target} linux/${arch} image build failed" >&2
          exit 1
        fi
        if [[ "$target" != web ]]; then
          prove_image_binary "$tag" "$arch" "uptime-phoenix-${target}" "/uptime-phoenix-${target}"
        fi
      done
    done
  fi
else
  echo "==> SKIP_DOCKER=1 or images stage not selected — skipping image builds"
fi

# ── Inventory ───────────────────────────────────────────────────────────────
if want_stage inventory; then
{
  echo "# Phoenix release dry-run inventory"
  echo "version: ${VERSION}"
  echo "generated_at: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "## Binaries"
  ls -la "$BIN_DIR" 2>/dev/null || true
  echo
  echo "## Failed cross-compiles"
  if [[ -f "${OUT_DIR}/failed-bins.txt" ]]; then
    if [[ -s "${OUT_DIR}/failed-bins.txt" ]]; then
      cat "${OUT_DIR}/failed-bins.txt"
    else
      echo "(none)"
    fi
  elif [[ ${#failed_bins[@]} -eq 0 ]]; then
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
fi

echo "==> Dry-run complete. Artifacts under ${OUT_DIR}"
echo "    See docs/RELEASING.md for promotion blockers and no-publish rules."
