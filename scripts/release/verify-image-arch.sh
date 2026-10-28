#!/usr/bin/env bash
# Prove every platform of a pushed multi-arch image carries a binary built for
# that platform. Fails closed on a missing platform, an extraction error, or an
# ELF e_machine mismatch (e.g. x86-64 inside linux/arm64, the v0.5.1/v0.6.0 bug).
#
# Usage: verify-image-arch.sh IMAGE@DIGEST BINARY_PATH [--tag TAG]... [PLATFORM...]
#   IMAGE@DIGEST registry reference pinned by digest: ghcr.io/o/img@sha256:...
#   BINARY_PATH  absolute path of the binary inside the image
#   --tag TAG    also require the pushed TAG to resolve to that digest
#   PLATFORM     defaults to linux/amd64 linux/arm64
set -euo pipefail

if (($# < 2)); then
  echo "usage: $0 IMAGE@DIGEST BINARY_PATH [--tag TAG]... [PLATFORM...]" >&2
  exit 2
fi
ref="$1"
binary_path="$2"
shift 2
tags=()
platforms=()
while (($#)); do
  case "$1" in
    --tag) tags+=("$2"); shift 2 ;;
    *) platforms+=("$1"); shift ;;
  esac
done
((${#platforms[@]})) || platforms=(linux/amd64 linux/arm64)

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/phoenix-image-arch.XXXXXX")"
trap 'rm -rf "$work"' EXIT

docker buildx imagetools inspect --raw "$ref" > "${work}/index.json"
# Resolve each platform to its own manifest digest. Pulling the index digest
# once per platform fails on Docker's classic image store ("cannot overwrite
# digest", v0.6.1 release): the second platform cannot be stored under the
# same index reference. A per-platform digest names exactly one image.
python3 - "${work}/index.json" "${work}/platform-digests" "${platforms[@]}" <<'PY'
import json, sys
index = json.load(open(sys.argv[1]))
digests = {}
for m in index.get("manifests", []):
    plat = m.get("platform", {})
    if plat.get("os") in (None, "unknown") or plat.get("variant") not in (None, "v8"):
        continue
    key = f"{plat['os']}/{plat['architecture']}"
    if key in digests:
        raise SystemExit(f"image index has more than one {key} manifest")
    digests[key] = m["digest"]
missing = [p for p in sys.argv[3:] if p not in digests]
if missing:
    raise SystemExit(f"image index is missing platforms {missing}; has {sorted(digests)}")
with open(sys.argv[2], "w") as out:
    for p in sys.argv[3:]:
        out.write(f"{p} {digests[p]}\n")
print("index platforms:", " ".join(sorted(digests)))
PY

repo="${ref%@*}"
while read -r platform platform_digest <&3; do
  arch="${platform#*/}"
  out="${work}/bin-${arch}"
  platform_ref="${repo}@${platform_digest}"
  cid="$(docker create --pull=always --platform "$platform" "$platform_ref")"
  if ! docker cp "${cid}:${binary_path}" "$out" >/dev/null; then
    docker rm "$cid" >/dev/null 2>&1 || true
    echo "ERROR: could not extract ${binary_path} from ${platform_ref} (${platform})" >&2
    exit 1
  fi
  docker rm "$cid" >/dev/null
  if ! proof="$(python3 "${here}/check-elf-arch.py" "$out" "$arch")"; then
    echo "ERROR: ${ref} (${platform}, ${platform_digest}) ${binary_path} has the wrong architecture" >&2
    file -b "$out" >&2 2>/dev/null || true
    exit 1
  fi
  echo "${platform} ${binary_path}: ${proof##*: }"
  if command -v file >/dev/null 2>&1; then
    file -b "$out"
  fi
done 3< "${work}/platform-digests"

digest="${ref##*@}"
for tag in "${tags[@]}"; do
  tag_digest="$(docker buildx imagetools inspect "$tag" --format '{{json .Manifest}}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["digest"])')"
  if [[ "$tag_digest" != "$digest" ]]; then
    echo "ERROR: ${tag} resolves to ${tag_digest}, expected ${digest}" >&2
    exit 1
  fi
  echo "${tag} -> ${digest}"
done
