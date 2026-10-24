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
  5. `journal.Create(identity, ownerID)`.
  6. On `created=true`, dispatch one goroutine: `Run(rootCtx, identity, owner,
     fresh attempt UUIDs)`, close the session, release the slot.
  7. On `created=false`, close the session and return the stored receipt with no
     dispatch. A changed identity conflicts (journal).
  8. When saturated, only an exact retry of committed work may answer; anything else
     is Busy.
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
case-variant or null keys and trailing JSON:
- strings: `operation_id`, `ssh_address`, `ssh_user`, `host_key_fingerprint`,
  `private_key`, `binary_version`, `binary_architecture`, `binary_sha256`;
- integer: `binary_size`.

The actor comes from the auth context.

View (never a domain type):

```json
{"operation_id":"…","kind":"ssh_probe_install","mode":"systemd","status":"running",
 "created_at":"…","updated_at":"…","deadline_at":"…",
 "binary":{"version":"…","architecture":"arm64"},
 "resources":[{"kind":"data_directory","state":"applied","error_code":null}, …],
 "error":null}
```

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
