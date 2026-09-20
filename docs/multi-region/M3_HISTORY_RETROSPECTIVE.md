# Historical recomputation: invalidation and transaction lessons

## Summary

The M3 history draft could report correct individual projections while losing
parent updates, leaving later history stale after clock regression, or disagreeing
with its on-demand reader. These defects were found before accepting the increment.
Codex implemented and corrected the draft on `codex/multi-region-probe-plan` after
`a1adb01`; Antigravity contributed a bounded design review. This is an engineering
record of those fixes, not a claim that the entire M3 milestone is complete.

## Root causes and fixes

**Queue order did not establish a transaction dependency.** An hour/day could be
selected before a source transaction committed a new minute and parent token.
The projection transaction then read the new parent token but the old child row,
published and consumed the fresh work. No later child completion re-created that
parent. `requireCleanHistoryChildren` now reads dirty child dependencies in the
same snapshot as the token, before projection; the final current lock and token
check handles source writes after that snapshot. A query-hook barrier reproduces
the exact before-snapshot race for both hour and day on MariaDB.

**Freshness bounded invalidation too narrowly.** `seq1 UP` at 00:00 and `seq2 UP`
at 01:00 were projected. Late `seq3 DOWN` at 00:00:30 correctly dominated sequence
2 in pure reconstruction, but carry-forward queued only its nearby freshness
window. The already-computed 01:00 row stayed UP. `markObservationHistoryTx` now
persists a merged repair range atomically with the source observation;
`expandHistoryRange` resumes one minute at a time and invalidates every resolution.
A later sequence can remain authoritative after becoming stale: the correct
result at 01:00 is UNKNOWN, not resurrection of sequence 2.

**Different readers used different evidence.** The background reader retained an
old sequence-leading seed, while `MonitorHealthService.History` loaded only the
freshness lookback. After the range fix, the worker showed UNKNOWN at 01:00 but
the on-demand service still showed UP. Both now use `ReadHistoryEvidence` and the
same coherent seed/gap/assignment reads. The regression asserts both outputs.

**Managed-only children lost legacy statistics.** Parent recomputation originally
excluded legacy child rows. Three old samples plus one new sample became one
sample, and latency changed from its proper weighted value. Parent reads now
retain all child statistics. Legacy duration fields remain zero, so sample counts
cannot fabricate known time. A both-engine regression expects four samples and
weighted mean 35 ms from three 40 ms samples and one 20 ms sample.

**A source revision needed identity, not a resettable counter.** Consume/reinsert
can reuse an integer revision and let a stale clear erase new work. Every mark
now creates a fresh random token, and clear is conditional on the exact token.
Overall keys coalesce by monitor/minute across probes. Last-write fault injection
proves publication and consumption roll back together.

**Assignment removal needed its own invalidation.** A removed probe may never
send another sample. Assignment writes now mark old and new members so duration
stops precisely at removal. Tests assert each fractional-minute denominator.

MariaDB also returned error 1020 during a current locking read after a concurrent
source commit. Treating that serialization result as a fatal worker failure was
unnecessary. The transaction rolls back; the worker leaves the revision queued
for a fresh snapshot. SQLite serializes with its writer lock instead. Neither
engine behavior was inferred from the other.

## How the defects were found

The parent race failed deterministically with `parent consumed new work before its
dirty child was recomputed`. Extending a backward-clock example beyond freshness
exposed the stale later bucket. Calling the public service reader against the same
database exposed its divergent result. Inserting a legacy child through the normal
heartbeat port exposed dropped statistics. Tests assert durable output, remaining
work, unchanged replay cursor and zero provider intents.

The broad gate also caught fixture errors: an old SQLite 037 migration rebuilt
aggregate tables without the 057 columns; the 035 downgrade/restore test omitted
057 from its later migrations. Tests now restore the actual schema sequence. The
failed broad run is recorded and replaced by subsequent corrected evidence, not
reported as acceptance. The earlier misspelled MariaDB variable correction is
separately recorded in `M3_CURRENT_STATE_ACCEPTANCE.md`.

## Why it slipped through the initial draft

Single-worker happy paths make queue ordering appear sufficient. Clock examples
inside freshness do not test invalidation beyond it. Testing a pure projector does
not prove its callers load equivalent evidence or queue every affected window.
An empty newly migrated database does not exercise legacy rollup statistics.
These were integration and test-coverage gaps in Codex's draft; they are not
attributed to the junior agent.

## Antigravity review and teaching

Antigravity's reasoning-only review in conversation
`92475cca-166b-4d5f-8473-03d5dcfdb3d1` independently identified the parent dependency
race. Codex reproduced it before accepting the finding. Antigravity's concern
about partial gap expansion was checked against the existing atomic parent
re-marking; its sequence-gap scenario is covered by a focused regression.

The integrator sent the verified mechanism, failed/passing test distinctions and
coding lessons back to Antigravity. It acknowledged that queue priority is not a
fence, projection and invalidation must be tested together, and package PASS is
not proof that an engine ran. It made no source or AGENTS.md edits.

Its final proposed older-sequence-after-newer-sequence example was not accepted
as a defect: ordered telemetry cannot accept sequence 3 while sequence 2 is missing
without a declared gap, and an earlier sample cannot invalidate genuinely newer
sequence 3 at its later timestamp. Review proposals remain hypotheses until the
real ordering and freshness contract supports them.

## Validation and follow-up

The original deterministic repros now pass. See
[M3_HISTORY_ACCEPTANCE.md](M3_HISTORY_ACCEPTANCE.md) for exact focused, real-engine,
process and final-gate coverage, including pending checks. The process acceptance
uses actual hub workers and an edge restart; it is shorter than 15 minutes.

Codex owns the remaining milestone work in `M3_COMPLETION_WORK_CONTRACT.md`.
The regression tests stay at the source/transaction/read boundaries; no new
canonical project rule or unrelated refactor is required by this retrospective.
