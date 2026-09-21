# M3 metadata retention retrospective

Date: 2026-09-21. Author/integrator: Codex. Baseline: `728aefd`.

## Defect and mechanism

Telemetry age/byte retention did not bound the other durable edge tables.
Every accepted configuration kept its protected payload; resolved incidents and
terminal provider deliveries also remained indefinitely. An individual document
limit does not bound accumulated revisions. A delivery admission cap counted old
terminal history, so it could eventually reject useful new work permanently.

Codex reproduced both missing cleanup paths using real SQLite before changing
source: 64 revisions retained all 64 rows, and 32 unnotified DOWN/UP cycles
retained all 32 resolved incidents after an age sweep 400 days later. The overlay
reproduction log remains `/private/tmp/phoenix-m3-metadata-repro.log`.

## Fix

Migration 010 retains assignment generation tombstones independently of old
configuration blobs. It adds an atomic 64 MiB metadata counter and growth guards,
with indexes for reference checks. Existing oversized stores remain intact and
can delete eligible history. A guarded downgrade refuses to restore the old
foreign key when its referenced config has legitimately retired.

Periodic retention retires at most 512 rows per table in one transaction, after
the greater of seven days and the telemetry retention horizon. Current config,
active state, unresolved incidents, pending/leased work and referenced rows stay.
Serialized telemetry and command idempotency use their own existing retention
rules. Late cleanup failure rolls back deletions and accounting together.

SQLite page limits now bound database growth. A reserved connection serializes
WAL admission checks with source writes; a reader that blocks checkpointing at
the threshold stops further admission. The 16 MiB threshold permits the last
admitted transaction's frames and is explicitly not a hard filesystem quota.

## Evidence and review lessons

Real SQLite effect tests cover the exact metadata quota, rollback of activation,
quota recovery, populated upgrade/downgrade, grandfathered oversized history,
generation reuse rejection, pending and leased deliveries, unchanged telemetry,
the minimum retention horizon and repeated bounded cleanup progress. File-cycle
tests verify page reuse; a pinned read transaction proves growth stops and later
recovers. Final gate evidence belongs in the acceptance record, not this narrative.

Antigravity's read-only review in conversation
`a7804dda-ca50-4741-bba4-451e04ae06a6` returned three alleged confirmed defects.
Codex checked and rejected them: missing caller context was treated as a globally
dead path; the assignment foreign-key removal was overlooked; and fixed-size
upsert accounting was described as double counting despite the review's own
net-zero explanation. These were review errors, not discovered product defects.
Antigravity received the caller and passing effect-test evidence and explicitly
retracted all three. Its report and correction are retained in
`/private/tmp/phoenix-m3-metadata-review.jsonl` and
`/private/tmp/phoenix-m3-metadata-teaching.jsonl`. It edited no files and ran no tests.

For future implementation and review: inventory every accumulating table, trace
its writer and retirement references, then test durable effects at capacity and
after restart. Mark incomplete source visibility as an inspection gap. Do not
convert hypotheses or deliberate safety pinning into confirmed defects. A tool's
successful exit or an agent's confident label is not acceptance evidence.
