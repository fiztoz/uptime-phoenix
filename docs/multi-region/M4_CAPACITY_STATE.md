# M4 per-probe capacity state and two-sample promotion

This slice gives a remote monitor's auxiliary capacity conditions (session pool
and storage) the same state contract the local deployment has: raw checker
evidence becomes a promoted `ok`/`warning`/`error` condition after two-sample
confirmation with five-point warning recovery hysteresis, `stale` stays a derived
display state, and capacity pressure never changes primary availability. It
advances M4's auxiliary evidence requirement; capacity paging, escalation,
recovery policy, backup and deployment compatibility remain open.

## Behavior and ownership

Promotion runs at the source that owns the assignment, exactly like the local
`MonitorConditionService`, and is never recomputed at the hub. One check produces
one durable commit: the raw `conditions` ride the immutable observation event,
the evaluated state is stored in the new `edge_condition_state` rows fenced by a
`version` column (a concurrent writer forces re-evaluation instead of losing or
duplicating a promotion), and a promoted state change emits one ordered
`condition.transition` event whose `previous_state` is the prior confirmed state
or null for the first promotion.

The promotion contract is the local one, now shared as `services.EvaluateCondition`
over `services.PromoteCondition`: a first `warning`/`error` candidate stays
unconfirmed until its second consecutive sample, a first successful sample with
no confirmed state confirms immediately as the baseline, warning recovery must
drop below `threshold - 5` for two consecutive samples, and a recovery candidate
is discarded — not counted — when hysteresis re-latches the warning. A checker
that fails to sample keeps its row (which ages to derived `stale`); a kind whose
check is disabled in the accepted configuration retires its row, and the
current-state projection's omission clears the hub mirror. Raw checker state is
preserved as evidence even when hysteresis holds the candidate at `warning`.

The hub mirrors; it never promotes. Observation `conditions` are stored as exact
raw samples on `probe_observations.conditions_json`. `condition.transition`
events copy the promoted state into the `monitor_conditions` regional row for the
same monitor/probe/assignment generation. The current-state snapshot carries the
complete evaluated `conditions` list into `monitor_probe_state.conditions_json`
and replaces the `monitor_conditions` mirror wholesale. All three writers share
one `source_seq` fence on the mirror row: the highest source sequence wins, so
older replay, retired assignment history and stale sessions cannot replace newer
state, while an equal-sequence snapshot retry must reproduce identical evidence
or conflict. Complete snapshot omission clears the assignment's condition rows
and keeps historical samples.

A `condition.transition` without preceding mirrored raw evidence is permanently
rejected (`condition_evidence_not_found`) — raw evidence always precedes its
promotion in the stream — and a supplied `source_alert_id` must correlate with a
capacity incident for the same monitor/generation/condition (`incident_mismatch`
otherwise). These transitions never change primary availability, never create
incident recoveries by inference, never rerun promotion, and never carry or
invent a delivery cursor. Capacity evidence creates zero provider work on both
sides.

## Operator limits and upgrade order

Mixed-version rehearsal remains open operational work: an upgraded edge emits
`condition.transition` events and conditional observation evidence that an older
hub permanently rejects per event (it does not corrupt the stream), and an older
edge simply never sends the new fields. Certificate and availability telemetry
are unchanged.

Run schema changes with all application writers stopped. Edge migration `012`
creates `edge_condition_state` and rebuilds `edge_telemetry_outbox` to widen its
`kind` check with `condition.transition`, copying every row before the drop and
recreating the age index; its downgrade path refuses while any evaluated state or
retained transition event exists, then rebuilds the outbox back to the narrower
check. Hub migration `068_probe_capacity_state` adds nullable `conditions_json`
to `probe_observations` and `monitor_probe_state` and `source_seq` to
`monitor_conditions` on SQLite and MariaDB; its MariaDB scripts are idempotent
(`ADD/DROP COLUMN IF EXISTS`) for the registry migration rehearsal and its
downgrade guard names only columns that outlive the migration. The MariaDB JSON
columns keep exact measurement values and timestamps independently of the SQL
column precision, like `tls_json`. Mirrored labels are truncated to the actual
`monitor_conditions` column widths (24/32/32/160 bytes) rather than failing an
entire ingest batch; messages stay bounded at 4096 bytes.

