# M7 aggregate availability paging foundation

This is a bounded implementation slice of [M7](IMPLEMENTATION_PLAN.md#10-follow-on-m7--optional-consolidated-paging), based on main
`fa6a76781936ddef392d41bc4e3e7dd22a3e56ea`. It does **not** complete M7 or enable
aggregate paging. Regional delivery remains the only accepted runtime mode.
The separate M6 conditional lab acceptance and operational gates are unchanged.

## Implemented decision contract

`EvaluateAggregatePaging` is a pure availability incident decision function. It
reuses `EvaluateMonitorHealth` rather than creating a second ANY/ALL truth table.
It proposes hold, open, recover, or administrative closure; it does no I/O and
does not authorize a provider send. No production caller is wired in this slice.

The caller supplies a complete assignment snapshot already selected by accepted
assignment generation and stream. Missing/stale/future/recovering evidence stays
UNKNOWN and remains in quorum. Paused assignments are excluded; maintenance is
handled by the existing health evaluator. UNKNOWN, PENDING, and maintenance hold
an existing incident. Only policy-satisfying fresh UP proposes recovery.

Current evaluation requires `SnapshotAt == Now`: reconstruct at one captured
instant instead of reusing an old health projection. This timestamp check is not
proof of source authenticity or replay ordering; those remain repository duties.
Historical reconstruction never proposes an incident transition. Empty delivery
mode defaults to regional; aggregate and both permit aggregate decisions only.

The assignment revision covers membership, policy, and delivery-mode changes.
An obsolete open incident closes administratively, never as target recovery and
never with a same-call reopen. Evidence observed on or before the configuration
effective boundary cannot establish a new transition. The evaluator copies the
evidence before invalidating it, preserving the caller's display snapshot.

## Contracts required before runtime activation

These are integration constraints for subsequent slices, not implemented storage
or API guarantees. Freeze exact ports, migrations, and file ownership before
parallel adapter/service work.

| Boundary | Required behavior |
|---|---|
| Desired configuration | Persist delivery mode on `MonitorProbeAssignments`; absent legacy mode is regional. Mode changes participate in existing assignment CAS revision and effective-time history. Policy/membership/mode no-ops do not increment revision. |
| Applied configuration | A source's applied mode and revision come only from its exact durable configuration receipt, including retained snapshot hash. Desired mode is never evidence that an offline or old probe stopped paging. Expose pending application explicitly. |
| Protocol compatibility | Extend the explicit `decodeConfigFields` allowlist with bounded optional decoding and absent-field regional defaults. A JSON struct tag alone is insufficient. Pending receipts continue to refer to their original immutable snapshot. |
| Source compatibility | Reject activation on an incapable source or leave it visibly pending; never silently claim aggregate-only behavior while it still pages regionally. The supported capability negotiation and activation barrier must be implemented and tested before controls are enabled. |
| Durable reconciliation | Atomically validate current assignment revision, accepted stream/generation evidence, and stored lease owner/epoch/expiry while changing the aggregate incident and inserting provider intents. Enforce one open availability incident per monitor and immutable source identity plus transition version. |
| Delivery | Deduplication, resend deadline, provider intent identity, and ownership fencing survive restarts. A claimed intent must recheck current policy and authority before send. Provider I/O occurs after transactions and shared locks are released. Provider ambiguity after a crash must be explicit. |
| Replay | Historical replay updates history without proposing pages. A fresh current snapshot is reconciled separately after committed source state; insertion time cannot replace stream sequence ordering. Preserve repaired replay/ACK throughput and lock ordering. |
| Policy change | Close old incidents with administrative reasons, invalidate obsolete pending intents, then allow a later reconciliation from post-boundary fresh evidence. Never fabricate recovery or immediately reuse pre-change evidence. |
| Regional suppression | Cover local heartbeat enqueue, queued-send authorization and legacy dispatcher fallback, plus edge recorder and final edge delivery authorization. Suppress availability only when the source has durably applied the selected mode. |
| API and UI | Admin-only mutation; scoped reads; decimal string revisions without JavaScript precision loss; typed stale-revision conflicts and regional legacy creation/import defaults. Update omission preserves the current explicit mode rather than downgrading it. Persist/reload, error, and conflict flows must be tested before controls become selectable. |
| Backup and clone | Backup carries desired policy/mode, never incident ownership, live intents, source credentials, or applied receipts. Restore retains disabled monitors until assignment import commits and disabled probes pending reenrollment. Preserve the existing admin clone rule: copy the complete desired assignment set, policy and mode atomically, with new identities and no copied applied receipts, incidents or intents; never reroute a remote clone to local. |
| Scope | Aggregate availability only. Do not invent aggregate certificate, capacity, or escalation behavior. Existing regional certificate/capacity ownership and `alert.scope` monitor/group meanings remain; delivery scope is a separate explicit label. |

Remote telemetry must continue to reject source claims of aggregate incidents.
When validating unsupported escalation combinations, inspect the raw selected
policy: `ResolvePolicy` intentionally hides enabled policies with no steps.

## Remaining acceptance work

The pure evaluator tests establish decision semantics only. They do not establish
durable incidents, provider delivery, source suppression, API selection, or UI
behavior. M7's implementation-plan checkboxes remain open.

Before activation, execute both-engine atomic transition, stale-owner, concurrent
reconciliation, restart throttle, mode-change, rollback, and populated migration
tests; current/replay ordering and fresh-policy intent authorization tests;
admin/scoped-view and large-revision API tests; and UI persist/reload/error/conflict
tests. Run the repository build, vet, full race, real MariaDB named-pass/skip
accounting, frontend check/test/build/lint/E2E, and `make gate-full`, then inspect
CI for the exact published head. Missing or skipped checks are not passes.
