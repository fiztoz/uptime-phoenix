# M8 — intentional install cleanup

An operator-initiated, durable, one-shot removal of everything one SSH install
operation created on its host. It is the explicit answer to two acceptance cases:

- **Interrupted install.** An install interrupted part-way leaves a subset of its
  resources behind. The installer preflight then refuses every new install on that
  host with `host_existing_install`.
- **Existing install.** A host that already runs a Phoenix probe is never adopted or
  overwritten.

Cleanup removes exactly the operation's identity-derived resources and nothing else.

## Scope

| In scope | Out of scope |
|---|---|
| The four systemd resources of one install operation: unit (stopped, disabled, file removed), artifact directory, data directory | Hub-side probe registration (revoke it with the existing probe admin API) |
| Partially created installs: an absent resource counts as removed | Hosts or operations the hub has no journal row for: manual cleanup only |
| A refused or never-started cleanup may be retried with a new cleanup ID | Any path not derived from the operation identity |

## Contract

### Domain (`internal/core/domain/probe_install_cleanup.go`)

- Host-action kind `cleanup` (`ProbeHostActionCleanup`).
  - `ProbeHostActionIntent.Valid` accepts it.
  - `Resource()` returns `{Ordinal: 1, Kind: "cleanup", Reference: "uptime-phoenix-probe-<op>", Owned: true}`.
  - The grant marker therefore binds the operation's whole resource family.
- `ProbeCleanupLifetime = 5 * time.Minute`.
- `ProbeCleanup{Identity ProbeCleanupIdentity{CleanupID, Install, ClientKeyFingerprint, RequestedBy}, OwnerID, State, ErrorCode, AttemptID, CreatedAt, UpdatedAt, DeadlineAt}`.
  `Valid()` enforces `DeadlineAt = CreatedAt + lifetime` and this table:

| State | Error code | Attempt | Meaning |
|---|---|---|---|
| `queued` | — | none | admitted, not begun |
| `running` | — | set | attempt reserved, helper may run |
| `cleaned` | — | set | helper verified every resource absent |
| `failed` | `not_started` | set | nothing reached the helper (definite) |
| `interrupted` | `owner_interrupted` | none | abandoned before it began |
| `interrupted` | `deadline_expired` | any | hard expiry reconciled by storage |
| `interrupted` | `outcome_unknown` | set | the helper may have removed some resources |

  A helper refusal after its grant (for example a modified unit file) reaches the hub
  as `outcome_unknown`, because the sender cannot tell it apart from a partial effect.

### Eligibility (enforced in the database, under the install operation row lock)

1. The install operation exists, and the cleanup's `Install` identity equals it exactly.
2. The install state is `resources_ready`, `failed`, `canceled` or `interrupted`: no
   install runner work remains.
3. No activation row exists, or the activation is terminal (`active`, `failed`,
   `canceled` or `interrupted`).
4. No other cleanup for the operation is nonterminal, and none is `cleaned`.

Once any cleanup row exists for an install, `CreateActivation` refuses with a
conflict.

### Persistence (migration `088`, both engines)

Two tables:
- `probe_install_cleanups`:
  - PK `cleanup_id`; FK `operation_id` to the install operation (restrict).
  - Columns: client key fingerprint, actor, owner, state, error, attempt and DB-clock
    stamps. The install identity is read from the install row, not copied.
  - `UNIQUE (attempt_id)`, `UNIQUE (cleanup_id, attempt_id, owner_id)` and an index on
    `(operation_id, created_at, cleanup_id)` for `Latest`.
  - CHECKs encode `ProbeCleanup.Valid()`. On MariaDB they include the exact 5-minute
    lifetime. SQLite cannot do that microsecond arithmetic on text stamps, so there
    the store's receipt validation enforces it.
- `probe_cleanup_admissions`:
  - PK `attempt_id`, plus `UNIQUE (cleanup_id)` and
    `UNIQUE (authority_key_id, nonce_hex)`.
  - Composite FK `(cleanup_id, attempt_id, owner_id)` to the cleanup.

Behaviour that is easy to miss:
- **Locking.** Every transition resolves `cleanup_id → operation_id` with an unlocked
  read, then locks the install operation row first. This avoids shared-to-exclusive
  deadlocks under MariaDB SERIALIZABLE.
- **Create reconciles first.** Create reconciles an expired install, an expired
  activation and expired cleanups, and those reconciliations commit even when the
  create is refused.
