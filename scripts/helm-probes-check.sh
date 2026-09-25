#!/usr/bin/env bash
# ─── Phoenix — remote-probe chart assertions ──────────────────────────────────
# Asserts the probes feature flag renders exactly what it claims:
#
#   * default renders (single pod and split worker) contain NO probe wiring, so
#     the pre-probe topology — single-pod defaults and split-image behavior —
#     is preserved;
#   * probes.enabled=true without an installation-key Secret reference fails the
#     render loudly instead of shipping a pod that cannot start
#     (PROBES_ENABLED without PROBE_SECRET_KEY_FILE is a startup error);
#   * probes.enabled=true with the reference wires the WORKER role only (the
#     all-in-one pod is its own worker): the flag, the key file path and the
#     Secret mount. The API tier never receives the key.
#
# Wired into `make helm-validate` (and therefore CI). Run helm lint first.
set -euo pipefail

chart=charts/uptime-phoenix
release=uptime-phoenix
fail() { echo "helm-probes-check: $*" >&2; exit 1; }

render() { helm template "$release" "$chart" "$@"; }

echo "==> probes defaults render no probe wiring (single pod + split worker)"
# shellcheck disable=SC2086
for mode_args in "" "--set database.engine=mariadb --set mariadb.enabled=true --set mode=worker"; do
  out=$(render ${mode_args})
  if grep -q "PROBES_ENABLED\|PROBE_SECRET_KEY_FILE\|probe-key" <<<"$out"; then
    fail "probe wiring rendered while probes.enabled=false (${mode_args:-single pod})"
  fi
done

echo "==> probes.enabled=true without a secret reference fails the render"
if render --set probes.enabled=true >/dev/null 2>&1; then
  fail "probes.enabled=true rendered without probes.secretName"
fi

echo "==> probes.enabled=true wires the worker role from the named Secret"
out=$(render --set probes.enabled=true --set probes.secretName=phx-probe-key)
for needle in \
  'name: PROBES_ENABLED' \
  'name: PROBE_SECRET_KEY_FILE' \
  'value: "/etc/uptime-phoenix/probe-key/installation-key"' \
  'mountPath: /etc/uptime-phoenix/probe-key' \
  'secretName: phx-probe-key' \
  'readOnly: true'; do
  grep -q "$needle" <<<"$out" || fail "enabled render is missing: $needle"
done

echo "==> the API tier never receives probe wiring"
out=$(render --set mode=api --set probes.enabled=true --set probes.secretName=phx-probe-key)
if grep -q "PROBES_ENABLED\|PROBE_SECRET_KEY_FILE\|probe-key" <<<"$out"; then
  fail "probe wiring rendered for the API tier"
fi

echo "==> custom secret keys become the mounted file name"
out=$(render --set mode=worker --set database.engine=mariadb --set mariadb.enabled=true \
  --set probes.enabled=true --set probes.secretName=phx-probe-key --set probes.secretKey=my-key)
grep -q 'value: "/etc/uptime-phoenix/probe-key/my-key"' <<<"$out" || fail "custom probes.secretKey not mounted"
grep -q 'key: my-key' <<<"$out" || fail "custom probes.secretKey not read from the Secret"

echo "helm-probes-check: ok"
