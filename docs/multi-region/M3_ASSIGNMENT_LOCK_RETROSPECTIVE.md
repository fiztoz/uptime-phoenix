# Assignment replacement versus configuration publication

Date: 2026-09-21. Discovered and fixed by Codex during M3 partition acceptance.

Adding the second partition target while hub workers were active failed with the
admin CLI's redacted assignment error. The disposable MariaDB deadlock report
identified the two participants: configuration publication held the `local`
registration and waited for `monitor_probe_assignment_sets`; replacement held
that set and needed the local registration while updating an old assignment's
foreign key. InnoDB rolled back replacement. Retrying the process harness or
stopping workers during setup would have hidden this production failure.

`ProbeAssignmentStore.Replace` now reads the expected member set without holding
its lock, then locks all relevant registrations in sorted order before locking
the set. Include removed members and `local`, because publication's legacy check
reads assignment sets even when a monitor is becoming remote. The transaction
rechecks the expected revision before changing membership; a stale preliminary
read cannot overwrite concurrent changes. Desired members still must be enabled.
The existing bounded serialization retry helper handles transient database
conflicts around the complete database-only transaction.

`TestProbeAssignmentReplacementUsesConfigurationLockOrder` holds a real MariaDB
SERIALIZABLE configuration-reader transaction while replacement reaches its
registration lock. Its missing-assignment read reproduced error 1213 before the
fix, using a source overlay and separate disposable schema. After the fix, both
transactions complete and exactly one new assignment revision/generation commits.
The focused registry/reset suite passed with both database engines. The original
process failure and deterministic reproduction remain at
`/private/tmp/phoenix-m3-partition-dev2-process.log` and
`/private/tmp/phoenix-m3-assignment-repro.log`.

The same verification round caught a separate test defect: a reset lifecycle
fixture used `time.Now()` on the host while live-state eligibility used MariaDB's
clock. The test could inadvertently exercise the future-observation guard and
leave the reset barrier intact. It now uses the durable reset confirmation time
from the database. Production future-evidence rejection is unchanged. The initial
full gate failure remains in `/private/tmp/phoenix-m3-storage-final-full.jsonl`.

The first new partition rehearsal also attempted target creation after an earlier
stage intentionally stopped the API. Codex corrected the harness to start and
await the API for setup, then stop it again. That setup failure remains in
`/private/tmp/phoenix-m3-partition-dev-process.log`; it was not a product defect.

Review lesson: include implicit foreign-key locks and SERIALIZABLE reads when
tracing lock order. Prove the failing interleaving with two real connections.
Keep lock-order correctness distinct from bounded retries and optimistic
revision checks; all three serve different purposes.

Antigravity reviewed the corrected function without tools or file ownership in
conversation `0dae4e6e-acac-4f2a-ba1e-c5339a39b50a`. It initially claimed a stale
pre-read could reach child writes after another replacement committed, but its
scenario required the immutable expected revision to change or match again.
Codex traced the mandatory in-transaction revision check, which returns conflict
before those writes, and rejected the claim. Antigravity explicitly retracted it
and narrowed its earlier test claim: the deterministic regression proves this
interleaving is fixed, not universal deadlock immunity. Review and correction are
retained in `/private/tmp/phoenix-m3-assignment-teaching.jsonl` and
`/private/tmp/phoenix-m3-assignment-correction.jsonl`.
