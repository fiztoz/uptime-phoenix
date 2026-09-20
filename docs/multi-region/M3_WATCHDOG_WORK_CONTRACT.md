# M3 connection-watchdog work contract

This is the next bounded increment after historical recomputation. It is a work
contract, not an implementation or acceptance claim. Preserve the accepted
config/replay/retention/current-state/history changes. Codex owns all source and
integration; no Antigravity process currently owns a file.

## Traced starting points

- `Session` validates health frame role and connection generation. Its transport
  liveness does not implement a durable connection incident.
- `EdgeRuntime` forwards validated hub health to replay/current-state pumps;
  `HubTransport` proves DB lease/writability on received probe health. Neither
  path currently drives a persistent watchdog.
- Config DTOs already contain `watchdog`; the encoder disables it, and the edge
  decoder explicitly rejects enabled watchdog configuration. Do not simply remove
  that guard before implementing source persistence and delivery.
- Edge resolved config has no watchdog settings. Source incident/delivery schema
  currently requires a monitor/generation and cannot represent a probe entity.
- Protocol decoding recognizes `watchdog.transition`, but recognizing it does not
  authorize ingestion, persist an incident or send a notification.
- Existing accepted source delivery machinery supplies bounded leases/retries and
  immutable delivery IDs. Extend shared machinery where its invariants fit;
  never send directly from a socket callback or fabricate a monitor row.

## Required behavior and ownership

1. Add a pure deterministic watchdog state machine in core services. Use elapsed
   monotonic time while a process runs. Valid application health—not a ping,
   handshake, arbitrary frame, queue activity or stale-generation frame—renews
   health. Hub ingest readiness/writability must be true for the edge watchdog;
   hub-side execution health must use the role-appropriate health facts.
2. Defaults are 45 seconds suspect, 90 seconds lost, and 30 seconds continuous
   healthy recovery. A degraded health frame or another disconnect interrupts
   recovery stabilization. Reconnect alone must not resolve an incident.
3. Arm only after a first durable applied configuration, including after restart
   of a previously activated probe. Never page a new unenrolled installation.
   Persist open source incident identity/version and acknowledgement so restart
   cannot create a second outage or resend an initial intent. Process-local
   monotonic baselines reset conservatively on restart; wall-clock regression
   cannot underflow durations or immediately manufacture a recovery.
4. Each side owns its connection incident and its own notification deliveries.
   Use explicit probe entity context and `probe_connection` scope, with null
   monitor/generation on the wire and in storage. The hub must never re-send a
   mirrored edge watchdog transition. Availability alerts remain separate.
5. Persist lifecycle, sequence, exact telemetry bytes and delivery intents in one
   source transaction. Receipts follow commit. Reconciliation before provider I/O
   checks current accepted watchdog settings, channel versions/activity and exact
   incident/version/acknowledgement. Disabled/removed channels suppress stale work.
6. Provide an explicit admin configuration path using the existing operator CLI
   and saved source graph, then include watchdog dependencies in complete snapshots.
   Default local-only deployment stays operational without extra configuration.
   Do not invent a new monitor/provider type or authorization dimension.
7. Hub-side state uses current connector lease/registration/installation fences;
   stale workers cannot commit watchdog progress. Edge session observations are
   generation-fenced. Persist diagnostic status without making link loss a remote
   monitor DOWN: normal regional freshness ages evidence toward UNKNOWN.
8. Keep health/control traffic independent of telemetry backlog, storage retry and
   current-state transfer. Integrate both watchdog loops into real startup and
   graceful cancellation. An isolated timer helper is not milestone acceptance.

Recheck both hub and edge migration directories before reserving numbers. Current
history work uses hub 057, runtime ownership uses hub 058, and current-state uses edge 004; do not assume these
remain the latest if another turn has advanced the repository.

## Acceptance

Use fake monotonic time for exact 45/90/30 boundaries, startup arming, degraded
application health with a live socket, recovery interruption, duplicate health,
stale generation, backward wall clock, restart with firing/acked incident and
config disable/re-enable. Assert durable effects and no duplicate source identity.

