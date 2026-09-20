# M2 work contract and integrator ledger

This contract records the completed delegation. The user later authorized Codex
to commit the accepted work; see [the current commit ledger](IMPLEMENTATION_STATUS.md).
The original restrictions below remain historical instructions for delegated work.

Objective (2026-09-20): Codex coordinates Gemini 3.8 Flash in Antigravity until
M2 is implemented and verified. M0/M1 prerequisite corrections remain part of the
work; this objective is not satisfied by leaving the cutover disabled. The complete
M2 scope is IMPLEMENTATION_PLAN.md section 5, ARCHITECTURE.md and PROTOCOL.md.

## Ownership for the current parallel slice

Codex owns all existing modified files, core contracts, hub bootstrap, migrations,
shared documents and integration. Antigravity must not edit those files while
Codex repairs and verifies the M1 integration. No commits, pushes or deployments.

Antigravity owns only these new files for its first M2 slice:

- `internal/adapters/probe/pinned_client.go`
- `internal/adapters/probe/pinned_client_test.go`
- `docs/multi-region/M2_PINNED_CLIENT_HANDOFF.md`

Use Gemini 3.8 Flash in Antigravity. Do not switch models or delegate to DeepCoder.
Read the protocol transport/enrollment sections and architecture security section
before implementing. This slice is one dependency of M2, not M2 completion.

## Frozen adapter contract for this slice

Package `probe` exports:

```go
type EndpointPolicy struct {
    AllowedCIDRs []netip.Prefix
}

func NewPinnedHTTPClient(endpoint, fingerprint string, policy EndpointPolicy) (*http.Client, error)
```

The client is dedicated to the specified endpoint. The composition root uses it
with the existing `coder/websocket` dialer for enrollment or runtime connections.
No provider, database, session or credential storage changes belong in this slice.

- Accept only an absolute `wss` URL for `/ws/probe/v1` or `/ws/probe/enroll/v1`,
  with hostname and optional valid port. Reject userinfo, query, fragment, encoded
  path variants and malformed syntax. Default port is 443.
- Require exactly 64 lowercase hexadecimal SHA-256 characters for the expected
  leaf-certificate DER fingerprint. No empty pin, TOFU, fallback or renewal here.
- Require TLS 1.3 and verify the leaf pin in constant time plus NotBefore/NotAfter.
  Self-signed certificates are intentional: explicit pin verification establishes
  identity. Keep the verification hook mandatory and reject missing certificates.
- Reject redirects. Do not forward authorization to another endpoint. Bind the
  client's dialer to its validated host and port; disable environment proxies.
- Resolve addresses inside the context-aware dial path and dial a checked IP,
  preventing a second unvalidated DNS lookup. Reject unspecified, multicast,
  link-local and cloud metadata destinations (including mapped IPv4 forms).
  Those categories remain forbidden even when a CIDR is supplied.
- With no configured CIDRs, permit public unicast addresses only; private and
  loopback destinations require an explicit matching CIDR. If AllowedCIDRs is
  nonempty, destinations must match it. Validate every supplied prefix.
- Bound dial/TLS/header handshakes to 10 seconds and honor caller cancellation.
  Return errors without credentials or other sensitive URL data. No new modules.
- Do not log request headers, tokens, TLS private keys or endpoint credentials.

## Evidence required from Antigravity

Use real local TLS servers and the existing WebSocket library where useful.
Prove correct pin success, wrong/missing/malformed pin failure, expired/not-yet-valid
certificate failure, TLS 1.2 refusal, endpoint/path rejection, redirect rejection,
and cancellation. Use an explicit loopback test CIDR. Unit-test the destination
policy for private, mapped, unspecified, multicast and link-local addresses.
Show that a client cannot be reused to send credentials to a different host/port.
Run focused race tests, build and formatting; report exact commands and results.
The handoff must distinguish tested transport behavior from the session/enrollment
and durable-runtime behavior still to be implemented by subsequent slices.

## Completion audit to retain across slices

- [x] M0 contract fixtures and existing local behavior verified at final state.
- [x] M1 integrated single delivery owner, automatic refresh, applied settings,
  acknowledgement, escalation, durable retries and restart verified on both DBs.
- [x] Dedicated probe entry point/bootstrap, edge SQLite migrations and exclusive
  data-directory lock; persisted identity, stream, TLS and accepted configuration.
- [x] Actual HTTP/TCP/DNS checks and existing notifier reuse; reject unsupported
  assignments; autonomous check/notification behavior without hub availability.
