#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart="${repo_root}/charts/uptime-phoenix"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

render_extensions() {
  local image="$1"
  local title="$2"
  local database_enabled="$3"
  local output="$4"
  local args=(
    template uptime-phoenix "${chart}"
    --set mode=split
    --set-string 'extensions[0].id=checksum-scope'
    --set-string "extensions[0].title=${title}"
    --set-string 'extensions[0].path=/checksum-scope'
    --set-string "extensions[0].image=${image}"
  )

  if [[ "${database_enabled}" == true ]]; then
    args+=(--set database.engine=mariadb --set mariadb.enabled=true)
  fi
  helm "${args[@]}" >"${output}"
}

extract_document() {
  local rendered="$1"
  local template="$2"

  awk -v marker="# Source: uptime-phoenix/templates/${template}" '
    $0 == marker { found = 1; next }
    found && /^---$/ { exit }
    found { print }
  ' "${rendered}"
}

checksums_for() {
  extract_document "$1" "$2" | awk '/checksum\/(config|secret):/ { print }'
}

secret_checksum_for() {
  extract_document "$1" "$2" | awk '/checksum\/secret:/ { print }'
}

assert_same_document() {
  local before="$1"
  local after="$2"
  local template="$3"
  local before_document after_document
  before_document="$(extract_document "${before}" "${template}")"
  after_document="$(extract_document "${after}" "${template}")"

  if [[ -z "${before_document}" || -z "${after_document}" ]]; then
    echo "MISSING: rendered workload in ${template}" >&2
    exit 1
  fi
  if [[ "${before_document}" != "${after_document}" ]]; then
    echo "UNEXPECTED ARGO DRIFT: extension image changed ${template}" >&2
    exit 1
  fi
}

assert_same_checksums() {
  local before="$1"
  local after="$2"
  local template="$3"
  local before_checksums after_checksums
  before_checksums="$(checksums_for "${before}" "${template}")"
  after_checksums="$(checksums_for "${after}" "${template}")"

  if [[ -z "${before_checksums}" || -z "${after_checksums}" ]]; then
    echo "MISSING: checksum annotations in ${template}" >&2
    exit 1
  fi
  if [[ "${before_checksums}" != "${after_checksums}" ]]; then
    echo "UNEXPECTED ROLLOUT: extension image changed checksums in ${template}" >&2
    exit 1
  fi
}

image_one='example.invalid/checksum-scope:one'
image_two='example.invalid/checksum-scope:two'

render_extensions "${image_one}" 'Checksum Scope' false "${tmp_dir}/extensions-before.yaml"
render_extensions "${image_two}" 'Checksum Scope' false "${tmp_dir}/extensions-after.yaml"
render_extensions "${image_one}" 'Checksum Scope Changed' false "${tmp_dir}/catalog-after.yaml"
render_extensions "${image_one}" 'Checksum Scope' true "${tmp_dir}/mariadb-before.yaml"
render_extensions "${image_two}" 'Checksum Scope' true "${tmp_dir}/mariadb-after.yaml"

assert_same_checksums "${tmp_dir}/extensions-before.yaml" "${tmp_dir}/extensions-after.yaml" deployment-api.yaml
assert_same_checksums "${tmp_dir}/extensions-before.yaml" "${tmp_dir}/extensions-after.yaml" deployment-worker.yaml
assert_same_checksums "${tmp_dir}/mariadb-before.yaml" "${tmp_dir}/mariadb-after.yaml" statefulset-mariadb.yaml
assert_same_document "${tmp_dir}/extensions-before.yaml" "${tmp_dir}/extensions-after.yaml" deployment-api.yaml
assert_same_document "${tmp_dir}/extensions-before.yaml" "${tmp_dir}/extensions-after.yaml" deployment-worker.yaml
assert_same_document "${tmp_dir}/mariadb-before.yaml" "${tmp_dir}/mariadb-after.yaml" statefulset-mariadb.yaml

before_extension_image="$(extract_document "${tmp_dir}/extensions-before.yaml" deployment-extension.yaml | awk '/^[[:space:]]+image:/ { print; exit }')"
after_extension_image="$(extract_document "${tmp_dir}/extensions-after.yaml" deployment-extension.yaml | awk '/^[[:space:]]+image:/ { print; exit }')"
if [[ -z "${before_extension_image}" || "${before_extension_image}" == "${after_extension_image}" ]]; then
  echo 'MISSING: extension image change did not alter the extension Deployment' >&2
  exit 1
fi

for template in deployment-api.yaml deployment-worker.yaml; do
  before_catalog="$(secret_checksum_for "${tmp_dir}/extensions-before.yaml" "${template}")"
  after_catalog="$(secret_checksum_for "${tmp_dir}/catalog-after.yaml" "${template}")"
  if [[ -z "${before_catalog}" || "${before_catalog}" == "${after_catalog}" ]]; then
    echo "MISSING ROLLOUT: extension catalog change did not alter ${template}" >&2
    exit 1
  fi
done

echo 'Helm checksum scope OK'
