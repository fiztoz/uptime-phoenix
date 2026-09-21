# M3 source metadata bounds and cleanup contract

Date: 2026-09-21. Pending work after the shutdown checkpoint. This is a verified
source inventory and implementation contract, not a claim that cleanup exists.
Codex owns all files. Antigravity's attempted storage audit returned no report.

## Existing bounds and missing paths

Telemetry already enforces configurable age/bytes, a critical-event reserve,
bounded gap fragmentation and explicit loss declarations. Provider queue admission
counts every retained row against a separate 64 MiB cap. Commands and rotation
ledgers have separate bounded admission and retention rules. Preserve those.

`edge.Store.ActivateConfig` inserts each protected config revision without pruning
old revisions or bounding their total bytes. It retains assignment identities to
reject reuse of an old generation. `RecordEdgeCheck`/watchdog source transactions
retain incident rows and generation-scoped state; resolved incidents without any
notification channel can continue accumulating. Completed delivery rows remain
in the 64 MiB admission budget without a cleanup path. Individual payload limits
are not a bound on all retained history. SQLite reuses freed pages, but there is
currently no explicit file/metadata budget to stop these paths growing forever.

## Required behavior

Reproduce growth and exhausted delivery capacity before changing behavior. Define
safe retirement using actual foreign keys and domain references. Preserve active
configuration, active/generation safety state, unresolved incidents, all pending
or leased provider work, required command idempotency and stream-reset evidence.
Queued telemetry contains immutable serialized evidence; cleanup must neither
rewrite it nor silently declare it acknowledged. Do not delete generation
tombstones without another durable stale-generation guard.

Bound retained config/state/incident metadata explicitly and fail closed with
visible storage diagnostics when live pinned state consumes the budget. A cap
alone must not permanently exhaust routine delivery: prune eligible completed
history in bounded transactions. Explain the retention horizon and how receipt
retries and queued outcomes remain safe. Preserve referenced configurations until
all relevant records permit retirement. Use paired new migrations if additional
durable cleanup watermarks are needed; do not edit old migrations.

Verify actual SQLite file behavior under repeated fill/drain cycles, including
WAL and long-lived readers. Avoid claiming a logical SUM is a hard filesystem
quota, or that read-only inspection cannot create WAL sidecars. Any checkpoint or
vacuum must be bounded and must preserve active storage ownership and durability.

## Acceptance

Real SQLite: repeated config changes, assignment-generation changes and unnotified
DOWN/UP cycles stay within the declared limits; completed provider history becomes
eligible without losing pending work; safe cleanup commits atomically and late
failure rolls back; restart and stale commands cannot resurrect deleted state.
Preserve unacknowledged telemetry byte-for-byte and prove referenced active data
survives. Show file sizes across repeated cycles, exact thresholds, pressure/error
health, and migration upgrade/downgrade behavior. Then run the complete M3 gate
and actual fifteen-minute partition, rather than marking M3 complete from the
existing telemetry cap alone.

## Executed reproduction before implementation

The read-only source inventory is now backed by an isolated Go test overlay at
`/private/tmp/phoenix_m3_metadata_overlay.json`. It appends
`TestM3MetadataCleanupReproduction` to a temporary copy of `store_test.go`; the
repository source stayed unchanged during the concurrent shutdown gate.

`go test -race -count=1 -overlay=... ./internal/adapters/repository/edge -run
TestM3MetadataCleanupReproduction` failed both cases on real SQLite:

- 64 successful `ActivateConfig` revisions retained all 64 configs after an age
  sweep 400 days later, although only the latest revision had an assignment ref.
- 32 actual unnotified DOWN/UP source transactions retained all 32 resolved
  incidents after that sweep. The last current-state reference must be retained;
  earlier resolved incidents had no provider-delivery references.

The runner is `/private/tmp/phoenix_m3_metadata_repro.py`, test additions are
`/private/tmp/phoenix_m3_metadata_repro_tests.txt`, and the failure log is
`/private/tmp/phoenix-m3-metadata-repro.log`. Convert these into permanent effect
tests when implementing cleanup, preserving active references and command
idempotency. These expected reproduction failures do not belong to the passing
shutdown regression gate and must not be hidden or counted as fixed.