- [x] Pinned TLS 1.3, endpoint policy, scoped header credentials, one-use local
  enrollment token and crash-safe exchange; wrong credentials/pins fail closed.
- [x] Bounded one-reader/one-writer supervisor, negotiation, deadlines, cancellation,
  fair control/replay scheduling, bounded jitter backoff and session fencing.
- [x] Hub DB connector leases/generations, takeover and stale close-callback tests;
  duplicate running identity diagnostics.
- [x] Durable edge observation/state/intent transaction and direct provider outbox;
  provider failure and restart preserve pending work without blocking recording.
- [x] Useful liveness/readiness during hub outage; no hub auth/admin/frontend APIs
  on the probe; actual manually initialized process smoke and restart proof.
- [x] Full applicable project gate plus relevant live MariaDB and M2 failure tests;
  update authoritative status/plan with evidence, not test counts or percentages.

Checked items reflect Codex's final independent acceptance in M2_ACCEPTANCE_REPORT.md.
Earlier checkpoints below remain historical; a slice handoff alone is not evidence
of milestone completion.

## Integrator checkpoint — 2026-09-20 11:10 local

Antigravity's first M2 assignment was submitted and received in “Check Task
Progress” using Gemini 3.8 Flash High. Its client is now addressing the concrete
review in M2_PINNED_CLIENT_REVIEW.md. The next bounded identity assignment is
prepared in M2_IDENTITY_WORK_CONTRACT.md; it is not yet dispatched.

M1 working-tree corrections now pass the real key-configured two-worker MariaDB
smoke, all 20 SQLite and 20 live MariaDB local-delivery contracts, and the new
escalation rollback/retry/ACK/cold-reader contracts on both engines. Migration 051
downgrade refusal also passed on both engines. A deterministic lock-order regression
passes on MariaDB. Full race suite is running; final lint and overall M2 gate remain
pending. See docs/postmortems/2026-09-20-m1-integration-followup.md for evidence.

## Integrator checkpoint — 2026-09-20, M2 foundations in progress

The earlier checkpoint is historical. Pinned client independently passed the
complete probe package race run. The identity assignment was dispatched, reviewed
and corrected: missing lock-symlink checks, unbounded file reads and insufficient
manifest validation were reproduced and fixed. Antigravity subsequently fixed a
remaining duplicate-key error redaction issue and now owns the three session files
under `M2_SESSION_WORK_CONTRACT.md`. `M2_SESSION_REVIEW.md` records concurrency,
sender-cancellation and peer-error redaction findings awaiting correction.

Codex added dedicated edge SQLite storage (`internal/adapters/repository/edge`),
explicit hash-only enrollment, encrypted monotonic config activation, and the
probe-only TLS enrollment listener. Tests prove transactional rollback, replay
rejection, lost-response recovery, no hub tables, FULL/WAL/FK settings, safe DB-file
modes, cold config retention and generation tombstones. The TLS enrollment test
rejects wrong pins/credentials and reuses only the durable runtime credential.
These components are not yet connected to a probe executable.

Hub migration 052 and `ProbeConnectorStore` implement a database-clock 60-second
lease with monotonic generation. Both engines pass competing independent workers,
renewal, same-owner reconnect, expiry/takeover, stale callback/release rejection,
disabled registration, overflow and downgrade-guard checks. No connector loop is
wired yet.

Independent logs:
- `/private/tmp/phoenix-m2-edge-store-race.log` — edge persistence passed.
- `/private/tmp/phoenix-m2-enrollment-http-race2.log` — real pinned TLS enrollment
  and core enrollment tests passed (the first test's unrelated health client used
  CA hostname validation against a pin-based self-signed identity; route assertions
  were corrected to inspect the handler directly).
- `/private/tmp/phoenix-m2-connector-leases.log` — SQLite and MariaDB passed.
- `/private/tmp/phoenix-m2-edge-config-race.log` — edge storage and service tests
  passed, probe package build interrupted by Antigravity's in-progress unused test
  import. Re-run after stable session handoff; no passing probe result claimed.

New `EdgeConfigDecoder` accepts the M2 HTTP/TCP/DNS scope with existing provider,
template, proxy and maintenance validators. It explicitly rejects enabled
connection watchdogs (M3), certificate paging and active escalation (M4), and remote
ACK links until command routing exists. None are silently ignored. Exact original
transfer bytes are now available through `ConfigTransfer.CommitDocument` for
protection without changing their source hash.

