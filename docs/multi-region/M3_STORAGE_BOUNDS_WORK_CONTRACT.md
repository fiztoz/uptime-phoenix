# M3 source metadata bounds and cleanup contract

Date: 2026-09-21. Implemented after the shutdown checkpoint; final acceptance
is recorded separately in M3_STORAGE_BOUNDS_ACCEPTANCE.md. Codex owns all files.
Antigravity provided an advisory source review and acknowledged corrections.

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

## Implementation decisions

Use bounded transactional pruning with a horizon of at least seven days, extended
to the configured telemetry horizon when longer. Retire only old terminal
deliveries, obsolete generation state, and resolved incidents with no current
state/provider references. Keep unresolved incidents, current state and active
configuration. Serialized queued telemetry remains untouched by metadata cleanup;
its own retention/ACK rules continue to apply. Old source command receipts retain
their existing expiry-plus-365-day rule. A newly issued command targeting a source
incident beyond retained history may be rejected as unknown; it must never affect
a newer incident or manufacture `already_resolved` evidence.

Retain assignment generation tombstones, but separate their revision number from
the protected-config foreign key in a new paired migration. This preserves the
old-generation rejection rule without forcing every removed monitor to pin a
whole obsolete encrypted snapshot forever. Downgrade must refuse if a tombstone's
old revision no longer has the config row required by the old schema.

Use a transactionally maintained 64 MiB budget for config/state/incident metadata,
separate from the existing 64 MiB provider-delivery cap and telemetry budget.
Account protected blobs, current observation bytes, incident text and conservative
row overhead. Triggers keep the counter atomic for every source path and roll back
growth beyond capacity; deletion and non-growing updates still work if an upgraded
store already exceeds the limit. Surface metadata pressure and exhausted storage
honestly. Do not scan the entire metadata history on every heartbeat. This logical
quota is not, by itself, a hard bound on SQLite/WAL file sizes; verify physical
behavior and add a separate explicit bound where needed.

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

## Physical admission decisions

Before each source write, reserve the sole Bun/SQLite connection across the
capacity check and transaction. Set SQLite max_page_count to the greater of its
existing size and the configured page budget: max(1 GiB, twice the telemetry
allocation plus 512 MiB). A pre-existing larger database may reuse/delete pages;
this change does not erase it or shrink it automatically. Reapply the page limit
on every acquired connection so a driver reconnect cannot remove enforcement.

At a 16 MiB WAL threshold, require a TRUNCATE checkpoint before admitting another
write. An external reader that keeps it busy causes a redacted storage failure,
including in CheckWritable; admission resumes after the reader releases. The
threshold can be exceeded by one admitted transaction and is not a filesystem
quota. journal_size_limit only helps reclaim a reset WAL; it is not the guard.
No VACUUM, loss of pending evidence, or forced reader termination is used. The
existing stream-reset archive limits remain unchanged; a configuration allowing
a database larger than the 1 GiB per-archive limit can require operator cleanup
before explicit reset.

SQLite behavior: [page limits and checkpoint pragmas](https://www.sqlite.org/pragma.html),
[WAL checkpointing and long readers](https://www.sqlite.org/wal.html).