- **Ordering.** Creation stamps are clamped to the newest stored cleanup stamp, so
  `Latest` stays chronological across a DB clock rollback.
- **Late outcome.** `RecordCleanup` past the deadline returns the
  `interrupted/deadline_expired` receipt with a nil error. Callers must read the
  state, not just the error.
- **Activation lock-out.** `CreateActivation` refuses with a conflict once any cleanup
  row exists for the install.

Every transition samples the DB clock after the lock and requires the stored owner
and an unexpired deadline. These are the same fencing rules as 084 and 087.
The port is `ports.ProbeCleanupRepository`: `CreateCleanup`, `ObserveCleanup`,
`LatestCleanup`, `BeginCleanup`, `RecordCleanup` and `AbandonCleanup`. The admission
port is `ports.ProbeCleanupAdmissionRepository.AdmitCleanupAction`.

### Host action (`ApplyProbeCleanup`, Linux helper)

Refusals happen before any effect (definite helper refusal; the hub records `outcome_unknown`):
- the unit file is present but not exactly `ProbeSystemdUnit(identity)`;
- the artifact directory holds anything other than the pinned release file and its
  stage marker;
- a present resource is not owned by root, or is not of its expected type.

Effects, in order. Every failure from here on is Unknown:
1. If the unit file is present: `systemctl disable --now <unit>`, remove the unit
   file, sync the directory, `systemctl daemon-reload`, then
   `systemctl reset-failed <unit>` (whose result is ignored).
2. Remove the release file and the marker, then rmdir the artifact directory.
3. Remove the data directory tree without following links.
4. Verify that all three are absent.

When everything is already absent, the result is applied.

### Service and HTTP

- `POST /api/probe-installs/:operation_id/cleanup` with `{"cleanup_id", "private_key"}`
  (strict JSON) returns 202 with a cleanup view.
- `GET /api/probe-installs/:operation_id/cleanup` returns the latest cleanup.
- Both are admin only and behind `PROBE_SSH_INSTALL_ENABLED`.
- The deployment key is transient. The host key is the install's pinned fingerprint.
- View: `{cleanup_id, operation_id, status, created_at, updated_at, deadline_at, error}`.
  It never includes paths, keys or output.

## Implementation status and verification

| Part | Where |
|---|---|
| Domain and ports | `probe_install_cleanup.go` (domain, ports); `ProbeHostActionIntent` accepts `cleanup` |
| Store | `repository.ProbeInstallCleanupStore` (088, both engines) |
| Host action | `provisioning.ApplyProbeCleanup`, wired in `applyHostProbeAction` (Linux) |
| Grants | `services.NewProbeCleanupActionAuthorizationService` |
| SSH session | `provisioning.SSHProbeCleanupSessionFactory` |
| Service | `services.ProbeCleanupOperationService`: Begin → host action → Record; Abandon if Begin is unconfirmed |
| HTTP | `handlers.ProbeCleanupHandlers`; routes registered on the install group |
| Composition | `newProbeCleanupOperations` (API-local; workers never run it) |

Verification. Executed locally unless noted:
- **MariaDB.** `TestM8InstallCleanup/{sqlite,mariadb}/…`: 77 MariaDB leaves, gate set
  `CLEANUP_REQUIRED`. They are in the `m8-stage-authorization` scope, which now
  requires 292 leaves; its `go test` timeout was raised to 1800 s.
- **Host action.** `TestApplyProbeCleanup*` checks real filesystem effects on darwin
  with a fake systemctl. Covered cases:
  - symlink and hard-link escapes;
  - foreign artifact content and a modified unit;
  - partial installs;
  - walk bounds;
  - disable and reload failures.
- **Service and handlers.** `TestProbeCleanupOperationService*` and
  `TestProbeCleanupHTTP*`.
- **End to end.** `TestProbeInstallAPICleanupRemovesTheInstallAndFreesTheHost` runs
  through the production composition and a loopback SSH helper:
  1. install, then cleanup;
  2. all resources are gone and the unit was disabled first;
  3. an exact retry returns the settled receipt;
  4. a second cleanup is refused (`cleanup_ineligible`), and so is activation;
  5. a new install on the same host passes the preflight.
- **Real hosts.** Real systemd and root effects on Linux passed in AWS run
  `398107b05ed2` on arm64 and amd64. Cleanup of an active probe, of an interrupted
  install and of a failed activation each left 0 resources, an inactive unit and no
  listener.