Remaining M2 integration: durable edge check/state/telemetry/provider intents and
retry worker, actual scheduler/notifier reuse, complete authenticated negotiation
and duplicate-identity ownership, hub protected credential preparation and connector
loop, `cmd/probe`/bootstrap/operator initialization, real process offline/restart
acceptance, and final full gates. All milestone checkboxes remain pending until
those execution paths and the final gates are independently verified.

## Integrator checkpoint — 2026-09-20 12:38 local

The prior checkpoint is historical. Antigravity delivered session transport and
edge delivery persistence; independent race checks passed. Gemini then hit its
quota before implementing authenticated runtime sessions. The app reports reset
at 12:57:49 Asia/Bangkok. Keep Gemini 3.8 Flash High; do not enable paid overages or
switch models. Codex took over `runtime_session.go/test.go` during the wait. After
reset, dispatch Antigravity to a read-only runtime audit in its own review file.

Codex implemented core edge recording/delivery services, bounded HTTP/TCP/DNS
scheduling, local diagnostics, and `cmd/probe` init/token/inspect/run composition.
Actual checker -> SQLite -> provider tests pass without a hub and after cold reopen.
Maintenance, retry confirmation, resend throttles, stale configuration, removed
channels, incident recovery and provider-error redaction are covered. A 64 MiB
engineering telemetry bound stops recording instead of dropping unacknowledged
history; retained delivery work has its own 64 MiB bound. Claims reserve outcome
space before sending. Full configurable retention/gap policy remains M3.

Authenticated runtime sessions now pass real TLS tests with durable generation
fencing, duplicate rejection preserving the incumbent, replacement, failure receipts,
exact protected config bytes and shutdown. Hub credential protection uses distinct
AES-GCM associated data binding installation, probe, stream, enrollment, version,
endpoint and pin. Migration 053 durably prepares protected credentials and the
trusted stream before I/O; tests on both engines prove atomic rollback, idempotent
retries, recovery of original credentials and guarded downgrade.

Evidence:
- `/private/tmp/phoenix-m2-session-independent.log` — session/identity/config race passed.
- `/private/tmp/phoenix-m2-delivery-independent.log` — Antigravity delivery race passed.
- `/private/tmp/phoenix-m2-edge-services-race.log` — recording/delivery rules passed.
- `/private/tmp/phoenix-m2-edge-scheduler-integration2.log` — real HTTP/webhook offline
  and cold reopen passed; initial test assumed textual webhook status, corrected to
  the existing numeric status plus severity contract.
- `/private/tmp/phoenix-m2-edge-pressure-race2.log` — queue-full rollback and late
  counter fault passed; `/private/tmp/phoenix-m2-delivery-headroom.log` proves outcome reservation.
- `/private/tmp/phoenix-m2-hub-credential-race2.log` — SQLite and live MariaDB passed;
  the first run's test-helper compile error was corrected, not counted as a pass.
- `/private/tmp/phoenix-m2-runtime-session-race2.log` — authenticated runtime passed.
- `/private/tmp/phoenix-m2-integrated-edge-race.log` — CLI, edge store, core services
  and scheduler focused race passed.
- `/private/tmp/phoenix-m2-current-build.log` — `go build ./...` exited 0 (sandbox
  emitted a module stat-cache write warning; final binary build should run with
  ordinary approved test/build permissions).
- `/private/tmp/phoenix-m2-current-lint2.log` — scoped changed-package lint: 0 issues.
- `/private/tmp/phoenix-m2-current-full-race.log` — full suite running, not yet passed.

Remaining critical path: wire an actual lease-owned hub connector and operator
enrollment/config preparation, resume Antigravity review, process-level offline /
restart / wrong-pin-token smoke, then final build/race/lint/make gate-full and live
MariaDB regression checks. Leave everything uncommitted and unpushed.

## Final integrator acceptance — 2026-09-20

M0/M1 corrections and M2 acceptance passed. See M2_ACCEPTANCE_REPORT.md for the
final full gate, full race, 0-issue lint, complete live MariaDB/SQLite contracts,
and two actual process regression reports. Source remains uncommitted/unpushed.
Antigravity contributed four reviewed slices on Gemini 3.8 Flash High; its final
optional audit is pending the native app's log-read approval. Codex independently
reviewed and verified the runtime, so that pending report is not claimed as a pass.
