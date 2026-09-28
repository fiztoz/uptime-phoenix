# M5 completion — administration and regional UI

Accepted locally on 2026-09-29. M5 is complete for V1. The next milestone is
[M6 failure validation and controlled activation](IMPLEMENTATION_PLAN.md#9-m6--failure-validation-and-v1-activation).
Remote probes remain opt-in. This acceptance does not deploy or activate a
production installation.

The [shared contract](M5_COMPLETION_CONTRACT.md), [HTTP/browser protocol](PROTOCOL.md#7-hub-administrative-http-api),
and [hashed evidence](M5_COMPLETION_EVIDENCE.json) accompany the implementation.
Earlier M5 slices remain historical acceptance records, not current work lists.

## Delivered behavior

| Surface | Result |
|---|---|
| Fleet administration | Admin list/detail, registration, manual enrollment, revisioned metadata and pause/resume, credential rotation, stream reset, revocation and soft deletion; durable operation progress and redacted failures |
| Assignment editing | Probe selection during admin create/edit, atomic create, revision conflict handling and pending application; existing non-admin local creation preserved |
| Monitor detail | Overall policy, coverage, separate regional freshness/connection/configuration states, heartbeat histories and selected-region latency; missing evidence stays UNKNOWN |
| Incidents and ACK | Source incident identity and region labels; pending commands remain pending until remote application; durable retries and requester-scoped receipt reads |
| Notifications | Default provider payloads carry readable regional attribution; default webhook additionally carries probe, delivery scope and original source incident ID |
| Live updates | Explicit safe mappings for all six regional browser events; current monitor authorization on fan-out, admin-only fleet/config streams and private requester/admin command receipts |
| Public pages | Overall policy history, uptime and 24-hour coverage across all density layouts; no probe inventory, endpoint, secret or invented overall latency |
| UI quality | English/Thai copy, labeled native controls, responsive desktop/mobile states and accessible textual statuses; incumbent design system preserved |

Custom notification templates retain their existing rendering contract and
available regional variables. ACK is implemented for availability incidents;
capacity, certificate and connection-watchdog acknowledgement remain outside
the V1 regional ACK endpoint.

## Operator flow and authority

1. Initialize the edge using the existing [operator guide](M2_OPERATOR_GUIDE.md).
   Obtain its `probe_id`, `stream_id`, certificate fingerprint and one-time
   enrollment token through the trusted console/SSH channel.
2. In **Probes**, register that exact probe ID, a stable key, name/location,
   endpoint and TLS fingerprint. Endpoint admission still follows the installation's
   configured network policy. Registration does not imply an authenticated session.
3. Enroll with the initialized stream ID and token. The browser clears the token
   after submission. The operation is successful only when the pinned runtime
   handshake confirms the prepared connection.
4. Assign local and remote regions to a monitor. Saving desired state does not
   establish execution: the UI waits for a receipt matching revision and digest.
5. During disconnection, configuration and ACK remain pending. Expired check
   evidence becomes UNKNOWN independently of connection status. Reconnection
   applies commands and configuration through the existing durable protocols.

The API retains optional `probe_id` for older registration callers, but manual
enrollment must match the edge's stable identity. A first enrollment requires
`stream_id`; retrying an already prepared connection may omit it. Supplying a
different stream is rejected instead of silently rebinding identity.

Revocation is a committed **hub-side** fence. It disables admission, connector
ownership, configuration/command authority and subsequent session writes. The
receipt is `succeeded / revoked_locally / remote_confirmed: false`: an offline
edge may still execute its last accepted configuration. Deletion returns 409
while actively assigned; otherwise it revokes and hides the fleet registration
while preserving identity, historical attribution and receipts. `local` is
protected. Repeating revocation is idempotent.

Migration **072** adds durable revocation/deletion state and receipts. Migration
**073** records authenticated command requester identity. Both SQLite and MariaDB
down migrations refuse to discard populated authority records. The dual-engine
tests include active-session fencing, rollback of an injected receipt failure,
historical observation retention, exact command retry, requester collisions,
permission removal, and downgrade guards.

Every monitor read and regional fan-out uses AccessService. Hidden monitors
return 404. Fleet administration requires admin, including programmatic writes
through a write-scoped API key. Browser receipt routing metadata is stripped
before serialization. An `access.changed` event immediately purges private
caches and refreshes the monitor snapshot; ordinary projection changes retain
bounded first-paint catalog data while superseding older in-flight reads.

## Production entry points

| Responsibility | Files |
|---|---|
| Hub composition | `internal/bootstrap/run.go`, `internal/adapters/http/router.go` |
| Durable lifecycle | `core/services/probe_admin_service.go`, `adapters/repository/probe_lifecycle.go`, migrations 072/073 and session/connector/command repositories under `internal/` |
| Source incident ACK | `internal/core/services/probe_alert_service.go`, `probe_command_service.go`, `internal/adapters/http/handlers/probe_alert.go` |
| Authorized live publication | `internal/adapters/http/handlers/regional_browser.go`, `internal/adapters/ws/regional.go`, `internal/core/services/access_service.go`, `web/src/lib/stores/ws.svelte.ts` |
| Public history and attribution | `internal/core/services/statuspage_regional.go`, `edge_delivery_service.go`, `internal/adapters/notifier/alert_format.go` and provider implementations |
| Fleet UI | `web/src/routes/(admin)/probes/`, `web/src/lib/api/probes.ts`, `ProbeStatus.svelte`, `ProbeOperation.svelte` |
| Regional UI | `ProbeAssignments.svelte`, `RegionalHealth.svelte`, `RegionalAlerts.svelte`, monitor form/detail, `web/src/lib/api/regional.ts`, shared public monitor list |
| Verification | `m5_lifecycle_test.go`, `m5_alert_api_test.go`, browser event/HTTP parity tests, public coverage/provider tests, `web/tests/e2e/08`–`11`, `scripts/m5_runtime_smoke.py` |

No external dependency, monitor type, provider or permission dimension was added.

## Executed verification

Commands and SHA-256 hashes are recorded in
[M5_COMPLETION_EVIDENCE.json](M5_COMPLETION_EVIDENCE.json). The local gate was
executed as its individual commands, including the Helm render variants and
`helm-validate`, with a longer Go timeout for the real dual-engine suite.

- Full `go test -race -count=1 -timeout=60m -json ./...`: **22 packages, 4,238
  named test/subtest passes, zero failures**. The two existing optional skips
  are MongoDB real-server and Telegram severity environment cases. No M5 tests
  or MariaDB matrix cases were skipped; 332 named MariaDB passes were observed.
- Final browser-decoder corrections were verified by a subsequent complete
  probe package race run and HTTP-to-browser parity test. These supplements
  are distinguished from the earlier full-suite run in the evidence record.
- Normal and CGO-free Go builds, vet, gofmt, golangci-lint, and whitespace gates
  pass. Vulnerability scan: zero reachable vulnerabilities; three module-level
  advisories are outside called code paths.
- Frontend: **256 unit tests**, type check **0 errors/0 warnings**, production
  build, Prettier/ESLint, and **18/18 Chromium browser cases** pass.
- Helm lint, default/split/external-database/Redis templates and topology/probe
  secret guards pass. Source boundary inspection found no framework/DB imports
  in `internal/core/`.

The browser cases cover the existing login, monitoring, notifications, public
status, RBAC, escalation and alert-ACK journeys, plus fleet registration,
operation failures, responsive diagnostics, regional UNKNOWN/pending ACK,
assignment retries, selected-region history, public coverage and navigation
while a catalog refresh is held. Mocked browser operation states are supplemented
by the real process acceptance below and engine-backed authorization tests.

The independent UI finish review resolved primary-action hierarchy, back-arrow
consistency and nested regional cards; final disposition was **ship**. Recaptures
covered desktop and mobile registration, enrollment and regional health.

### Actual pinned-TLS process acceptance

`scripts/m5_runtime_smoke.py` ran a real Go hub against disposable MariaDB 11,
a real autonomous Go edge backed by SQLite, a TLS-preserving partition relay,
a local HTTP target and a local webhook receiver. This was a local process
exercise, not a deployed VM or production canary.

All 11 checkpoints passed: API enrollment, two UP regional streams, matching
configuration receipt, original remote incident, offline pending configuration,
stale evidence becoming UNKNOWN, reconnect/configuration application, remote ACK
confirmation, mirrored original ACK identity and recovery. The partition lasted
89.943 seconds until the existing evidence expired. The final read showed exactly
**one remote incident and one remote outage delivery**, with an applied,
remote-confirmed command receipt. The redacted runtime report is embedded in
the evidence JSON; private keys, tokens and runtime logs are not committed.

### Regressions found and verified

- Real enrollment exposed generated hub identities that could not match an
  initialized source. Registration/enrollment now accept and verify the actual
  probe and stream IDs; the real process run proves the path.
- Regional provider messages lacked reliable attribution. Default messages and
  webhook correlation now retain region/source identity; provider-shape tests
  and the real webhook count verify the effect.
- A regional projection could erase navigation's cached folder structure while
  HTTP refresh was blocked. A deterministic injected browser event reproduced
  the failure; bounded first-paint retention fixed it, while access changes still
  purge immediately. Unit and full browser gates passed afterward.
- The browser decoder did not accept the HTTP producer's zero initial revision,
  fractional durations or absent timestamps. Shared parity coverage now accepts
  those honest states while rejecting contradictory assignment/region counts.

An initial full Go attempt hit the default 10-minute package timeout; the longer
recorded run passed. Frontend unit generation was rerun sequentially after a
Paraglide file replacement collision. Browser verification used a fresh build
after discovering the harness intentionally reuses existing `web/dist`.

## M6 boundary

M5 does not establish populated production migration timing/lock/disk behavior,
distributed load envelopes, a 24-hour backlog drain rate, every kill/restart
boundary, an actual remote VM deployment or a production canary. Those remain
M6. Regional publication queues and caches are bounded, but this record makes
no unmeasured throughput claim. Existing provider-acceptance ambiguity and
retention-gap contracts remain in effect.
