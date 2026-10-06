#!/usr/bin/env bash
# Regression tests for issue #67: chart-managed extension UI-token rotation must
# roll exactly the affected extension pod.
#
# Asserts, from rendered manifests only (no cluster):
#   1. deterministic: two renders with unchanged values produce byte-identical
#      extension pod templates (the managed Secret itself is deliberately
#      non-deterministic without a cluster — lookup + randAlphaNum — so the
#      comparison is scoped to the extension Deployments, per the issue)
#   2. rotating one extension's nonempty uiToken changes THAT extension's pod
#      template and leaves unrelated extensions byte-identical
#   3. the fingerprint is a sha256 digest — the raw uiToken value never appears
#      in the pod template, only the existing secretKeyRef hand-off
#   4. an extension without a chart-managed uiToken gets no checksum annotation
#      (no churn), and adding one later does change the pod template
#   5. externally managed Secret-backed env (envFromSecret / database.secretName)
#      is not fingerprinted (the chart cannot see those contents) and the
#      template documents the restart/rollout contract for rotating it
#
# All uiToken values used here are synthetic test fixtures ("fixture-*"), not
# credentials.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${repo_root}/charts/uptime-phoenix"
template="${chart}/templates/deployment-extension.yaml"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

ui_key="uiToken"
alpha_value="fixture-alpha-1"
alpha_rotated="fixture-alpha-2"
beta_value="fixture-beta-1"
shared_value="fixture-shared"

# --set-string argument assigning one extension's chart-managed launch value.
set_ui() {
  printf -- '--set-string=extensions[%s].%s=%s' "$1" "${ui_key}" "$2"
}

base_args=(
  template uptime-phoenix "${chart}"
  --set-string 'extensions[0].id=alpha'
  --set-string 'extensions[0].title=Alpha'
  --set-string 'extensions[0].path=/alpha'
  --set-string 'extensions[0].image=example.invalid/alpha:1'
  --set-string 'extensions[1].id=beta'
  --set-string 'extensions[1].title=Beta'
  --set-string 'extensions[1].path=/beta'
  --set-string 'extensions[1].image=example.invalid/beta:1'
)

render() {
  local output="$1"
  shift
  helm "${base_args[@]}" "$@" >"${output}"
}

# Print the extension Deployment document for one extension id (metadata.name
# ends in -ext-<id>). The extension Service shares that name, so the document
# must be a Deployment.
extract_extension() {
  local rendered="$1" id="$2"
  awk -v suffix="-ext-${id}" '
    /^---[[:space:]]*$/ {
      if (keep) { printf "%s", buf; keep = 0; exit }
      buf = ""; keep = 0; isdep = 0; next
    }
    { buf = buf $0 "\n" }
    $1 == "kind:" && $2 == "Deployment" { isdep = 1 }
    $1 == "name:" && isdep && $2 ~ ("^.*" suffix "$") { keep = 1 }
    END { if (keep) printf "%s", buf }
  ' "${rendered}"
}

require_extension() {
  local rendered="$1" id="$2" out="$3"
  extract_extension "${rendered}" "${id}" >"${out}"
  [[ -s "${out}" ]] || fail "extension '${id}' Deployment missing from ${rendered}"
}

checksum_value() {
  awk '/checksum\/ui-token:/ { print $2; exit }' "$1"
}

# sha256 hex digest: exactly 64 lowercase hex characters.
is_sha256() {
  local value="$1"
  [[ "${#value}" -eq 64 ]] || return 1
  [[ "${value}" == *[!0-9a-f]* ]] && return 1
  return 0
}

# ── 1. determinism ───────────────────────────────────────────────────────────
render "${tmp_dir}/det-a.yaml" \
  "$(set_ui 0 "${alpha_value}")" "$(set_ui 1 "${beta_value}")"
render "${tmp_dir}/det-b.yaml" \
  "$(set_ui 0 "${alpha_value}")" "$(set_ui 1 "${beta_value}")"
require_extension "${tmp_dir}/det-a.yaml" alpha "${tmp_dir}/alpha-det-a.yaml"
require_extension "${tmp_dir}/det-b.yaml" alpha "${tmp_dir}/alpha-det-b.yaml"
require_extension "${tmp_dir}/det-a.yaml" beta "${tmp_dir}/beta-det-a.yaml"
require_extension "${tmp_dir}/det-b.yaml" beta "${tmp_dir}/beta-det-b.yaml"
cmp -s "${tmp_dir}/alpha-det-a.yaml" "${tmp_dir}/alpha-det-b.yaml" \
  || fail "unchanged values rendered a different alpha pod template (non-deterministic)"
cmp -s "${tmp_dir}/beta-det-a.yaml" "${tmp_dir}/beta-det-b.yaml" \
  || fail "unchanged values rendered a different beta pod template (non-deterministic)"
echo "PASS determinism: unchanged values render byte-identical extension pods"

# ── 2. rotation rolls only the affected extension ────────────────────────────
render "${tmp_dir}/rot.yaml" \
  "$(set_ui 0 "${alpha_rotated}")" "$(set_ui 1 "${beta_value}")"
require_extension "${tmp_dir}/det-a.yaml" alpha "${tmp_dir}/alpha-before.yaml"
require_extension "${tmp_dir}/rot.yaml" alpha "${tmp_dir}/alpha-after.yaml"
require_extension "${tmp_dir}/det-a.yaml" beta "${tmp_dir}/beta-before.yaml"
require_extension "${tmp_dir}/rot.yaml" beta "${tmp_dir}/beta-after.yaml"
cmp -s "${tmp_dir}/alpha-before.yaml" "${tmp_dir}/alpha-after.yaml" \
  && fail "rotating alpha's launch value did not change alpha's pod template"
