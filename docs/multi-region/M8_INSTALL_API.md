# M8 — admin installer API (asynchronous SSH systemd install)

Continuation after `0f71d1f`. The systemd install path (journal → runner →
`SSHProbeInstallExecutor` → authorized helpers) is accepted on real hosts
([host actions](M8_HOST_ACTIONS.md)) but has no hub caller. This slice adds the
admin-only asynchronous routes and the API-local service that start it. It is
**not** enrollment, `probe init`, config activation, service start, the Docker path
or installation success. `resources_ready` is the best outcome and means "all
four systemd resources applied", not "probe installed and running".

## Frozen contract

### Configuration (`internal/bootstrap/config.go`)

| Variable | Default | Rule |
|---|---|---|
| `PROBE_SSH_INSTALL_ENABLED` | `false` | requires `PROBES_ENABLED=true` |
| `PROBE_STAGE_AUTHORITY_KEY_FILE` | — | required when enabled; loaded with `LoadProbeStageGrantSigner` at startup (fail fast) |
| `PROBE_INSTALL_ARTIFACT_DIR` | — | required when enabled; existing directory holding operator-placed release files named `ProbeBinaryPin.ReleaseName()` |

Endpoint policy reuses `PROBE_ENDPOINT_POLICY_FILE`. Worker-mode processes never
accept or run installs. The default deployment loads nothing new.

### Domain (`internal/core/domain/probe_install_submission.go`)

```go
type ProbeInstallSubmission struct {
    OperationID        string
    Host               ProbeSSHHost   // User must be "root"
    HostKeyFingerprint string
    PrivateKey         []byte         // transient, ≤ 64 KiB, never stored
    Binary             ProbeBinaryPin // operator pin; mode is systemd
    RequestedBy        int64          // from auth context only
}
var ErrProbeInstallBusy, ErrProbeInstallClosed, ErrProbeInstallArtifact // sentinels
```

### Port (`internal/core/ports/probe_install_session.go`)

```go
// ProbeInstallSessionFactory parses one transient key, verifies the pinned
// artifact from the operator directory, and returns a one-operation executor
// plus the key's PUBLIC fingerprint. Nothing durable is created.
type ProbeInstallSessionFactory interface {
    NewInstallSession(ctx context.Context, privateKey []byte, hostFingerprint string,
        pin domain.ProbeBinaryPin) (ProbeInstallSession, string, error)
}
type ProbeInstallSession interface {
    ProbeInstallResourceExecutor
    Close(ctx context.Context) error // disposes the prepared snapshot; idempotent
}
```

`provisioning.SSHProbeInstallSessionFactory` implements it from the existing sender,
preparer and `SSHProbeInstallExecutor`. The two authorization services are injected
at composition. The artifact is opened as `<dir>/<ReleaseName>` with a no-follow
open of a single path component, prepared and verified against the pin (Go/ELF
platform, size, SHA-256) before returning. Errors are reduced to fixed sentinels.

### Service (`internal/core/services/probe_install_operation.go`)

`ProbeInstallOperationService` follows `ProbePreflightOperationService`:

- `NewProbeInstallOperationService(ctx, journal, runnerFactory, sessions, ownerID, capacity)`.
  The journal (Create/Observe/Cancel) and the runner are small consumer interfaces.
  `capacity` must be within 1–4 (composition passes 2).
- **Submit** steps:
  1. Validate. The user must be root, the pin valid, the key 1..64 KiB and
     `RequestedBy > 0`.
  2. Reserve a slot.
  3. Create the session; artifact or key failures happen here, before any row exists.
  4. Build the identity: systemd mode, layout 1, the public client fingerprint.
  5. `journal.Observe(id)`. An existing operation is answered here: the stored
     receipt for an exact retry (never re-preflighted, never redispatched), or a
     conflict for a changed identity. Rerun behaviour is therefore explicit:
     the same operation ID always returns its receipt; a new install on the same
     host is a new operation ID and must pass the preflight.
  6. **Host preflight** (`session.Preflight`, see below). A refusal returns a fixed
     code and creates nothing: no row, no host change.
  7. `journal.Create(identity, ownerID)`.
  8. On `created=true`, dispatch one goroutine: `Run(rootCtx, identity, owner,
     fresh attempt UUIDs)`, close the session, release the slot.
  9. On `created=false` (a concurrent identical submit committed first), close the
     session and return the stored receipt with no dispatch.
  10. When saturated, only an exact retry of committed work may answer; anything
      else is Busy.

  The whole admission is bounded by 90 s (`probeInstallSubmitTTL`), covering the
  artifact preparation, the lookup and the preflight with one host-key-family retry.

