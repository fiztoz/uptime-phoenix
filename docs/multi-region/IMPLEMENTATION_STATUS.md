# Multi-region implementation status

Started: 2026-09-13. Source baseline: `b706fb09` on `codex/multi-region-probe-plan`, based on application `5183093c`. This branch belongs in the ordinary repository checkout so subsequent agents can continue the same work.

## Current delivery

M0 and M1 are **in progress**, not complete. This initial foundation implements executable contracts, shared health rules, and additive registration/assignment storage. It does not supply a running remote probe. No probe listener, enrollment endpoint, remote scheduler, connector, provider outbox, ingest cursor, or regional user interface is enabled.

| Surface | Implemented behavior | Remaining integration |
|---|---|---|
| Domain | Reserved `local` identity, registration/assignment types, positive revision/generation contracts, `UNKNOWN=4`, ANY/ALL policy and count/coverage types | Regional observations/state, stream/incident/command types and atomic commit/ingest ports |
| Retry | Pure `EvaluateRetry` reused by the existing `HeartbeatService.Record`; independent state inputs, retry confirmation, maintenance/raw-PENDING reset and overflow saturation | Regional caller/state persistence and extraction of condition/maintenance evaluation |
| Health | Pure complete-assignment ANY/ALL truth table, deadline freshness, explicit missing/future/invalidated evidence, paused counts, duration-based uptime/coverage | Readers/projections, historical interval reconstruction, incident recovery and browser consumers |
| Protocol | Bounded envelope validation, observation-only telemetry batches, ACK/retry/gap payload validation, valid/invalid executable fixtures | Other event DTOs, config/state transfers, commands/enrollment, durable cursor/assignment validation and complete API/browser fixtures |
| Database | Migration `035_probe_registry` on MariaDB/SQLite; local backfill; credential-free registration stores; atomic revision-checked assignment replacement; tombstones prevent generation reuse | Atomic monitor-create integration, scheduling ownership, regional heartbeat/rollup/state/incident schemas, edge DB, config/command metadata and dirty projections |

The repository constructors are intentionally not wired into bootstrap or handlers. Registration metadata conveys no authentication or scheduling authority. `InitializeLocal` initializes an existing monitor explicitly; reads never invent an assignment. Before activating the subsystem, integrate monitor creation and assignment initialization in one transaction, and cover create/clone/import/restore paths. Monitors created after migration currently continue normal legacy execution and have no assignment row until that integration is implemented.

## Concrete contract decisions

1. Observation conditions are raw samples. Current-state conditions require separate observed/candidate/promoted state; a first warning can have null effective state. Replay never reruns promotion.
2. Remote V1 acknowledgement links are deferred. Keep existing local opaque-token links; remote incidents are acknowledged through authenticated hub commands after mirroring. Remote snapshots resolve `include_ack_url` to false and must reject true at activation.
3. Existing gRPC checks can return raw PENDING. Preserve PENDING with zero consecutive failures, including transport validation.
4. Known duration is UP + DOWN; unknown duration includes UNKNOWN, PENDING, and paused time. Maintenance is separate. Uptime uses known time; coverage uses known + unknown. A zero denominator yields null. These are durations from policy-derived intervals, never pooled samples.
5. A future-dated observation is not fresh current evidence. At the exact freshness deadline, observed evidence becomes UNKNOWN, including an old maintenance observation. Later configuration-derived maintenance must evaluate the current schedule explicitly.
6. New dependency versions use the complete snapshot's positive revision. Direct notification IDs and per-link IDs must agree; escalation preserves existing target-inclusion behavior.
7. Preserve distinct wire dialects: existing HTTP uses lowercase status and `message`; legacy browser heartbeat uses `msg` and `paused`; probe observations use uppercase status. Reserving UNKNOWN does not change legacy mappings or authorize emitting it there.
8. Assignment sets contain at least one enabled registration. No-op replacement retains revision/generation; membership or health-policy change increments the set revision; retained members keep generations; remove/re-add increments the tombstoned generation. Revision mismatch and exhaustion return conflict.

## Migration safety

Migration 035 creates `probes`, `monitor_probe_assignment_sets`, and `monitor_probe_assignments`. It seeds the reserved local row and revision/generation-one local assignments for existing monitors. It does not change heartbeat partitions, rollup keys, IDs, or existing scheduler queries.

Down migration is allowed only for untouched local-only foundation data. Remote registrations (even disabled), remote tombstones, or changed assignment revisions/policies prevent downgrade through a database constraint before the source tables are dropped. Stop all application writers for downgrade: MariaDB DDL auto-commits. A failed guard is a refusal, not permission to remove the check or discard records. Populated real-MariaDB migration/rollback rehearsal remains mandatory before deployment.

## Verification record

The unmodified baseline passed Go 1.26.6 build, the full race suite, and golangci-lint 2.12.2 with zero issues. That suite covers existing retry/recovery, condition promotion/hysteresis, certificate delivery cursors, maintenance suppression, acknowledgement/escalation, folder transitions, and WebSocket query budgets.

Integrator verification passed with Go 1.26.6:

- `go build ./...`.
- `go test -race -count=1 ./...` across the complete backend suite.
- `golangci-lint run --timeout 5m`: zero issues; formatting checks also clean.
- Six real SQLite registry/assignment contract cases, including two independent database handles for concurrent replacement, rollback after partial insertion, migration backfill, schema constraints and guarded downgrade.
- Protocol decoders: 25 golden fixtures plus required/null, size/depth, duplicate-key, sequence and status-coherence tests. Independent race review passed.
- Health truth table, freshness boundaries, paused counts, duration coverage and independent regional retry inputs; existing heartbeat/dispatcher regressions preserved.
- Documentation links, fenced JSON examples, whitespace and the exact changed-file manifest validated.

Lint identified one deprecated `bun.In` call; it was replaced with the repository's existing `bun.List` convention. Build/lint and the focused registry race suite passed again after that final adjustment.

Frontend/Helm changes are absent. Live MariaDB/MongoDB tests were skipped because the required environment variables/services were unavailable. SQLite success does not establish MariaDB execution coverage; a populated real-MariaDB migration/rollback rehearsal remains required before deployment.

## Next implementation steps

1. Finish the M0 contract gaps listed in `PROTOCOL.md` section 10: incident subject/threshold/ack metadata, exact state chunks and condition snapshots, enrollment/commands, API/browser shapes, and complete fixture index. Do not advertise full `phoenix.probe.v1` capability on the strength of the framing decoder.
2. Reserve the next migrations after checking both directories and the shared branch HEAD. Add regional state, transactional sequence/outbox, stream receipts/gaps, scoped incident/delivery and projection storage. Preserve partition-safe heartbeat identity and rollup auto-increment keys.
3. Wire local monitor creation and scheduler ownership to assignments atomically, preserving every legacy creation path. Gate remote assignment activation until every worker enforces ownership; never let old workers run remote-only monitors.
4. Integrate shared retry/condition/maintenance evaluation with regional commit. Prove T01/T02/T03/T24/T31/T33 against real persistence, not just the current pure-policy tests. Connect overall readers and update all UNKNOWN consumers before emitting the new status.
5. Only then start the M2 edge runtime and authenticated transport. Leave SSH provisioning and public push gateway for their follow-on milestones.

Use disjoint file ownership, update shared contracts before delegation, and commit each tested slice. The initial helper/persistence work is not evidence that offline replay, notification ownership, or failure recovery already works.