cmp -s "${tmp_dir}/beta-before.yaml" "${tmp_dir}/beta-after.yaml" \
  || fail "rotating alpha's launch value rolled the unrelated beta extension"
before="$(checksum_value "${tmp_dir}/alpha-before.yaml")"
after="$(checksum_value "${tmp_dir}/alpha-after.yaml")"
[[ -n "${before}" && -n "${after}" && "${before}" != "${after}" ]] \
  || fail "alpha's checksum/ui-token did not change on rotation"
echo "PASS rotation: only the rotated extension's pod template changes"

# ── 3. fingerprint shape, raw value never in the pod template ────────────────
is_sha256 "${before}" || fail "checksum/ui-token is not a sha256 digest"
for f in alpha-before alpha-after beta-before beta-after; do
  for raw in "${alpha_value}" "${alpha_rotated}" "${beta_value}"; do
    if grep -q "${raw}" "${tmp_dir}/${f}.yaml"; then
      fail "raw launch value leaked into the pod template (${f})"
    fi
  done
done
grep -q 'secretKeyRef:' "${tmp_dir}/alpha-before.yaml" \
  || fail "UI_TOKEN secretKeyRef hand-off disappeared"
grep -q 'ext-alpha-ui-token' "${tmp_dir}/alpha-before.yaml" \
  || fail "UI_TOKEN secret key changed (expected ext-alpha-ui-token)"
echo "PASS fingerprint: sha256 digest only, raw value never rendered in the pod"

# Per-extension scoping: identical values on two extensions still yield distinct
# fingerprints, so the annotation binds value AND extension identity.
render "${tmp_dir}/same-values.yaml" \
  "$(set_ui 0 "${shared_value}")" "$(set_ui 1 "${shared_value}")"
require_extension "${tmp_dir}/same-values.yaml" alpha "${tmp_dir}/alpha-same.yaml"
require_extension "${tmp_dir}/same-values.yaml" beta "${tmp_dir}/beta-same.yaml"
alpha_sum="$(checksum_value "${tmp_dir}/alpha-same.yaml")"
beta_sum="$(checksum_value "${tmp_dir}/beta-same.yaml")"
[[ -n "${alpha_sum}" && -n "${beta_sum}" && "${alpha_sum}" != "${beta_sum}" ]] \
  || fail "checksums are not scoped per extension"
echo "PASS scope: fingerprint binds launch value AND extension identity"

# ── 4. no chart-managed value ⇒ no annotation, no churn ──────────────────────
render "${tmp_dir}/no-value-a.yaml"
render "${tmp_dir}/no-value-b.yaml"
require_extension "${tmp_dir}/no-value-a.yaml" alpha "${tmp_dir}/alpha-no-value.yaml"
require_extension "${tmp_dir}/no-value-b.yaml" alpha "${tmp_dir}/alpha-no-value-b.yaml"
cmp -s "${tmp_dir}/alpha-no-value.yaml" "${tmp_dir}/alpha-no-value-b.yaml" \
  || fail "valueless renders produced different pod templates (non-deterministic)"
if grep -q 'checksum/ui-token' "${tmp_dir}/alpha-no-value.yaml"; then
  fail "checksum/ui-token rendered without a chart-managed launch value"
fi
if grep -q 'UI_TOKEN' "${tmp_dir}/alpha-no-value.yaml"; then
  fail "UI_TOKEN env rendered without a chart-managed launch value"
fi
if cmp -s "${tmp_dir}/alpha-no-value.yaml" "${tmp_dir}/alpha-before.yaml"; then
  fail "adding a launch value did not change the extension pod template"
fi
echo "PASS no-value: no annotation, deterministic, adding a value rolls the pod"

# ── 5. externally managed Secret-backed env: no fingerprint + documented ─────
ext_args=(
  --set-string 'extensions[0].envFromSecret=alpha-env'
  --set-string 'extensions[0].database.secretName=alpha-db'
  --set-string 'extensions[0].database.secretKey=dsn'
)
render "${tmp_dir}/ext-secret-a.yaml" "${ext_args[@]}"
render "${tmp_dir}/ext-secret-b.yaml" "${ext_args[@]}"
require_extension "${tmp_dir}/ext-secret-a.yaml" alpha "${tmp_dir}/alpha-ext-secret.yaml"
require_extension "${tmp_dir}/ext-secret-b.yaml" alpha "${tmp_dir}/alpha-ext-secret-b.yaml"
cmp -s "${tmp_dir}/alpha-ext-secret.yaml" "${tmp_dir}/alpha-ext-secret-b.yaml" \
  || fail "externally managed secret refs rendered non-deterministic pod templates"
if grep -q 'checksum/ui-token' "${tmp_dir}/alpha-ext-secret.yaml"; then
  fail "externally managed secret env must not get a chart-computed checksum"
fi
grep -q 'secretRef:' "${tmp_dir}/alpha-ext-secret.yaml" \
  || fail "envFromSecret reference disappeared"
grep -q 'alpha-db' "${tmp_dir}/alpha-ext-secret.yaml" \
  || fail "database secret reference disappeared"
grep -q 'rollout restart' "${template}" \
  || fail "deployment-extension.yaml must document the restart contract for externally managed secret env"
echo "PASS external secrets: no bogus checksum, references intact, contract documented"

echo "Extension UI-token rollout checksum OK"
