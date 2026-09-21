# M3 hub stream reset and operator recovery

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Date: 2026-09-21. Baseline: `ba997db`. Codex owns implementation, verification
and commits. Antigravity supplied a read-only advisory review and owns no files.
No push or deployment. This checkpoint does not complete M3.

## Implemented behavior

Hub migration 064 retains immutable reset identity, original cursor and recovery
phase separately from enrollment. Preparation serializes with probe authority,
reserves a new epoch and revokes both parent runtime and connector leases.
Prepared operations block runtime discovery/acquisition/renewal, replay and new
commands. Existing stream history and cursor remain untouched.

An exact source receipt activates the new epoch transactionally. Activation
authenticates and reseals the same runtime token, retires the old stream without
deleting its history, starts the new cursor at zero and invalidates current and
auxiliary state. Active assignments receive an explicit `stream_reset` UNKNOWN
marker, without a fabricated observation, recovery or provider intent.

Old pending commands are canceled locally with `stream_reset_unconfirmed` while
retaining their original protected body and stream. Their remote outcome remains
unknown. `LocalCancellationCode` is separate from the durable source `Outcome`;
the operator view never presents local cancellation as source confirmation.

Activation reports `awaiting_peer`. The connector confirms completion only from
its established callback after authenticated source health and credential/pin
confirmation. New command/rotation issuance waits for this confirmation. Exact
retries recover the original plan, source receipt and phase; altered identities,
reused epochs, overlapping resets and unresolved rotations fail.

The stopped-source CLI validates the retained key, configuration and TLS identity
even with a pending reset reservation. Its certificate read deliberately performs
no overlap housekeeping on a recovery handle. Runtime writes remain prohibited.

## Operator flow

Stop the original source owner. Keep its identity, protection key and archives.
Use private mode-0600 files, for example by setting `umask 077` first. Choose and
retain explicit reset/new-stream UUIDs; never regenerate them for a retry.

```sh
phoenix-probe-admin prepare-reset --probe-id PROBE_UUID --reset-id RESET_UUID \
  --previous-stream-id OLD_STREAM_UUID --stream-id NEW_STREAM_UUID > plan.json
probe reset-stream --data-dir EDGE_DIR --plan-file plan.json > source-receipt.json
phoenix-probe-admin activate-reset --probe-id PROBE_UUID --reset-id RESET_UUID \
  --receipt-file source-receipt.json
phoenix-probe-admin reset-status --probe-id PROBE_UUID --reset-id RESET_UUID
```

Restart the edge after its reset. The hub may be activated before that restart or
after the source has restarted; `awaiting_peer` stays honest until actual new-stream
admission. A lost output is recovered by repeating the exact command. A pending
source reservation blocks ordinary startup; retry `reset-stream` with the original
plan after correcting the archive/storage failure. Never remove retained evidence
or manufacture a new bootstrap identity to bypass the failure.

The plan/receipt are explicit metadata DTOs, not bearer credentials or signed hub
messages. Local source and hub operators authorize their respective mutations.
Receipt metadata is an operator statement, not proof of a live peer. Unknown work
after restoring an old backup remains unknown coverage; no loss range is invented.
An unreachable copied edge cannot be stopped by a hub database lease. Running
cloned identities remains unsupported. Complete key loss requires verified
reenrollment, not this reset flow.

## Verification ledger

The final CGO-free build, full repository race suite and zero-issue lint passed.
The suite recorded 3,693 named passes in 22 packages, zero failures and two
existing optional skips. All 315 MariaDB-named cases passed, including 302
explicitly audited live-engine cases, with no engine skips. All 38 reset/codec/CLI
cases ran. The 28 source/harness hashes stayed unchanged during this final gate.
See [machine-readable evidence](M3_HUB_RESET_EVIDENCE.json).

Targeted race tests currently pass: both database engines preserve evidence,
reject stale owners and wrong keys, enforce immutable concurrent preparation,
roll back late preparation/activation failures, guard populated downgrade and
reject unresolved credential/certificate rotations. Closed codec cases preserve
signed-64-bit counters and reject ambiguous metadata. The source CLI recovers an
archive failure plus lost committed output, and real TLS tests retain bootstrap
and rotated pins across reset/restart.

The first compiled-process attempt failed because the Python harness retained
SQLite diagnostic readers. Explicitly closing them fixes the harness; the exact
same plan succeeded once the failed harness exited. Keep this failure distinct
from a clean rerun. A second run caught archive inspection creating sidecars.
After its immutable-read correction, the third fresh-database compiled run passed
all 30 stages: source-before-hub and hub-before-source restart recovery, retained
archive, unchanged bootstrap/key/pin and independent old/new sequence one. The
migration-rehearsal repair also passed focused both-engine tests and this final
full gate. See [retrospective](../postmortems/2026-09-21-m3-integration.md#reset-and-archive-durability).

```text
Commands executed: CGO_ENABLED=0 go build ./...; go test -race -count=1 -timeout=20m -json ./...; golangci-lint run; compiled probe_runtime_smoke.py --verify-replay --verify-stream-reset.
Engines and named tests exercised: SQLite and disposable MariaDB; reset storage, rotations, codec, CLI, TLS and complete repository suite.
Passed / failed / skipped: 3,693 named passes, zero final failures, two optional skips; 30 process stages passed. Earlier failures retained in evidence.
Acceptance criteria still unverified: bounded shutdown/pressure completion, actual fifteen-minute partition, whole-M3 gate and requirement audit.
```

Remaining M3 work: pressure diagnostics and bounded shutdown flushing, the actual
15-minute partition acceptance, then a complete requirement audit and repository
gate under [subsequent acceptance](M3_COMPLETION_ACCEPTANCE.md).
