# M3 historical recomputation work contract

This is the next implementation contract after current-state acceptance, not a
completion claim. Codex owns production code, migrations, tests and integration.
No other agent currently owns source files.

## Intent and traced gaps

Late remote replay must correct regional 1m/1h/1d rollups and overall history at
the original UTC times, even outside the periodic lookback. Declared loss must
reduce known coverage; it must never become a fabricated successful heartbeat or
provider send.

The existing code is insufficient by itself:

- `AggregateService.Rollup1m` reads legacy heartbeats. Remote replay stores
  `probe_observations`, so merely scheduling that function misses remote history.
- `MonitorHealthService.ProcessDirty` reads, replaces and clears in separate
  transactions. A concurrent mark can be lost between reconstruction and clear.
- `markDirtyTx` treats an existing key as a no-op. A consumer cannot rely on that
  read for mutual exclusion against the source transaction.
- `probe_telemetry_gaps` persists bounded resumable recomputation metadata, but no
  worker consumes it. Overall reconstruction has no gap input yet.
- Existing aggregate tables have per-probe uniqueness and sample counts, but no
  duration coverage fields. Sample count alone cannot express an outage in data.

## Required implementation properties

1. Keep projection algorithms in core services and DB operations in adapters.
   Introduce a small transactional work port: storage supplies one coherent
   bounded evidence window to a pure projector, then persists output and consumes
   work atomically. Reuse the existing assignment history and reconstruction.
2. Establish a tested writer/consumer serialization rule. Re-marking existing
   work must participate in locking or revision comparison. A failed projection,
   late write or competing worker must leave recoverable work; no read/clear race.
3. Process closed windows and advance gap-range work in bounded steps with a
   durable cursor. Never expand an arbitrarily long gap into an unbounded insert
   or read. Historical monitor deletion must not stall the entire queue.
4. Preserve per-probe rollup keys, ID primary keys and meaningful latency sample
   weighting. Add explicit nonnegative UP/DOWN/PENDING/UNKNOWN/maintenance/paused
   duration coverage. Do not pool regional samples to compute overall uptime.
5. Compute original-time regional and overall effects from assignment generation,
   effective policy, source sequence, freshness and declared gaps. Same-stream
   sequence remains authoritative after a backward clock adjustment; source time
   still locates intervals. Test boundary and clock cases before choosing gap
   clipping details.
6. Propagate changes through 1m, 1h and 1d without allowing periodic legacy rollup
   writers to overwrite coverage or recent remote recomputation. Preserve legacy
   deployment behavior when no remote configuration is enabled.
7. Wire the bounded worker into the real composition root and shutdown lifecycle.
   A helper that has no running consumer does not satisfy M3/T22.

## Acceptance before claiming completion

Use SQLite and real MariaDB, including concurrent connections and a barrier:
replay outside normal lookback, repeated replay, gaps with and without monitor
lists, zero-length source-time loss, backward clock, assignment removal/re-add,
policy changes, rollback at the last write, process restart and competing worker.
Assert all three regional resolutions, overall intervals, duration coverage and
remaining work. Assert zero provider intents and no replay-cursor changes caused
by recomputation. Run the worker through production wiring, then full gates.

Reserve migration 057 only after checking both migration directories again.
The final M3 15-minute process acceptance remains a separate required gate.

## In-progress implementation and validation

Migration 057 was unoccupied and is reserved by the current uncommitted history
increment. The draft has a pure gap-aware duration projector, source-sequence
history ordering, duration fields, guarded legacy aggregate writes, a worker
with coherent reads and atomic revision-checked publish/consume, bounded gap
range expansion and composition-root startup/shutdown wiring. This is not accepted.

Dirty revisions are fresh random tokens, not counters that reset on insertion:
otherwise a consume/reinsert ABA can make an old snapshot appear current again.
Overall work is canonicalized to the reserved local key per monitor/minute, so
updates from different probes invalidate the same overall computation.

Initial service and SQLite smoke tests passed. The first expanded test draft
had an interface/concrete fixture compile error, followed by an aggregate SELECT
alias error; both were corrected. While investigating, Codex found its matrix
commands used the wrong MariaDB environment name. `TEST_MARIADB_DSN` is required.
MariaDB claims from the earlier wrong-variable commands are withdrawn. First
verify the immutable current-state commit, then run this draft's real-engine
contracts. No history acceptance or commit is authorized by a skipped test.


## Final integration findings

The original parent queue priority was insufficient: a concurrent source could
commit after selection but before the coherent read. The parent now checks dirty
children within its token snapshot before publish/consume. Late backward clocks
also need durable range invalidation beyond freshness; migration 057 now includes
one merged repair range per monitor/probe, advanced in bounded minutes. Source
commits and gap/range expansion mark all resolutions atomically.

The on-demand service now uses the same coherent sequence-leading seed reader as
the background worker. Hour/day projections retain legacy child sample statistics
without inventing known duration. Assignment removal independently invalidates
old/new member denominators. Focused reproductions pass; final gate evidence and
remaining scope are in `M3_HISTORY_ACCEPTANCE.md`, with mechanisms and Antigravity
feedback in `M3_HISTORY_RETROSPECTIVE.md`.

After this increment is accepted and committed, continue with
`M3_WATCHDOG_WORK_CONTRACT.md`. Whole-M3 completion still requires the remaining
items in `M3_COMPLETION_WORK_CONTRACT.md`.