Use real SQLite and MariaDB for hub ownership/last-write rollback/competing workers;
use the actual edge SQLite store for lifecycle+outbox atomicity, retries and restart.
Transport tests must prove valid health drives the correct side while backlog is
stalled. Process acceptance must exercise both sides and direct local test-provider
delivery. The final M3 15-minute partition still separately covers pending ACK,
rotations/reset dependencies and exactly-once retained-history effects.

## Accepted timer and ownership prerequisite

History is committed as `97a0df7`. The ownership increment adds a pure timer and
measured loss checkpoints as domain values; persisting those values remains next. Exact-boundary unit tests cover unarmed startup, degraded
application health, interrupted 30-second recovery, restart, duration limits and
pending loss when a healthy frame arrives before a delayed tick. These tests do
not prove watchdog runtime/persistence/provider completion.

Antigravity's no-tools design review in conversation
`e49a801f-64df-443a-a55f-4e0e7c05f882` confirms that connector generation churn must
not reset the outage clock. Its suggested subtraction of DB wall timestamps on
handoff was rejected: a DB clock step could manufacture 90 seconds. Persist
already measured elapsed loss and pending-loss state under a separate fenced
watchdog ownership row; resume that duration conservatively after handoff, without
counting uncertain downtime or inheriting a partial recovery streak.

The implemented ownership rule is that the watchdog lease owner also runs that
probe's connector attempts. The watchdog lease survives reconnect backoff while
per-connection generations still advance. This keeps validated health receipt
and watchdog timing on the same process monotonic clock; another worker takes
both responsibilities after fenced ownership transfer. Do not make an independent
polling watchdog infer precise health timing from another process's UTC timestamp
or from when a DB poll happens to notice a changed counter. Every lifecycle write
must check the watchdog epoch, and health-derived writes additionally obey current
connector generation. Revocation/disable and cancellation stop both loops.

Hub migration 058 now stores durable runtime owner/epoch/lease rows. Production
connector attempts retain that owner across backoff, while child generations
advance independently. Child deadlines are bounded by current parent leases;
release and takeover atomically invalidate child sessions. Real both-engine tests
cover contention, same-owner duplicate loops, stale callbacks, rollback, legacy
adoption, disable, overflow and replay after takeover. The timer checkpoint remains
a domain value: no durable watchdog incident or notification is enabled yet.
The final full Go race gate passed with 202 MariaDB-named pass events and zero
MariaDB skips, zero-issue lint and CGO-free build. Real process evidence proves
enrollment with live workers and stable ownership through edge restart. See
[runtime acceptance](M3_RUNTIME_ACCEPTANCE.md) and
[the retrospective](M3_RUNTIME_RETROSPECTIVE.md).

Enrollment is separate from runtime ownership: a worker waiting for first
enrollment must not monopolize the operator's ability to enroll it. The operator
revalidates enabled registration and the immutable prepared credential via the
existing preparation transaction, then the edge atomically consumes its one-use
enrollment token. It never advances a runtime connection generation.

### Next bounded source transaction

Implement source watchdog persistence, complete config settings and notification
work before enabling either watchdog. Add settings to the protected configuration
graph (local remains disabled), a durable hub/edge watchdog state checkpoint and
probe-scoped incident/delivery context. Every hub write checks current runtime
epoch, registration and installation; health-derived writes additionally check
current session generation. Source transition, exact telemetry bytes, sequence,
checkpoint and delivery intents commit together. Open/acked identity survives
restart; mirror ingestion never generates send work. Extend actual edge schema
and delivery reconciliation for nullable monitor/generation, without fake monitors.

Revisit `withRuntime` when adding the watchdog loop: parent renewal must be tied
to successful bounded watchdog progress/checkpoint persistence, so a stalled
watchdog cannot retain authority forever merely because another goroutine renews
the lease. Keep health receipt and timer mutation on the same process clock.
Recovery continuity cannot be inherited across ownership changes. No subtraction
of saved wall timestamps and no independent poller inferring health receipt time.

Existing runtime migration is hub 058; edge remains 004. Check directories before
reserving the next numbers. No other agent owns source files.
