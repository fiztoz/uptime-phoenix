#!/usr/bin/env bash
# Validate the release-critical binaries and chart metadata produced by dry-run.sh.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 <version> <artifact-directory>" >&2
  exit 2
fi

version="$1"
artifact_dir="$2"
bin_dir="${artifact_dir}/binaries"
chart_dir="${artifact_dir}/charts"

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  echo "Invalid release version: ${version}" >&2
  exit 2
fi

for arch in amd64 arm64; do
  for name in uptime-phoenix-probe phoenix-probe-admin phoenix-probe-key; do
    binary="${bin_dir}/${name}_${version}_linux_${arch}"
    [[ -s "$binary" ]] || { echo "Missing release binary: ${binary}" >&2; exit 1; }
    build_info="$(go version -m "$binary")"
    [[ "$build_info" == *$'build\tGOOS=linux'* ]] || { echo "Not a Linux build: ${binary}" >&2; exit 1; }
    [[ "$build_info" == *$'build\tGOARCH='"${arch}"* ]] || { echo "Wrong architecture in build metadata: ${binary}" >&2; exit 1; }
  done
done

# The edge runtime uses linker-stamped internal/version.Version for AgentVersion.
string_dump="$(mktemp)"
trap 'rm -f "$string_dump"' EXIT
for arch in amd64 arm64; do
  probe="${bin_dir}/uptime-phoenix-probe_${version}_linux_${arch}"
  strings "$probe" > "$string_dump"
  grep -F "$version" "$string_dump" >/dev/null || { echo "Probe binary lacks advertised version ${version}" >&2; exit 1; }
done

if [[ "$(uname -s)" == "Linux" ]]; then
  run_arch="$(uname -m)"
  case "$run_arch" in
    x86_64) run_arch=amd64 ;;
    aarch64|arm64) run_arch=arm64 ;;
    *) echo "Unsupported Linux runner architecture: ${run_arch}" >&2; exit 1 ;;
  esac
  required=(
    "${bin_dir}/uptime-phoenix-probe_${version}_linux_${run_arch}"
    "${bin_dir}/phoenix-probe-admin_${version}_linux_${run_arch}"
    "${bin_dir}/phoenix-probe-key_${version}_linux_${run_arch}"
  )
  "${required[0]}" help >/dev/null
  "${required[1]}" help >/dev/null
  "${required[2]}" --help >/dev/null
elif [[ "$(uname -s)" == "Darwin" ]]; then
  case "$(uname -m)" in
    x86_64) run_arch=amd64 ;;
    arm64) run_arch=arm64 ;;
    *) echo "Unsupported macOS runner architecture: $(uname -m)" >&2; exit 1 ;;
  esac
  docker_context="${DOCKER_CONTEXT:-colima-phoenix-m6}"
  runner_image="${RELEASE_BINARY_RUNNER_IMAGE:-gcr.io/distroless/static-debian12:nonroot}"
  docker --context "$docker_context" image inspect "$runner_image" >/dev/null
  required=(
    "${bin_dir}/uptime-phoenix-probe_${version}_linux_${run_arch}"
    "${bin_dir}/phoenix-probe-admin_${version}_linux_${run_arch}"
    "${bin_dir}/phoenix-probe-key_${version}_linux_${run_arch}"
  )
  for binary in "${required[@]}"; do
    case "$binary" in
      *phoenix-probe_*) args=(help) ;;
      *phoenix-probe-admin_*) args=(help) ;;
      *phoenix-probe-key_*) args=(--help) ;;
    esac
    binary_abs="$(cd "$(dirname "$binary")" && pwd)/$(basename "$binary")"
    docker --context "$docker_context" run --rm --platform "linux/${run_arch}" \
      --entrypoint /tmp/release-cli -v "${binary_abs}:/tmp/release-cli:ro" \
      "$runner_image" "${args[@]}" >/dev/null
  done
else
  echo "Cannot execute Linux release binaries on $(uname -s)" >&2
  exit 1
fi

[[ -s "${bin_dir}/SHA256SUMS" ]] || { echo "Missing binary checksums" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$bin_dir" && sha256sum -c SHA256SUMS)
else
  (cd "$bin_dir" && shasum -a 256 -c SHA256SUMS)
fi

chart="${chart_dir}/uptime-phoenix-${version}.tgz"
if [[ -f "$chart" ]]; then
  chart_metadata="$(helm show chart "$chart")"
  chart_version="$(awk '$0 ~ /^version: / {sub(/^version: /, ""); print; exit}' <<<"$chart_metadata")"
  chart_app_version="$(awk '$0 ~ /^appVersion: / {sub(/^appVersion: /, ""); print; exit}' <<<"$chart_metadata")"
  [[ "$chart_version" == "$version" ]] || { echo "Chart version does not match ${version}" >&2; exit 1; }
  [[ "$chart_app_version" == "$version" ]] || { echo "Chart appVersion does not match ${version}" >&2; exit 1; }
fi

echo "Release artifacts verified for ${version}"
