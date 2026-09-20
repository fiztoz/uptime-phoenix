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
