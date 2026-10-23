# M8 — host-authorized systemd actions (data_directory, unit_file, service)

Continuation after `527805b`. The [stage grant record](M8_BINARY_STAGE_GRANT.md)
covers `artifact_directory`. This slice applies the other three systemd resources
under the same one-shot DB admission → Ed25519 grant → host verification model.
It is still not enrollment, `probe init`, starting the service, or installation success.

> **Status: implemented locally; unit file and `service` semantics still await owner
> review.** Native, SQLite and Go SSH evidence only. MariaDB, OpenSSH, real systemd
> and Linux runtime acceptance are planned on AWS (`scripts/m8_aws_acceptance.py`).

## Contract (frozen for implementers)

### Domain — `internal/core/domain/probe_host_action.go` (written)

- `ProbeHostActionIntent{Identity, OwnerID, AttemptID, Kind}`. It is valid only
  for a root systemd identity and kind `data_directory` | `unit_file` | `service`.
  `Resource()` returns the identity-derived spec.
- `Marker()` is canonical text (`phoenix-host-action-v1`, kind, reference, every
  identity field). For `unit_file` it adds `unit_sha256=` of `ProbeSystemdUnit(identity)`.
- `ProbeHostActionAdmissionRequest{Intent, Challenge}`. `SigningMessage()` uses the
  separate domain `phoenix-host-action-admission-v1\x00`, so it can never verify as
  a binary-stage grant, and vice versa. Admission and grant types mirror the
  binary-stage types.
- `ProbeSystemdUnit(identity)` is pure, deterministic unit bytes (below).

### Ports and service (written)

- `ports.ProbeHostActionAdmissionRepository.AdmitHostAction(ctx, request)`
- `ports.ProbeHostActionGrantSigner`. The existing `Ed25519ProbeStageGrantSigner`
  implements it with `SignHostActionAdmission`.
- `services.ProbeHostActionAuthorizationService.Authorize(ctx, request) (grant, error)`
  mirrors the binary-stage service exactly.

### Storage — migration `086_probe_host_action_admissions` (both engines)

A new table, `probe_host_action_admissions`, with the same columns, checks and
uniqueness as `probe_binary_stage_admissions` except
`kind IN ('data_directory','unit_file','service')`. The FK is to
`probe_install_resources (operation_id, kind)`. The down migration uses the same
non-empty guard as `085` (refuses while any admission exists). A new table avoids
rebuilding `085` on SQLite, which cannot alter CHECK constraints.

`repository.ProbeHostActionAdmissionStore.AdmitHostAction` uses the same transaction
as `AdmitBinaryStage`:
1. Operation-first locked graph load, DB clock, expiry reconciliation.
2. The current receipt must match identity/owner, be `running` and within its deadline.
3. The kind's resource must be at its layout index, with ordinal, reference and owned
   exactly as derived, `in_flight` with the intent's attempt ID.
4. Every earlier resource must be `applied`.
5. The admission stamp must precede the deadline.
6. Plain INSERT: any uniqueness collision is `ErrConflict`, including an exact replay.
7. Bump `probe_install_operations.updated_at`, read back, compare.

All failures return a zero admission. Errors are reduced to the same safe set.

### Wire — host verb `phoenix-probe-admin stage-host-action-authorized`

The prerequisites before the challenge are the same as `stage-binary-authorized`:
uid 0, trusted key, sshd host-key fingerprints, entropy. There is no `/opt` parent.

1. stdout: the same canonical challenge line.
2. stdin: an **action frame**: `PHXACT1\n`, one byte kind length N (1..32), N bytes
   kind, then the unchanged `PHXSTG1` stage header for the identity/owner/attempt.
   Decode reuses `DecodeProbeBinaryStageHeader`. The intent must be valid and must
   pin one of this host's keys, else Unauthorized.
3. stdin: the same 72-byte grant frame, verified as a `ProbeHostActionGrant`, then
   real EOF (no payload).
4. Apply the action (below). stdout: exactly
   `{"state":"host_action_applied","kind":"<kind>","marker_sha256":"<64 hex>"}` + LF.

The sender adds `SendHostAction(ctx, intent, authorizer)`, reusing the
`SendAuthorized` transport: no source/payload, the same NotStaged/Unknown boundary,
and a strict canonical receipt. The executor dispatches by kind: the artifact
executor for `artifact_directory`, the host-action path for the rest.

### One forced command: `authorized-dispatch`

A restricted key forces exactly one command, but there are two authorized verbs, so
the key is forced to the dispatcher:

```text
restrict,command="/usr/local/bin/phoenix-probe-admin authorized-dispatch" ssh-ed25519 <operator-managed-public-key>
```

`authorized-dispatch` runs `stage-binary-authorized` or `stage-host-action-authorized`
only when `SSH_ORIGINAL_COMMAND` exactly equals that verb's fixed command string. Any
other value is refused with fixed usage text (exit 2) and never echoed. This differs
from the original-command *validator* rejected earlier: the requested command grants
nothing. Each verb still requires its own verified one-shot grant. The legacy
unauthorized verbs are not reachable through the dispatcher.

### Sender and executor (implemented)

