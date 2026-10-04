#!/usr/bin/env bash
# Regression test: every required image build and architecture proof fails closed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DRY_RUN="${ROOT}/scripts/release/dry-run.sh"
TMP_BASE="${TMPDIR:-/private/tmp}"
TEST_ROOT="$(mktemp -d "${TMP_BASE%/}/phoenix-image-gate.XXXXXX")"
chmod 700 "$TEST_ROOT"
trap 'rm -rf "$TEST_ROOT"' EXIT
FAKE_BIN="${TEST_ROOT}/fake-bin"
mkdir -m 700 "$FAKE_BIN"
MINIMAL_BIN="${TEST_ROOT}/minimal-bin"
mkdir -m 700 "$MINIMAL_BIN"
for utility in bash dirname mkdir rm; do
  ln -s "$(command -v "$utility")" "${MINIMAL_BIN}/${utility}"
done

cat > "${FAKE_BIN}/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
command_name="$1"
shift
if [[ "$command_name" == buildx && "${1:-}" == build ]]; then
  shift
  platform=""
  target=""
  tag=""
  while (($#)); do
    case "$1" in
      --platform) platform="$2"; shift 2 ;;
      --target) target="$2"; shift 2 ;;
      -t) tag="$2"; shift 2 ;;
      *) shift ;;
    esac
  done
  if [[ "${FAKE_FAIL_KIND:-}" == allinone && -z "$target" && "$platform" == linux/amd64 && "$tag" == uptime-phoenix:* ]]; then
    exit 41
  fi
  if [[ "${FAKE_FAIL_KIND:-}" == probe && "$tag" == uptime-phoenix-probe:* ]]; then
    exit 42
  fi
  if [[ "${FAKE_FAIL_KIND:-}" == split-api && "$target" == api ]]; then
    exit 43
  fi
  if [[ "${FAKE_FAIL_KIND:-}" == split-worker && "$target" == worker ]]; then
    exit 44
  fi
  if [[ "${FAKE_FAIL_KIND:-}" == split-web && "$target" == web ]]; then
    exit 45
  fi
  if [[ -n "${FAKE_DOCKER_LOG:-}" ]]; then
    printf '%s %s %s %s\n' "$platform" "$target" "$tag" "${FAKE_FAIL_KIND:-}" >> "$FAKE_DOCKER_LOG"
  fi
  if [[ "$command_name" == buildx ]]; then
    exit 0
  fi
fi
if [[ "$command_name" == create ]]; then
  printf 'fixture-%s\n' "${1//[^a-zA-Z0-9]/_}"
  exit 0
fi
if [[ "$command_name" == cp ]]; then
  if [[ "${FAKE_FAIL_KIND:-}" == extract ]]; then
    exit 46
  fi
  source_path="$1"
  output_path="$2"
  if [[ "${FAKE_FAIL_KIND:-}" == wrong-arch ]]; then
    machine='\xb7\x00'
  elif [[ "$source_path" == *arm64* ]]; then
    machine='\xb7\x00'
  else
    machine='\x3e\x00'
  fi
  printf '\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00%b' "$machine" > "$output_path"
  exit 0
fi
if [[ "$command_name" == rm ]]; then
  exit 0
fi
echo "unexpected fake docker command" >&2
exit 47
DOCKER

cat > "${FAKE_BIN}/file" <<'FILE'
#!/usr/bin/env bash
set -euo pipefail
path="$1"
if [[ "$path" == *arm64* ]]; then
  printf '%s: ELF 64-bit LSB executable, ARM aarch64\n' "$path"
else
  printf '%s: ELF 64-bit LSB executable, x86-64\n' "$path"
fi
FILE
chmod 700 "${FAKE_BIN}/docker" "${FAKE_BIN}/file"

run_case() {
  local name="$1" failure="$2" expected="$3"
  local out="${TEST_ROOT}/out-${name}" log="${TEST_ROOT}/${name}.log" rc=0
  local docker_log="${TEST_ROOT}/${name}-docker.log"
  mkdir -m 700 "$out"
  : > "$docker_log"
  if PATH="${FAKE_BIN}:${PATH}" VERSION=0.5.0-rc.1 OUT_DIR="$out" STAGES=images \
      SKIP_DOCKER=0 SKIP_SBOM=1 FAKE_FAIL_KIND="$failure" FAKE_DOCKER_LOG="$docker_log" \
      bash "$DRY_RUN" >"$log" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  chmod 600 "$log" "$docker_log"
  if [[ "$expected" == fail && "$rc" -eq 0 ]]; then
    echo "FAIL ${name}: stage unexpectedly succeeded" >&2
    cat "$log" >&2
    return 1
  fi
  if [[ "$expected" == pass && "$rc" -ne 0 ]]; then
    echo "FAIL ${name}: stage exited ${rc}" >&2
    cat "$log" >&2
    return 1
  fi
  if [[ "$expected" == pass ]]; then
    for arch in amd64 arm64; do
      grep -q "linux/${arch}" "$log"
      grep -q "uptime-phoenix:0.5.0-rc.1-linux-${arch}" "$docker_log"
      grep -q "uptime-phoenix-probe:0.5.0-rc.1-linux-${arch}" "$docker_log"
      for target in api worker web; do
        grep -q "uptime-phoenix-${target}:0.5.0-rc.1-linux-${arch}" "$docker_log"
      done
    done
  fi
  printf 'PASS %s exit=%s expected=%s\n' "$name" "$rc" "$expected"
}

run_missing_docker_case() {
  local out="${TEST_ROOT}/out-missing-docker" log="${TEST_ROOT}/missing-docker.log" rc=0
  mkdir -m 700 "$out"
  if PATH="$MINIMAL_BIN" VERSION=0.5.0-rc.1 OUT_DIR="$out" STAGES=images \
      SKIP_DOCKER=0 SKIP_SBOM=1 bash "$DRY_RUN" >"$log" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  chmod 600 "$log"
  if [[ "$rc" -eq 0 ]]; then
    echo "FAIL missing-docker: stage unexpectedly succeeded" >&2
    cat "$log" >&2
    return 1
  fi
  printf 'PASS missing-docker exit=%s expected=fail\n' "$rc"
}

run_case allinone-failure allinone fail
run_case probe-failure probe fail
run_case split-api-failure split-api fail
run_case split-worker-failure split-worker fail
run_case split-web-failure split-web fail
run_case extraction-failure extract fail
run_case architecture-mismatch wrong-arch fail
run_case successful-fake-build none pass
run_missing_docker_case
