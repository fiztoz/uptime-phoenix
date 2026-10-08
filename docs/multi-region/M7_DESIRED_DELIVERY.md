# M7 desired alert delivery persistence

This slice stores the operator's desired availability paging mode. It does **not**
complete M7, send aggregate pages, or suppress regional delivery. Regional paging
remains the only runtime mode. The [decision foundation](M7_PAGING_FOUNDATION.md)
is still unwired.

## Stored contract

`monitor_probe_assignment_sets.alert_delivery` and the matching history column
accept `regional`, `aggregate`, and `both`. Migration `076` defaults existing
rows to `regional`. An empty legacy value read by the application is regional;
any other stored value fails closed.

A mode change uses the existing assignment CAS revision and closes/opens
effective history at one boundary, together with membership and policy. An
identical member set, bindings, policy, and mode does not increment the
revision. `Replace` without a mode preserves the stored mode so older callers
cannot silently downgrade it.

`PUT /api/monitors/:id/probes` accepts the three modes. Omission preserves the
current mode. The response and GET view return `alert_delivery` plus
`alert_delivery_pending`. Pending is true whenever the desired mode is not
regional. `sync_status: applied` is still only a configuration-document receipt
and does not mean a source stopped regional paging.

Backup and config-as-code carry the desired mode. An old document that omits
the field restores or applies as regional on create, and config apply preserves
the live mode when the field is absent. Clone copies the source mode with the
rest of the desired set. The admin assignment command preserves the current
mode when it rewrites membership.

The assignment UI round-trips the stored mode and shows that aggregate paging
is not active. It does not offer a selector. Selecting the mode in the UI
remains blocked until capability negotiation, applied receipts, and
persist/reload/error/conflict coverage exist.

## Explicitly not done

- No probe config snapshot includes `alert_delivery`. Old decoders are unchanged.
- No source capability is required or advertised for this field.
- No aggregate incident, provider intent, deduplication, or ownership fence is written.
- No regional availability send is suppressed because the desired mode changed.
- Certificate, capacity, and escalation behavior are unchanged.