REST `GET /api/monitor-conditions` serves the mirrored rows through the existing
repository, so remote capacity state is visible through the same read model as
local capacity state. Region labels and fleet views stay M5 work.

## Verification coverage

`TestEvaluateConditionTwoSamplePromotion` locks the state machine in the shared
service layer: first warning unconfirmed then confirmed with a null-previous
transition, the 82/82 single promotion, the 74/77/74/74 hysteresis sequence with
no early recovery, error-to-warning without borrowed candidate counts, the
first-sample baseline rules, and debounce continuity from a persisted candidate.

`TestConditionWireRoundTrip` drives the production encoder and validators: raw
evidence on observations with derived freshness, explicit empty lists, promoted
transitions with rejected inconsistent identities, and the current-state
evaluated payload check that refuses evidence disagreeing with the immutable
observation bytes.

`TestEdgeConditionLifecycleAndRestart` drives the production recorder and real
edge SQLite: unconfirmed first sample with no events, promotion emitting exactly
one ordered `condition.transition`, the fenced stale re-evaluation, restart
retention of the candidate, disabled-check removal inventing no events, and
rejected mismatched transitions and duplicate kinds.

`TestProbeCapacityStateAcceptance` runs on both hub engines:

- `RawHistoryAndUnconfirmedState`: exact raw samples in history, unconfirmed
  mirror, and zero incidents/deliveries/alerts with availability untouched.
- `PromotedTransitionMirrorsStateWithoutReevaluation`: transition promotion and
  recovery copying source state, duplicate receipts adding nothing, and no
  invented delivery cursor.
- `UnsupportedConditionTransitionsAreRejectedPermanently`: missing raw evidence
  and fabricated incident references rejected without blocking later events.
- `SnapshotReplacesAndOmissionClears`: complete evaluated replacement,
  same-sequence conflict, and omission clearing while history is retained.
- `OlderEvidenceCannotReplaceNewerState`: older replay cannot regress the mirror
  or the current projection.
- `MigrationRoundTripAndEvidenceGuard`: populated evidence refuses downgrade;
  an empty round trip re-applies cleanly.

## Executed evidence

The full race suite passed across 22 packages on the local Go toolchain against
SQLite and MariaDB 11 (`TEST_MARIADB_DSN` pointed at the disposable
`phoenix-m04-checker-coverage` container), including every authored capacity
case above on both engines and the pre-existing replay, TLS, certificate-paging
and auxiliary contract suites. `go build ./...`, `go vet ./internal/...` and
`gofmt -l internal/` were clean. `golangci-lint` aborts in this environment
before analyzing code (its binary is built with go1.26 and a checked file
requires go1.27), so the documented gofmt+go vet fallback applies.

Two first-run failures were root-caused and fixed, not suppressed: the shared
`MixedDurableOutcomes` fixture expected `unsupported_event` for a body-less
`condition.transition`, which is now a supported kind and rejects as
`event_invalid` instead; and the `MigrationAndGuards` auxiliary rehearsal lost
the `source_seq` column because migration 041 predates 068 and SQLite rebuilds
`monitor_conditions` with 041's column list — the same shared-schema drift class
the MariaDB tail-healing harness handles, now healed for 068 as well. A test-fixture
bug was also fixed where durable priors leaked the raw state into the promoted
state slot, which falsified the transition's previous-state identity.

Executed results are recorded in the accompanying
[hashed evidence](M4_CAPACITY_STATE_EVIDENCE.json). The process smoke harness
(`scripts/probe_runtime_smoke.py`) was not extended with capacity assertions and
was not run for this slice, so process-level capacity behavior across offline
failure/recovery and restarts is author-tested but not process-verified.

## Remaining scope

Capacity paging from the source outbox (`capacity_condition` delivery outcomes
and capacity incidents) is the natural follow-up: the wire `subject`/`event_kind`
slots and the incident correlation check for `source_alert_id` are already in
place, but no source opens a capacity incident yet. Escalation synchronization,
group/status-page recovery policy, Insights/cache invalidation, config-as-code
and backup compatibility, deployment documentation and fleet UI remain open M4/M5
items. Local acceptance does not authorize push, deployment or a production
migration.
