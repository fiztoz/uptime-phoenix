# M3 edge watchdog source persistence acceptance

This increment follows runtime ownership commit `1396ebf`. It adds the edge
source transaction needed by a connection watchdog. It does not enable a
watchdog, and M3 remains incomplete.

## Implemented effects

Edge migration 005 represents a watchdog as a probe entity: incident and delivery
monitor/generation fields are SQL NULL. Existing regional incidents, delivery
leases and exact telemetry bytes survive the transactional upgrade. The downgrade
refuses to discard any checkpoint, watchdog incident/event or new ACK metadata.

`edge.Store.CommitWatchdog` checks installation/probe/stream, accepted config
revision, optional health generation and expected state version inside the
serialized writer transaction. It commits measured loss checkpoint, source
incident, exact `watchdog.transition` bytes, source sequence and provider intents
together. Failed encoding, a late counter/checkpoint write or duplicate delivery
ID commits nothing. A checkpoint or resend allocates no incident telemetry;
resends retain the exact incident/version/sequence. Existing bounded provider
queue and leased outcome machinery are reused.

The source keeps firing/acked/resolved identity and recorded ACK metadata through
restart. A resolved identity deliberately remains referenced while unarmed; it is
not an open incident. Re-enable can open a new UUID, but cannot reopen the old one.
Recovery preserves a committed ACK and cannot invent one. Only a newly committed
recovery can enqueue its UP delivery; a later healthy checkpoint cannot repeat it.
This preserves source wall times through a backward clock correction without
using those times to calculate watchdog loss or stabilization.

## Verification

Focused domain and real-edge SQLite race tests passed. They assert:

- One winner under competing initial commits; stale version and health generation
  produce no changes.
- Rollback at incident, exact telemetry, counter, delivery and checkpoint writes.
- Restart while firing and ACKed, backward-wall-clock recovery, null probe scope,
  two claimable source intents and sequenced delivery outcomes.
- Duplicate resend ID rollback; a new resend ID retains source transition identity.
- Disable/unarmed/re-enable/new outage without reopening the old UUID or paging
  merely because settings changed.
- Migration failure rollback, preservation of existing availability telemetry and
  in-flight claim identity, and an empty `PRAGMA foreign_key_check` result.

The final whole-project race gate passed with 21 test-bearing packages, 202
MariaDB-named pass events and zero MariaDB skips. CGO-free `go build ./...` and
zero-issue `golangci-lint run` passed. Gate logs and source hashes are recorded in
[M3_WATCHDOG_SOURCE_EVIDENCE.json](M3_WATCHDOG_SOURCE_EVIDENCE.json). No frontend,
Helm, dependency, monitor type or notification provider changed. No process-level
watchdog acceptance is claimed.

## Review and limits

Antigravity performed a bounded no-tools audit in conversation
`c5f163cb-aa28-4b9d-a788-27af6ded5a13`. Its rearm and duplicate-delivery test
suggestions were added and verified by Codex. Its proposed data/identity concerns
were not reproducible defects under the actual transaction and foreign-key
invariants. Codex independently reproduced and fixed two lifecycle validation
bugs. See [the retrospective](M3_WATCHDOG_SOURCE_RETROSPECTIVE.md).

The enabled-watchdog decoder guard remains. Source storage is not a running
watchdog: hub source ownership/storage, mirrored watchdog authorization, complete
settings and dependency snapshots, real health callbacks and both-side provider
reconciliation still need implementation. The port interface is deferred until
there is a second adapter or a service test double that needs it. ACK persistence
here is a storage primitive, not the durable command/offline ACK workflow.
The storage primitive checks revision and lifecycle; it does not decode the
protected policy or channel membership. The source service must evaluate those
settings, and the dispatcher must revalidate them immediately before provider I/O.
Commands, rotations/reset, cleanup, bounded flushing and the real fifteen-minute
partition remain separate M3 requirements. Nothing was pushed or deployed.