### Host preflight gate

Before anything is created, the installer runs the fixed command
`/usr/local/bin/phoenix-probe-admin install-preflight` over the pinned SSH
transport with the deployment key. `authorized-dispatch` routes it by exact
`SSH_ORIGINAL_COMMAND` match, like the two authorized verbs. It needs no grant: it
reads no input, changes nothing and prints one canonical report:

```
phoenix-install-preflight-v1
kernel=Linux
machine=x86_64
systemd=1
docker=0
root=1
authority=1
existing=0
```

- `systemd` means `/run/systemd/system` is a directory and `systemctl` is present.
- `authority` means the protected stage-authority loader accepts the key; the
  authorized helpers would accept it too.
- `existing` counts `uptime-phoenix-probe-*` entries under `/var/lib`, `/opt` and
  `/etc/systemd/system`, capped at 64.

`ProbeInstallHostReport.Refusal` checks in a fixed order, so the most fundamental
mismatch is the one reported. Every refusal is HTTP **422** with one of these codes:

| Code | Cause |
|---|---|
| `host_trust_failed` | no presented host key matches the pin (nothing authenticated) |
| `host_destination_refused` | endpoint policy refused the resolved address |
| `host_auth_failed` | host verified, then root refused the deployment key (missing root access) |
| `host_unreachable` | TCP connection or handshake failed before host verification |
| `host_helper_unavailable` | helper absent, nonzero exit, or a noncanonical or oversized report |
| `host_unsupported_os` | kernel is not Linux |
| `host_architecture_mismatch` | `machine` does not map (`x86_64`→amd64, `aarch64`→arm64) to the pin |
| `host_root_required` | helper's effective uid is not 0 |
| `host_systemd_unavailable` | systemd mode and the host is not booted with systemd |
| `host_authority_missing` | no protected trusted stage-authority key |
| `host_existing_install` | any Phoenix probe entry exists, including leftovers of an interrupted install |

An existing install is never adopted or overwritten. A second probe would contend
for the listener. Leftovers are removed by an intentional cleanup.

The preflight is not a host lock: two different operations submitted for one host
at the same instant can both pass it. The later install then fails honestly at its
own resources, or at activation readiness.
- **Observe:** `journal.Observe`; cold expiry reconciles in storage.
- **Cancel:** reads first, and returns a `resources_ready` receipt unchanged: it has
  no remaining installer work, and the journal would otherwise demote it to a
  `cancel_requested` that nothing finishes. Any other state goes to `journal.Cancel`;
  the runner's watcher observes it. This is never a claim that remote work stopped.
  A run reaching `resources_ready` between the read and the cancel ends `interrupted`
  at its deadline.
- **Close:** stops admission, cancels the root context and joins every task.

No key, signer or prepared snapshot outlives its task. No DB transaction spans SSH.

### HTTP (`internal/adapters/http/handlers/probe_install.go`)

Routes are always registered. They use session-or-write-API-key, then `RequireAdmin`,
and responses are `Cache-Control: no-store`.
- `POST /api/probe-installs`
- `GET /api/probe-installs/:operation_id`
- `POST /api/probe-installs/:operation_id/cancel`

POST takes one strict JSON object (≤ 128 KiB) that refuses unknown, duplicate,
case-variant or null keys and trailing JSON.

An optional `mode` selects `systemd` (the default) or `docker`. A Docker install
replaces the binary members with these, and any `binary_*` member is refused:
- strings `image_repository`, `image_version`, `image_architecture`,
  `image_index_sha256`;
- integer `image_index_size`.

See [Docker install](M8_DOCKER_INSTALL.md). The systemd members are:
- strings: `operation_id`, `ssh_address`, `ssh_user`, `host_key_fingerprint`,
  `private_key`, `binary_version`, `binary_architecture`, `binary_sha256`;
- integer: `binary_size`.

The actor comes from the auth context.

View (never a domain type):

```json
{"operation_id":"…","kind":"ssh_probe_install","mode":"systemd","status":"running",
 "created_at":"…","updated_at":"…","deadline_at":"…",
 "binary":{"version":"…","architecture":"arm64"},
 "resources":[{"kind":"data_directory","state":"applied","error_code":null,
               "started_at":"…","finished_at":"…"}, …],
 "failed_phase":null,
 "error":null}
```

### Progress, output and redaction

