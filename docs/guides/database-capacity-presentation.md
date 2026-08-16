# Database capacity — how it should look

**Date:** 2026-08-16
**Status:** Implemented baseline; phased follow-up plan included below.
**Full research** (gitignored): `docs/local/DATABASE-CAPACITY-PRESENTATION-RESEARCH.md`

This note answers: should session-pool / storage threshold be a new
heartbeat `WARNING` status, and how should it appear on the dashboard
and Insights?

---

## Summary

Capacity is a **different question** from “is the database reachable.”
Industry tools never map “80% disk / 80% connections” onto the same
axis as “TCP refused”:

| Class | Reachability | Capacity |
|---|---|---|
| Nagios / Icinga / Checkmk | Host UP/DOWN | Service **OK / WARNING / CRITICAL** |
| Prometheus | Scrape up | Alert `severity: warning\|critical` |
| Kubernetes | Ready | `DiskPressure` **condition** |
| Status pages | Downtime | Degraded (incident) |
| Uptime Kuma | UP/DOWN/PENDING/MAINT | Not first-class; users use DOWN or another tool |

Phoenix already has this split for **TLS expiry**: the HTTP monitor
stays UP and a side-channel notify fires. Capacity should follow that
pattern, plus **visible chips** so it does not look like every other
green monitor.

---

## Why the original DOWN model was replaced

The first capacity-check implementation returned **DOWN** at or above
the configured percent. That reused notifications, retries, folders,
and Insights **as if the database were down**.

That is convenient and also incorrect for:

- Insights availability / outage count / downtime / flapping
- 24h/30d uptime %, badges, SLA bars
- Public status page (**Major Outage**)
- Folder worst-of-children + folder alerts
- Favicon / admin “anything down” chrome

A privilege error on optional SQL had the same problem: “forgot
`VIEW SERVER STATE`” looked like an outage. The implemented model now
records that as a condition `error` while the primary probe remains UP.

---

## Verdict

### 1. Do **not** add `domain.StatusWarning`

- Heartbeat statuses stay **DOWN / UP / PENDING / MAINTENANCE**.
- **Yellow is already PENDING** (retry window; no notify). A fifth
  yellow status would collide and would need ~55–70 files (domain,
  Insights, rollups, 11 notifiers, WS, badges, every pill/bar/filter)
  to stay consistent. A half-added WARNING would notify, SLA, folders,
  and WS disagree.
- Uptime Kuma (same 0–3 enum) has not added WARNING.

The operator still gets a **warning** — as a **condition**, not as a
heartbeat status.

### 2. Split reachability from capacity

| Axis | States | What it means |
|---|---|---|
| Heartbeat | UP / DOWN / PENDING / MAINTENANCE | Connect, auth, `SELECT 1` / PING |
| Capacity condition | `ok` / `warning` / `error` / derived `stale` | Session pool or storage vs threshold and query health |

- Connect / `SELECT 1` fail → **DOWN** (unchanged).
- Over warning threshold → heartbeat stays **UP**, condition =
  `warning`, notify via the cert-style `capacity_condition` event,
  **do not** open an outage or trip Insights.
- Optional later: a **critical** percent that *does* flip DOWN
  (Nagios `-c`). Default off. That is the honest “treat this as an
  outage” switch.
- Privilege / unknown capacity: **not** “checkout-db is DOWN.” Stay
  UP, persist condition `error`, show it in Needs attention, and notify
  “check failed / need grant.”

### 3. Dashboard — make it look different

Keep the green **Operational** pill if the DB answers.

Add **condition chips** next to the pill (amber, `bg-warning/10
text-warning border-warning/25`):

`Sessions 88%` · `Storage 84%`

- Card footer already has `heartbeat.msg` — keep the full sentence.
- Left rail and favicon stay **red only for DOWN**. Warning ≠ retry
  PENDING; do not reuse the wallboard yellow-rail for both.
- “Needs attention” = `down || pending` plus confirmed capacity
  `warning` / `error` / derived `stale` on an **active** monitor.
  Paused and maintenance monitors can still show a stale chip on
  detail, but they do not re-enter the attention list just because
  sampling stopped.
- Latency sparkline stays ping. Do not paint it red for capacity.

### 4. Insights — do **not** mix this into availability

Insights ranks reliability (availability, outages, downtime, latency,
flapping). Those words mean **reachability**. Folding disk-full into
them repeats the ranking lie called out in
`docs/local/INSIGHTS-PAGE-REVIEW.md`.

- No change to `ComputeReliability` for capacity.
- Dashboard reliability preview stays outage-based.
- **Later:** an Insights “Capacity attention” card (monitors currently
  warning, or highest utilization). Not a composite score. Not a
  fifth sort key until samples are persisted.

### 5. Charts and detail