- `SSHBinaryStageSender.SendHostAction(ctx, intent, ProbeHostActionAuthorizer)` shares
  the authorized transport through a generic challenge → grant step (`grantPlan`).
  It sends the action frame and grant frame, then EOF, with no payload, and requires
  the exact canonical receipt after the challenge.
- `SSHProbeInstallExecutor` dispatches `artifact_directory` to the artifact executor
  and the other kinds to `SendHostAction`. It rebuilds each intent from a receipt whose
  whole layout equals the identity-derived one, with every predecessor applied.

### Actions (Linux, fresh-only, no adoption, no cleanup)

| Kind | Effect | Conflict |
|---|---|---|
| `data_directory` | `mkdirat` `/var/lib/uptime-phoenix-probe-<op>` mode 0700, root, under a no-follow protected walk `/`→`var`→`lib`; fstat readback | already exists |
| `unit_file` | create `/etc/systemd/system/uptime-phoenix-probe-<op>.service` with `O_CREAT\|O_EXCL\|O_NOFOLLOW`, mode 0644, exact `ProbeSystemdUnit` bytes, fsync file and directory, readback compare | already exists |
| `service` | requires the unit file to equal `ProbeSystemdUnit`. Fixed-argument `systemctl daemon-reload`, then `systemctl enable <unit>`; applied only when `systemctl is-enabled` reports `enabled`. **Not started**: `probe run` needs `probe init` + enrollment, a later step | already enabled |

`systemctl` comes from a fixed absolute candidate list (`/usr/bin/systemctl`,
`/bin/systemctl`), with an empty environment and bounded output. It is never resolved
through PATH.

### Proposed unit (`ProbeSystemdUnit`, `phoenix-systemd-unit-v1`)

The service runs as root with `CapabilityBoundingSet=CAP_NET_RAW` (ICMP monitors)
and no ambient capabilities. It gets `NoNewPrivileges`, `ProtectSystem=strict` with
write access only to its data directory, `ProtectHome`, `PrivateTmp`,
`PrivateDevices`, kernel/cgroup/clock/hostname protection, address families
INET/INET6/UNIX/NETLINK, `RestrictNamespaces`, `MemoryDenyWriteExecute`,
`SystemCallArchitectures=native`, `UMask=0077`, and `Restart=on-failure` (10 s).

The probe runs as root to avoid adding a user-creation resource to the fixed layout.
`DynamicUser=` conflicts with a fresh-only pre-created `/var/lib` directory.
**Open questions for review:**
- A dedicated static user instead, as a new layout resource and layout version?
- Docker socket monitors may need a `ReadWritePaths`/socket exception.

## Ownership for this slice

- Main owns domain/ports/service/signer (done), the host verb, Linux actions, sender
  generalization, executor dispatch, docs and final verification.
- Storage worker owns only: the two migration pairs, `internal/adapters/repository/probe_host_action_admission.go`
  and its tests.
- AWS harness worker owns only `scripts/m8_aws_acceptance.py` and
  `scripts/test_m8_aws_acceptance.py`.

## Implementation notes and fixes found by tests

- `ApplyProbeService` mutates only when the first `is-enabled` reports exactly
  `disabled` (systemctl reads unit files directly). Any other nonzero answer, such
  as a transient failure, `masked` or `not-found`, refuses as Unavailable before
  `daemon-reload`/`enable`. Exit 0 (any enabled-like state) is Conflict.
- The host-action helper re-checks cancellation immediately before the challenge,
  so a canceled helper never prompts a sender to consume an admission. Apply
  errors carrying Unknown stay Unknown even when joined with a definite sentinel.
  Both were defects found by the test agent and fixed here.
- Admission nonce uniqueness is per table. Distinct signing domains already prevent
  a grant from verifying across artifact staging and host actions, and helpers draw
  fresh entropy for every challenge.
- The MariaDB gate's `m8-stage-authorization` scope now also requires the 72
  host-action leaves (23 for `unit_file`, 23 for `service`, 22 for
  `data_directory`, 4 schema), which are independently restated in its tests.
  Because those required leaves alone exceed the full scope's 300-pass floor, the
  floor's regression test now checks that a missing required leaf still fails.

## Verification (local)

| Check | Result |
|---|---|
| Repository host-action tests on SQLite | 72 leaves pass; 72 MariaDB twins skipped (no DSN) |
| Provisioning/domain host-action tests | 80 top-level tests (584 subtests) pass, repeated ×3/×5 without flakes; ~65 overlay mutants killed or proven redundant |
| Bootstrap helper/dispatcher tests | 438 passes, zero skips after the two fixes |
| Gate/harness script tests | 44 gate + 60 AWS-harness tests pass offline |
| Build, vet, gofmt, Linux amd64/arm64 (CGO0) and Windows builds, core import check | Passed |
| Full native race suite (Darwin/arm64, no DSN, `-timeout 3600s -p 4`) | 8,522 passes, zero failures, 637 skips = 560 prior + 72 new host-action MariaDB twins (repository) + 5 known optional |

Not executed: real MariaDB/InnoDB, OpenSSH, systemd, root `/var/lib`/`/etc`, or any
Linux runtime.