The receipt is the progress stream; clients poll GET.
- **Per step.** Each resource (and each activation step) carries its state, a fixed
  error code, and DB-clock `started_at`/`finished_at` (null until reserved and until
  settled).
- **Failed phase.** `failed_phase` names the first step whose attempt did not apply,
  for `failed`, `canceled` and `interrupted` receipts only.
- **Exit status.** It is the step state: `applied`, `not_started` (definitely
  nothing reached the host), or `unknown`.
- **Cancellation.** The cancel routes.
- **Retention** is bounded by construction: the step layout is fixed and each step has
  exactly one immutable attempt.

Remote stdout and stderr are **never captured, stored or returned**. Redaction holds by
construction, not by filtering. Helpers answer with one canonical receipt line; the
SSH layer discards stderr and bounds stdout. The enrollment token exists only in the
activation executor's memory and never reaches a view, a log or the database.

`status` is the journal state (`queued`, `running`, `cancel_requested`,
`resources_ready`, `failed`, `canceled`, `interrupted`). `error` is a fixed
allowlisted `{code,message}`; unknown stored codes fail closed with 503. Selectors,
fingerprints, actor/owner IDs, attempt IDs, keys and raw output are never returned.

| Case | Status |
|---|---|
| New or in-progress submission (after commit) | 202 |
| Exact retry of a terminal receipt; GET; cancel of a terminal receipt | 200 |
| Cancel accepted | 202 |
| Malformed body/ID/key/fingerprint/pin, non-root user | 400 |
| Body over limit | 413 |
| Pinned artifact missing or failing verification | 422 `artifact_rejected` |
| Changed identity | 409 |
| Unknown operation | 404 |
| Disabled, closed, busy (with `Retry-After`) or corrupt receipt | 503 |

## Ownership

- Main: domain, port, service, provisioning factory, config, composition, router
  wiring, docs and final verification.
- Handler worker: only `handlers/probe_install.go` and `handlers/probe_install_test.go`.
- Service-test worker: only `services/probe_install_operation_test.go`.
- Integration worker (after the above): only
  `internal/bootstrap/probe_install_api_integration_test.go` (router + real service +
  SQLite + Go SSH server running the real authorized helpers).

## Implementation status and verification (local)

Implemented as specified. The end-to-end composition test runs:
- the real router with real admin auth (JWT and API key);
- the real handler, service, session factory, runner and journal;
- both admission stores and the Ed25519 signer on SQLite;
- a Go SSH server that dispatches the two real authorized helpers, applying effects
  under temp parents with a fake systemctl.

Observed end states:

| Scenario | Result |
|---|---|
| Happy path | 202, then `resources_ready` with all four resources `applied`; exact files and modes; four execs; 1 stage and 3 host-action admissions; no key, address or fingerprint in any response or in SQLite |
| Exact retry | 200, byte-identical receipt; no new SSH connection, row or systemctl call |
| Changed pin, host, host pin, key or actor | 409 `install_conflict`; nothing executed |
| Missing, truncated, mismatched, escaping-symlink or directory artifact | 422 `artifact_rejected`; nothing durable (GET 404); the same ID installs later |
| Cancel while the stage helper waits before its challenge | `canceled`: `data_directory` applied, `artifact_directory` not_started, the rest planned; no stage admission consumed |
| Cancel while a host action is in flight | `interrupted` / `outcome_unknown` with `data_directory` unknown. The host still creates the directory when released, so cancel is intent, not proof of remote stop |
| `Close` with a held action | returns with the receipt already `interrupted`; later submits get 503 `installs_closed`; reads still work |
| Cancel of `resources_ready` | 200, unchanged (after the fix above; first reported by the integration test) |
| Auth gates | 401 unauthenticated or with a read-scope key; 403 non-admin; typed 503 when disabled or unwired |
| Saturation (capacity 2) | 503 `installs_busy` with `Retry-After`; exact retries still answer |

Unit coverage:
- handler: 365 tests, 5 deliberate breaks caught;
- service: 147 tests, all 25 compiling mutants killed;
- session factory and config tests.

Full native race suite (Darwin/arm64, no DSN): 9,058 passes, zero failures, the same
637 audited skips (632 missing-DSN repository, five known optional). Build, vet,
gofmt and Linux amd64/arm64 builds passed.

Not covered: MariaDB (the journal and admission stores are unchanged and were
accepted on real MariaDB in run `b6ffc9ac26ab`), real sshd/systemd for this API
layer, enrollment and service start. A request refused as busy still pays for key
parsing and artifact preparation (admin-only, capacity 2).
