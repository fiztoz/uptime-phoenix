# M4 deployment compatibility

The operator deployment documentation and the Helm chart now carry the
remote-probe feature flag, configuration and secret references. This is the
"update operator deployment documentation and Helm feature flag/config/secret
references" bullet of [M4](IMPLEMENTATION_PLAN.md). Single-pod defaults and
split-image behavior are preserved: with the flag off, the rendered manifests
are exactly the pre-probe deployment.

## Helm

| Value | Default | Effect |
|---|---|---|
| `probes.enabled` | `false` | Renders `PROBES_ENABLED=true` on the **worker role** only — the `mode=all` Deployment or `uptime-phoenix-worker`. Off by default renders **no** probe wiring at all |
| `probes.secretName` | `""` | Existing Secret holding the installation key. **Required** when enabled: the render fails loudly without it, because the process refuses to boot with `PROBES_ENABLED` and no `PROBE_SECRET_KEY_FILE` |
| `probes.secretKey` | `installation-key` | Key inside that Secret; becomes the mounted file name at `/etc/uptime-phoenix/probe-key/` |

The key is a read-only Secret mount; the chart never generates, stores or
rotates it ([key provisioning](KEY_PROVISIONING.md) owns that). The API tier
never receives the flag or the key — connector workers are a worker-role
capability. No workload is added or removed: the remote probe itself remains an
operator-deployed process ([operator guide](M2_OPERATOR_GUIDE.md)), exactly as
in V1.

Topology preservation:

- `mode=all` (single pod, the default) renders the same manifests as before,
  plus nothing;
- `mode=api` / `worker` / `split` keep the split-image wiring
  (`Dockerfile.split` api/worker/web images) unchanged; the flag attaches to
  the worker Deployment in every mode;
- turning the flag off again removes every trace of probe wiring (no orphan
  volumes or env).

## Operator documentation

- `docs/DEPLOYMENT_MODES.md` — "Remote probes (multi-region) in any mode":
  secret creation, the enable command, what renders in each mode, and the
  pointer to registration/enrollment and recovery.
- `docs/RUNBOOK.md` — §5 environment reference documents `PROBES_ENABLED` and
  `PROBE_SECRET_KEY_FILE` with their production rules; §1 (configuration
  backup and restore drill) now covers multi-region restores: version 2
  backups carry probe identity and assignments, restored identities are
  disabled pending reenrollment, and a restored hub database must follow the
  [lifecycle/recovery](M4_LIFECYCLE_RECOVERY.md) procedure (including
  re-running clear history for monitors cleared after the backup's timestamp).
- `charts/uptime-phoenix/README.md` — the three values, a worked example, and
  the failure mode of enabling without a secret reference.

## Verification

Executed in this checkout:

- `helm lint charts/uptime-phoenix` — clean.
- `make helm-validate` — full render matrix passes, now including
  `scripts/helm-probes-check.sh`, which asserts **both directions**:
  - default renders (single pod and split worker) contain no probe wiring;
  - `probes.enabled=true` without `probes.secretName` fails the render;
  - `probes.enabled=true` with the reference renders the flag, the key file
    path, the read-only mount and the named Secret;
  - the API tier renders no probe wiring even when the flag is on;
  - a custom `probes.secretKey` becomes the mounted file name and the Secret
    key read.
- No Go sources changed in this slice; the repo-wide Go gates from the
  previous slices are unaffected.

Not run: a live-cluster install. The local Kubernetes context (`colima-k8s`)
is not running and no release image is published yet (first publish is an
owner action), so the on-cluster steps of the Helm topology verification —
Pod boot with the mounted key, connector workers attaching, `helm upgrade`
idempotence — remain unverified and are recorded as such. The assertions that
do not need a cluster (render matrix, negative renders, secret-reference
shape) are in the repeatable gate and run in CI. Local acceptance does not
authorize push, deployment, or a production migration.
