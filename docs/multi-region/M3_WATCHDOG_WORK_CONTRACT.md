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

Runtime migration is hub 058. Edge migration 005 now implements the source
checkpoint/incident/telemetry/delivery transaction; see
[M3_WATCHDOG_SOURCE_ACCEPTANCE.md](M3_WATCHDOG_SOURCE_ACCEPTANCE.md). Check the
migration directories before reserving the next numbers. No other agent owns
source files. The enabled-watchdog config guard remains.

The next source adapter must distinguish hub-owned connection incidents from
mirrored edge connection incidents even though both have the remote probe identity.
A known hub source UUID must not become writable through edge replay. Preserve
that ownership for all historical incidents, not only the currently open one.
Hub source sequences must not advance the remote edge stream cursor. Extend the
existing outbox with explicit probe scope; do not fabricate monitor/assignment
values to satisfy old schema constraints. Retained config revision/channel
membership must authorize mirrored watchdog events and delivery results without
requiring a monitor assignment. Original wall clocks may regress; source versions
and sequence remain the order.

The edge storage transaction preserves a last resolved identity across unarming.
Treat `Status != resolved` as open; nonnil is insufficient. Recovery from firing
cannot invent ACK metadata, and healthy checkpoints cannot enqueue recovery work
without a new transition. Generation-fence every checkpoint derived from health;
do not relabel delayed stale health as a generation-zero local timer write.


### Hub source counterpart

Hub migration 059 now implements source ownership, journal/checkpoint and nullable
probe outbox context. See [hub acceptance](M3_HUB_WATCHDOG_ACCEPTANCE.md). The new
`ProbeWatchdogRepository` port is implemented by the hub store and private edge
store. This does not enable runtime, provider sends or wire watchdog mirroring.

The next implementation should complete config and runtime as one integration:
operator settings and protected dependency closure; explicit probe name/location
for notification context; both application-health callbacks; source service/timer
coordination; mirror authorization; actual source delivery reconciliation. Keep
claims scoped to tested behavior until both process paths work. Preserve the
existing shared outbox rather than add a second notification queue.

When authorizing a resend result, its channel version can refer to a newer accepted
config than the incident's opening revision. Resolve channel/watchdog membership
from that delivery version while retaining the exact source incident transition;
do not require a new firing lifecycle event merely because channel config changed.
A hub source journal never increments an edge replay cursor. Current runtime
ownership and health generation remain mandatory even when callbacks are delayed.

### Runtime ingress audit and integration constraints

The read-only Antigravity follow-up (same `b5e7f7af` conversation) confirms that
`Session.readerLoop` currently executes the supplied callback synchronously. Both
hub replay ingestion and edge config application can delay reading later health
frames. Outgoing priority queues do not solve this incoming blocking. Introduce a
bounded receive dispatcher: one ordered non-health worker and a separate ordered
health worker. Capture a `time.Time` retaining its process monotonic component at
read completion, before queueing; never use the time a worker happens to run or
`health.clock_time` as receipt time. Validate role/generation before admission.

Keep every healthy and unhealthy sample in order. Queue overflow must close the
session and interrupt stabilization, rather than silently replacing/dropping an
unhealthy sample. A full queue cannot safely receive a synthetic replacement
sample as suggested in the audit. Both workers need bounded contexts and must be
joined on cancellation; non-health ordering and durable-receipt-after-commit stay
unchanged. Prove behavior with stalled real replay/config handlers, not only a
standalone timer test. The hub lease callback already captures its generation in
`connectOnce`; retain that fence when adding explicit health observation inputs.

Keep the watchdog owner outside individual sessions and retain each sample's
generation through all later health-derived checkpoints. A stale in-flight sample
must never be relabeled as an unfenced local tick after reconnect. The parent
renewal progress deadline must advance after durable checkpoint progress, not
after evaluation alone. An in-memory timer that runs while storage is stuck does
not prove useful progress. These are integration requirements, not implemented
runtime acceptance.
