#!/usr/bin/env bash
# Resolve workflow inputs into validated release outputs.
set -euo pipefail

: "${EVENT_NAME:?EVENT_NAME is required}"
: "${REF_NAME:?REF_NAME is required}"
: "${REPO_OWNER:?REPO_OWNER is required}"
: "${VERSION_REGEX:?VERSION_REGEX is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

to01() { if [[ "$1" == "true" ]]; then printf '1'; else printf '0'; fi; }
owner="$(printf '%s' "$REPO_OWNER" | tr '[:upper:]' '[:lower:]')"

if [[ "$EVENT_NAME" == "push" ]]; then
  raw="$REF_NAME"
  version="${raw#v}"
  tag_name="v${version}"
  publish=1
  build_aio=1
  build_split=1
  build_probe=1
  build_chart=1
  build_binaries=1
  skip_docker=0
  echo "tag_push: full release version=${version} tag=${tag_name} (publish after dry-run if Environment approves)"
else
  version="${RAW_VERSION#v}"
  if [[ -z "$version" ]]; then
    echo "version input is empty" >&2
    exit 2
  fi
  publish="$(to01 "${PUBLISH_INPUT:-false}")"
  build_aio="$(to01 "${BUILD_AIO_INPUT:-false}")"
  build_split="$(to01 "${BUILD_SPLIT_INPUT:-false}")"
  build_probe="$(to01 "${BUILD_PROBE_INPUT:-false}")"
  build_chart="$(to01 "${BUILD_CHART_INPUT:-false}")"
  build_binaries="$(to01 "${BUILD_BINARIES_INPUT:-false}")"
  if [[ "${SKIP_DOCKER_INPUT:-false}" == "true" ]] || {
    [[ "$build_aio" == "0" ]] && [[ "$build_split" == "0" ]] && [[ "$build_probe" == "0" ]]
  }; then
    skip_docker=1
  else
    skip_docker=0
  fi
  if [[ "$publish" == "1" ]]; then
    tag_name="v${version}"
    if [[ "${build_aio}${build_split}${build_probe}${build_chart}${build_binaries}" == "00000" ]]; then
      echo "ERROR: publish=true but no artifact selected (tick at least one of chart / split / all-in-one / probe / binaries)." >&2
      exit 2
    fi
  else
    tag_name=""
  fi
  echo "dispatch: version=${version} publish=${publish} aio=${build_aio} split=${build_split} probe=${build_probe} chart=${build_chart} binaries=${build_binaries} skip_docker=${skip_docker}"
fi

if ! printf '%s' "$version" | grep -Eq "$VERSION_REGEX"; then
  echo "ERROR: version '${version}' is not supported SemVer (expected X.Y.Z or X.Y.Z-prerelease)." >&2
  exit 2
fi

{
  echo "image_owner=${owner}"
  echo "version=${version}"
  echo "skip_docker=${skip_docker}"
  echo "tag_name=${tag_name}"
  echo "publish=${publish}"
  echo "build_aio=${build_aio}"
  echo "build_split=${build_split}"
  echo "build_probe=${build_probe}"
  echo "build_chart=${build_chart}"
  echo "build_binaries=${build_binaries}"
} >> "$GITHUB_OUTPUT"
