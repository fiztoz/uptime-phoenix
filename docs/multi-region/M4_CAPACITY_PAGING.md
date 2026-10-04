# M4 per-probe capacity paging

This slice gives a remote monitor's confirmed capacity conditions the same
paging the local deployment has: the source that owns the assignment opens one
capacity incident per condition kind, pages warning/error changes and recovery
through its own durable outbox with an immutable rendered snapshot, and the hub
mirrors the incident and delivery outcomes without doing provider work. With the
[capacity state slice](M4_CAPACITY_STATE.md) this completes M4's capacity
requirement; escalation, recovery policy, backup/deployment compatibility and
fleet UI remain open.

## Behavior and ownership

Paging is level-triggered against a durable delivered-state cursor on the
condition row, exactly like the local `MonitorConditionService` compares against
its `LastNotifiedState`: a confirmed warning/error the operator has not been told
about pages, a changed warning/error re-pages, and a confirmed recovery pages
once. Unconfirmed candidates and the first baseline `ok` never page. The cursor
records the committed delivery intent — stronger than the local after-success
cursor — because the durable outbox owns retries and outcomes.

One capacity incident per monitor, assignment generation and condition kind is
an identity, not a state: a warning/error change restates the open `firing`
incident at a new transition version and a recovery is the single terminal
`resolved` advance. A partial unique index makes the single-open-incident rule a
storage invariant. The promoted `condition.transition` names the open incident in
its `source_alert_id`, and the hub correlation added with the capacity state
slice binds it to the same monitor/generation/condition.

Every page stores its immutable rendered snapshot (state, prior state,
measurements, bounded labels and message) with the delivery intent, so a retry
after a restart renders the state it was committed for. The delivery authority
revalidates the snapshot against the stored incident before any provider send: a
restated or resolved incident supersedes pages for older states instead of
sending them late, and a `capacity_condition` outcome can only follow a capacity
incident (`status_change`, `certificate_expiry` and `probe_connection` keep their
own subjects). Rendering pins the status pair to `UP`/`UP` so no provider can
produce a false outage or recovery headline, and carries the eleven `condition.*`
template fields of the local notification contract.

Maintenance suppresses the entire paging lifecycle without advancing the cursor
or quietly closing a factually open incident: the deferred page fires at the
first check after the window, with its snapshot repeating the state on both
sides exactly as the local service renders a delayed page. Disabling a capacity
check closes its open incident administratively with no provider work and retires
the row, so the current projection's omission clears the hub mirror.

## Operator limits and upgrade order

Mixed-version rehearsal remains open operational work. An older hub permanently
rejects `alert.transition` events whose subject is `capacity` and
`delivery.result` events whose kind is `capacity_condition` per sequence without
corrupting the stream; an older edge simply never emits them. `acknowledgement`
on a capacity incident is rejected like certificate acknowledgement — ACK stays
an availability concept.

Run schema changes with all application writers stopped. Edge migration `013`
adds the paging cursor columns to `edge_condition_state` and rebuilds
`edge_alerts`, `edge_delivery_outbox` and `edge_watchdog_state` to widen the
subject/event-kind checks with `capacity`/`capacity_condition`, add
`condition_kind` and the `condition_json` snapshot column, and recreate the
`edge_alerts_certificate_identity` invariant plus the new open-capacity-incident
unique index and the metadata-budget triggers (SQLite index names are global, so
indexes are created only after the old tables are gone). Its downgrade guard
refuses while any capacity incident, delivered cursor, capacity delivery or
capacity transition event exists. The 005 migration-rehearsal test now steps 013
out and back around its round-trip exactly like 011, because older rebuilds
silently drop newer columns.

## Verification coverage

`TestEvaluateCapacityPagingLifecycle` pins the pure paging contract: the open
page and its rendered snapshot, unconfirmed/baseline silence, restate keeping the
incident identity under a new version, steady-state silence, recovery resolving
once and failing loudly without its incident, maintenance deferral without
consumption, administrative close on removal, and channel-less deployments still
advancing the cursor.

`TestEdgeCapacityPagingLifecycle` drives the production recorder and real edge
SQLite end to end: unconfirmed silence, ordered `observation → alert.transition
→ condition.transition` events on the confirming sample, the stored immutable
snapshot, the two-sample-confirmed restate under the same identity, recovery
resolution and its page across a process reopen, a new breach opening a new
identity, and disabled-check retirement closing the incident with exactly the
committed pages and no invented work.

`TestProbeCapacityPagingAcceptance` runs on both hub engines: the mirrored
firing/restate/resolution lifecycle with per-transition outcomes, duplicate
receipts adding nothing and zero hub provider work; nothing accepted after the
single resolution; a delivery kind escaping its incident subject rejected;
remote acknowledgement rejected; and a `condition.transition` naming exactly its
own capacity incident accepted after its raw evidence.

Two pre-existing contracts were updated, not weakened: the wire encoder test that
pinned capacity subjects as unemittable now pins the real invariant (a kind-less
capacity subject stays unemittable), and the 005 rehearsal test brackets 013 the
way it already bracketed 011.

Executed results are recorded in the accompanying
[hashed evidence](M4_CAPACITY_PAGING_EVIDENCE.json). The process smoke harness
was not extended with capacity assertions and was not run, so process-level
paging across offline failure/recovery and restarts is author-tested but not
process-verified. Provider rendering of the `condition.*` template fields through
real provider APIs was not exercised beyond the existing sender tests.

## Remaining scope

Escalation synchronization (direct-monitor then ancestor-group precedence with
acknowledgement effect), group/status-page recovery policy, Insights/cache
invalidation, config-as-code and backup compatibility, deployment documentation
and fleet UI remain open M4/M5 items. Local acceptance does not authorize push,
deployment or a production migration.
