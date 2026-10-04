# M5 completion contract

Accepted 2026-09-29; see [completion and executed evidence](M5_COMPLETION.md).
This extends the existing M5 slices; their wire
fixtures and actual handler JSON tags remain authoritative.

## Shared contract

- Fleet: `internal/adapters/http/handlers/probe_fleet.go` defines `ProbeView`,
  `ProbeDetailView`, diagnostics, and `{items, next_cursor}` pagination. Revision
  and version counters are decimal strings; unavailable diagnostics stay null.
- Registration and operations: `probe_admin.go` defines create/patch and exact
  operation receipts. Create accepts probe_id/key/name/location/endpoint/
  tls_fingerprint; patch uses name/location/enabled/revision. Manual enrollment
  supplies the probe_id and stream_id from `probe init`, never invented source
  identities. Enrollment accepts enrollment_token and stream_id. The token is
  write-only; an already prepared connection permits omitting its existing stream.
- Revoke: `POST /api/probes/:probe_id/revoke` accepts `{reason}` and returns 202
  operation receipt plus `remote_confirmed`. Hub revocation must be durable and
  fence admission immediately; false explicitly means remote execution has not
  been confirmed stopped. Delete returns 204 only after soft deletion, rejects
  an active assignment with 409, and never permits deletion of `local`.
- Monitor assignment and health: `monitor_regional.go` and its testdata/m5
  fixtures define every field. PUT uses expected_revision/probe_ids/health_policy/
  alert_delivery and optional bindings. Omitted bindings preserve existing
  references. Save is desired state; application is separately proven.
- Regional history: preserve the existing `web/src/lib/api/regional.ts` contract
  and selected-region latency UI. Overall latency remains unavailable.
- Browser events retain protocol section 8 names and explicit wire mappings.
  All monitor events recheck visibility; fleet/config events require admin.
  Decimal projection versions are compared without converting to Number.
  Reconnect causes a bounded resync; regional beats never fetch the full list.
- Regional ACK returns a command receipt and remains pending until the durable
  remote result confirms application. Region attribution is separate from the
  existing monitor/group alert scope. Public views contain overall coverage,
  never regional inventory or endpoints.
- Incident reads use `GET /api/monitors/:id/probe-alerts`; ACK uses
  `POST /api/monitors/:id/probe-alerts/:alert_id/ack` with optional command_id
  and note, and its receipt uses `GET .../ack/:command_id`. Only the requester
  or an admin with current monitor access may read a receipt. Actor identity
  comes from authentication. Availability incidents support ACK; capacity,
  certificate and connection-watchdog ACK are outside this contract.
- Health versions include decimal `"0"` before a projection; duration fields
  permit fractional seconds and missing timestamps/diagnostics stay null.
  The browser decoder and HTTP producer share executable parity coverage.
- Public `coverage_percent` is nullable and uses the overall 24-hour timeline.
  Remote-monitor uptime and historical bars also use overall policy. No measured
  overall latency is fabricated; local-only behavior remains unchanged.
- UI extends docs/DESIGN.md and existing form/control conventions. English and
  Thai copy, keyboard labels, loading/error/empty states, and narrow layouts
  are required. Existing local behavior and non-admin creation remain intact.

## Ownership during this completion (closed)

- Main: all Go/backend, websocket store and shared browser event contracts,
  repository-wide tests, evidence and documentation.
- Svelte editor: fleet API/types, fleet list/detail pages, regional API additions,
  monitor form/detail and regional components, admin navigation, incident/ACK
  UI, English/Thai messages, and new M5 UI tests. No Go or websocket-store edits.

Verification must include the actual SQLite and MariaDB legs, the full Go race
gate, frontend type/build/lint/unit checks, and browser acceptance. A completed
subtask or passing mock-only UI test does not establish full M5 completion.