- Uptime bar / downtime bands = reachability only (no red day for
  “88% sessions”).
- Detail page: a dedicated Capacity signals section shows the current
  measurement, semantic resource/scope/source, freshness, and state.
- Utilization history chart is optional and needs persisted samples
  (`Metadata` is dropped today except TLS).

### 6. Public status page

Stay **Operational**. Optional muted line (like “TLS expires …”).
Do not auto-Degrade the public page from capacity.

---

## Implemented baseline (2026-08-16)

1. `ports.CheckResult` carries typed `Conditions`, primary
   `LatencyMs`, and total `DurationMs`. Capacity query time no longer
   inflates the primary latency metric.
2. `applyCapacityChecks` leaves a successful heartbeat UP and emits
   `session_pool` / `storage` observations with state, used, limit,
   percent, threshold, unit, resource, scope, source, and freshness.
3. Migration `029_monitor_conditions` persists one latest row per
   `(monitor_id, kind)` in both MariaDB and SQLite. Notification cursor
   fields survive worker restarts; disabled checks delete obsolete rows
   and remove them from live clients.
4. `MonitorConditionService` keeps a stable state separate from the
   candidate count. Warning/error/recovery promote only after two
   consecutive candidate samples. Recovery hysteresis is evaluated
   against the stable warning state, so `74%` then `77%` cannot
   recover. The first-ever warning/error is unconfirmed (no chip, no
   notify) until the second sample. First-ever OK is immediately
   stable. Notifications fire only after that promotion, using the
   delivery cursor.
5. Capacity queries run on every primary check. Freshness (`stale`) is
   three monitor intervals, with a one-minute floor, owned by
   `MonitorConditionService` — not the scheduler.
6. `GET /api/monitor-conditions` and WebSocket condition events are
   access-scoped through the same RBAC monitor visibility used elsewhere.
7. Dashboard and detail surfaces show condition chips and freshness.
   Needs attention includes warning/error/stale conditions on active
   monitors, but not paused or maintenance monitors whose rows simply
   go stale. DOWN counts, uptime, folders, badges, status pages, and
   Insights remain availability-only.
8. All 11 notification providers receive `capacity_condition` copy;
   webhook payloads and reusable templates expose typed `condition.*`
   values. A recovery emits its own condition event after debounce.

No fifth heartbeat status or status-value migration was added.

---

## Improvement plan

### Phase 1 — Correct semantics and latest state (complete)

- Separate availability from capacity, persist latest state, debounce
  transitions, add hysteresis/freshness, notify, and surface the result
  in dashboard/detail views.
- Verify both adapters, RBAC, Redis WebSocket map-shapes, maintenance,
  delivery failure, and engine-specific units with effect-based tests.

### Phase 2 — Operational hardening (next)

- Add a per-notification delivery ledger. The baseline has one cursor
  per condition: a partial multi-channel failure is observable, but the
  successful delivery advances that coarse cursor to avoid duplicate
  alerts on every sample.
- Add explicit `unknown` / `disabled` presentation for “configured but
  never sampled” and “check turned off”; today disabled rows are removed
  and never-sampled checks have no row.
- Export condition state/age as Prometheus metrics and add structured
  counters for query error, transition, suppressed send, partial send,
  and stale condition.
- Run MariaDB integration coverage for every fixed query and privilege
  failure, not only SQLite repository coverage and checker unit tests.
- Add a configuration preview that names the exact semantic resource
  before save (for example PostgreSQL database size versus Redis memory).

### Phase 3 — History and capacity insights

- Add a bounded `monitor_condition_samples` time-series with retention
  and rollups. Keep `monitor_conditions` as the fast latest-state table.
- Add a separate Insights “Capacity attention” view: current warnings,
  highest utilization, trend, time-to-threshold, and data freshness.
  Do not merge it into availability, outage count, or flapping.
- Add detail charts only after sample retention and downsampling are
  defined; never infer history from heartbeat messages.

### Phase 4 — Explicit critical policy (requires product approval)

- Optionally add per-kind critical thresholds and a clearly named
  “critical capacity affects availability” switch, default off.
- Critical-to-DOWN must use separate debounce/recovery settings and show
  its effect in the form before save. It must not silently reinterpret an
  existing warning threshold.
- Re-run public status, folder rollup, notification, SLA, badge, and
  Insights contract tests before enabling that policy.

### Deliberately not planned

- No `domain.StatusWarning` fifth heartbeat state.
- No automatic public-status degradation from a private capacity warning.
- No opaque composite “health score” mixing reachability and utilization.

---

## Related

- Setup and least-privilege grants:
  `docs/guides/database-monitor-setup.md`
- Architecture monitor table: `docs/ARCHITECTURE.md`
- Insights ranking rules: `docs/local/INSIGHTS-PAGE-REVIEW.md`
