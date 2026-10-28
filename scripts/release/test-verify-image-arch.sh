#!/usr/bin/env bash
# Regression test for verify-image-arch.sh (the post-push check in release.yml).
# The fake docker models Docker's classic image store: once an index digest
# has been pulled for one platform, pulling it again for another platform
# fails with "cannot overwrite digest" (what broke the v0.6.1 release). The
# verifier must pull each platform by its own manifest digest instead.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERIFY="${ROOT}/scripts/release/verify-image-arch.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/phoenix-verify-arch.XXXXXX")"
chmod 700 "$TEST_ROOT"
trap 'rm -rf "$TEST_ROOT"' EXIT
FAKE_BIN="${TEST_ROOT}/fake-bin"
mkdir -m 700 "$FAKE_BIN"

INDEX=sha256:1111111111111111111111111111111111111111111111111111111111111111
AMD64=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
ARM64=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
OTHER=sha256:2222222222222222222222222222222222222222222222222222222222222222
IMAGE=ghcr.io/example/uptime-phoenix
export INDEX AMD64 ARM64 OTHER IMAGE

cat > "${FAKE_BIN}/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_STATE:?}"
if [[ "$*" == "buildx imagetools inspect "* ]]; then
  shift 3
  raw=0; ref=""
  while (($#)); do
    case "$1" in
      --raw) raw=1; shift ;;
      --format) shift 2 ;;
      *) ref="$1"; shift ;;
    esac
  done
  if ((raw)); then
    arm_digest="$ARM64"
    [[ "${FAKE_MODE:-}" == missing-arm64 ]] && arm_digest=""
    python3 - "$AMD64" "$arm_digest" <<'PY'
import json, sys
manifests = [{"digest": sys.argv[1], "platform": {"os": "linux", "architecture": "amd64"}}]
if sys.argv[2]:
    manifests.append({"digest": sys.argv[2], "platform": {"os": "linux", "architecture": "arm64"}})
manifests.append({"digest": "sha256:" + "c" * 64, "platform": {"os": "unknown", "architecture": "unknown"}})
print(json.dumps({"schemaVersion": 2, "manifests": manifests}))
PY
    exit 0
  fi
  digest="$INDEX"
  [[ "$ref" == *:stale ]] && digest="$OTHER"
  printf '{"digest":"%s"}\n' "$digest"
  exit 0
fi
case "$1" in
  create)
    platform="$4"; ref="$5"; digest="${ref##*@}"
    if [[ -f "${state}/pulled-${digest}" && "$(cat "${state}/pulled-${digest}")" != "$platform" ]]; then
      echo "cannot overwrite digest ${digest}" >&2
      exit 1
    fi
    echo "$platform" > "${state}/pulled-${digest}"
    echo "$platform" >> "${state}/create.log"
    echo "cid-${digest#sha256:}"
    ;;
  cp)
    cid="${2%%:*}"; digest="sha256:${cid#cid-}"
    machine=62
    [[ "$digest" == "$ARM64" && "${FAKE_MODE:-}" != wrong-arch ]] && machine=183
    [[ "$digest" == "$INDEX" ]] && machine=62
    python3 -c 'import struct,sys; open(sys.argv[1],"wb").write(b"\x7fELF\x02\x01\x01"+b"\0"*11+struct.pack("<H",int(sys.argv[2]))+b"\0"*44)' "$3" "$machine"
    ;;
  rm) ;;
  *) echo "unexpected docker $*" >&2; exit 99 ;;
esac
DOCKER
chmod 700 "${FAKE_BIN}/docker"

run_case() {
  local name="$1" mode="$2" expected="$3" reason="$4"; shift 4
  local state="${TEST_ROOT}/state-${name}" log="${TEST_ROOT}/${name}.log" rc=0
  mkdir -m 700 "$state"
  if PATH="${FAKE_BIN}:${PATH}" FAKE_STATE="$state" FAKE_MODE="$mode" \
      bash "$VERIFY" "${IMAGE}@${INDEX}" /uptime-phoenix "$@" >"$log" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  if [[ "$expected" == pass && "$rc" -ne 0 ]] || [[ "$expected" == fail && "$rc" -eq 0 ]]; then
    echo "FAIL ${name}: exit ${rc}, expected ${expected}" >&2
    cat "$log" >&2
    return 1
  fi
  if ! grep -q -- "$reason" "$log"; then
    echo "FAIL ${name}: output lacks '${reason}'" >&2
    cat "$log" >&2
    return 1
  fi
  if [[ "$expected" == pass ]]; then
    grep -q 'linux/amd64 /uptime-phoenix: ELF64 machine=62' "$log"
    grep -q 'linux/arm64 /uptime-phoenix: ELF64 machine=183' "$log"
    [[ "$(wc -l < "${state}/create.log")" -eq 2 ]]
  fi
  printf 'PASS %s exit=%s expected=%s\n' "$name" "$rc" "$expected"
}

run_case both-platforms-classic-store none pass "0.0.1 -> ${INDEX}" --tag "${IMAGE}:0.0.1"
run_case arm64-carries-x86-64 wrong-arch fail "(linux/arm64, ${ARM64}) /uptime-phoenix has the wrong architecture"
run_case index-missing-arm64 missing-arm64 fail "missing platforms \['linux/arm64'\]"
run_case tag-points-elsewhere none fail "stale resolves to ${OTHER}" --tag "${IMAGE}:stale"
