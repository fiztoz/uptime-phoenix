# M3 ordered replay increment — 2026-09-20

Baseline: `d3eea61`, branch `codex/multi-region-probe-plan`. The user requested
continuation through Antigravity and proper commits. Codex owns integration,
independent verification, final documentation and commits. Nothing is authorized
for push, deployment, paid overages or model changes.

## Deliverable

Connect the existing durable edge telemetry outbox to authenticated, fenced hub
ingestion for the implemented HTTP/TCP/DNS availability runtime. Replay the mixed
ordered stream of `observation`, `alert.transition`, and `delivery.result`; return
durable ACK/rejection outcomes; survive a lost ACK and process restart without
duplicate history or provider sends. Source recording and delivery keep working
during a hub outage. This increment does not complete all M3.

Retention eviction/gaps, current-state snapshot recovery, watchdogs, remote
commands, auxiliary alert capabilities and fleet UI remain later work. Unsupported
semantics must fail explicitly; do not advertise or silently accept them.

## Coordination and completed ownership

Codex used Antigravity (Gemini 3.8 Flash High) through the installed `agy` CLI
because native UI input was unreliable. `/grill-me` identified the authority,
transaction and ACK boundaries. A broad `/goal` was interrupted after repeated
guessed-path reads; its saved partial files were not accepted as an implementation.

Fresh bounded goals owned only edge `replay.go`/`replay_test.go`, then probe
`edge_replay.go`/`edge_replay_test.go`/`runtime_session.go`, each with a separate
handoff. Frozen domain/port signatures and exact file maps preceded delegation.
All implementation ownership has now returned to Codex. A final `/learn` task
wrote only [the retrospective](M3_REPLAY_RETROSPECTIVE.md). No agent is authorized
to resume editing source from an older task.

Antigravity used file tools only because its headless command calls could not
prompt for permission. No permission bypass or model change was used. Codex owns
all builds, tests, live database/process checks, final corrections and commits.
Authored tests are **not run by Antigravity**. Handoff reports are historical
claims, superseded by [independent acceptance](M3_REPLAY_ACCEPTANCE.md).

## Decisions answering the planning questions

### 1. Mixed events and typed results

Add a small dedicated replay domain/port contract instead of widening the older
observation-only `ProbeIngestRepository` into runtime authority. Keep that legacy
foundation and its callers unchanged. The new boundary carries pure domain
values for the three supported event types, trusted session authority, sequence
bounds and a structured result with committed cursor, accepted count, duplicate
count and permanent rejection codes. No HTTP, Bun, SQL, transport DTO or JSON tags
in core types. The adapter maps explicit wire DTOs; never marshal a domain type.

Strict wire decoding validates framing, types, sizes and contiguous sequence
coverage before storage. A malformed envelope, sequence gap, wrong stream or
stale session is not a per-event permanent rejection; it must not advance progress.
Existing decoders and limits are the contract; do not invent a second dialect.

### 2. Historical authority and durable outcomes

Extend the authorization choke point through an `AccessService` method in the new
authorization file. The repository captures transaction-bound authority facts;
the core method decides from those facts, with no database/framework imports.
Do not scatter independent authorization rules through transport handlers.

The transaction checks enabled enrolled probe, installation/stream identity,
current connector owner/generation and unexpired **database-clock** lease. Lock
probe before dependent rows. Serializable authority reads prevent stale facts;
reuse bounded MariaDB deadlock retries where concurrent assignment writes
acquire the assignment-set lock first. Unknown or
retired streams do not get auto-created by telemetry. Enrollment already owns
stream registration. Recheck authority within the same transaction as writes.

An observation or incident requires the exact retained protected config revision,
authorized monitor/generation and supported semantics. Historical generations
also require membership at the source observation time within persisted
`[started_at, ended_at)` assignment intervals and a bounded history horizon (the
existing seven-day target is the increment's explicit default). Do not recreate
a deleted monitor. Removed/revoked or invalid events get stable redacted rejection
codes. Current-state updates additionally require the currently active assignment
and increasing source sequence; old generations are historical evidence only.
Future-dated evidence must not become current merely because it just arrived.

Use additive paired migration 054 for durable telemetry receipts, including
permanent rejections and enough accepted incident-transition metadata to correlate
later delivery outcomes with the exact authorized transition/configuration. Reuse
existing stream/history/mirror tables; do not add a second heartbeat pipeline.
Receipt rows are outside partitioned heartbeat data. Bound fields and batch sizes;
retain receipts until the later explicit stream/retention policy, and document
that no receipt-pruning policy is implemented here. Down migration must refuse
to discard retained receipts.

Accepted writes, rejection receipts, dirty buckets and cursor advancement commit
together. Any retryable storage failure rolls back the entire new suffix. Return
no successful ACK before commit. Replaying a duplicate prefix must not rewrite
history, dirty buckets, incident state or timestamps; return recorded rejection
outcomes for retried rejected sequences.

### 3. Mirroring is never provider work

Process events in strict stream order within the transaction. A delivery result
requires an already accepted incident transition, earlier in the batch or stored
previously, and the channel/version authorized by that exact transition's retained
configuration. A scalar `version <= latest incident version` is insufficient if
the referenced transition was never accepted. Verify incident/notification/event
identity and monotonic outcome progression. Missing or unauthorized parent data
is a durable permanent rejection, not an endless transient retry.

Reuse `putIncidentTx` and `putDeliveryTx` where their invariants fit. Preserve the
guard against collision with hub-owned source delivery intents: do not pass
`sourceOwned=true` merely because a remote event has a source ID. Remote mirrors
do not own the hub's sending outbox. No writes to `alerts`, `alert_escalations` or
`probe_delivery_intents`; no `HeartbeatService.Record`, dispatcher, notifier or
provider calls. No event publication inside the transaction.

### 4. Exact edge replay and ACK pruning

Expose a small edge repository port returning bounded contiguous batches of the
**exact stored event bytes**, starting after the durable edge committed cursor.
Use the private store's writer/identity fence for reads and ACK commits when
needed. Do not re-evaluate observations or rebuild payloads from mutable settings.
Honor the complete-envelope 512 KiB limit, 256-event limit and 64 KiB event limit.
Detect missing sequences and oversized/corrupt rows explicitly; never skip them.
Handle signed-64-bit exhaustion without overflow.

One in-flight batch per established session, sent by `Session.SendReplay` so the
existing single writer and control priority remain intact. Retry the same batch
after a bounded timeout or `telemetry.retry`; do not create unbounded goroutines.
The handler validates ACK stream, generation, cursor range and counts/rejections
against the current in-flight batch. ACKs prove the request prefix through its
last sequence; they never request deletion beyond the batch. Retried rejected
sequences remain in `rejected`, while `duplicate_count` counts accepted duplicates
only, so outcomes partition the batch exactly. An ACK cannot acknowledge unsent data.

After a matching durable ACK, atomically advance `edge_identity.committed_seq`
and delete acknowledged telemetry rows under the exact current installation,
probe, stream and connection-generation fence. Rejected rows are also covered
by that durable cursor; retain diagnostic rejection evidence on the hub. A failed
local ACK commit retains both the prior cursor and all corresponding rows. A
stale callback cannot prune. Health and welcome frames never authorize deletion.
On reconnect, replay from the local durable cursor so a lost ACK is repaired by
duplicate processing. A restored hub cursor below already-pruned history blocks
visibly pending future gap/reset support; never fabricate missing evidence.

### 5. Runtime wiring and health

Wire ingest through the connector's trusted lease, using a typed callback or
service port; transport never manufactures authority from a payload. The hub
ACK/retry adapter uses the real result, and retries preserve the committed cursor.
Authentication/fence failures close the connection. Health reports ingest
ready only after the ingest path is configured and a connector DB write confirms
lease authority. Health refreshes that proof; ingest rechecks authority inside
its own write transaction. Do not replace `ingest_unavailable` with unconditional success.

Edge replay workers and callbacks terminate with the session. Config transfer,
health, cancellation and control fairness must keep working during a backlog.
Runtime startup in `cmd/probe` and normal opt-in hub workers must wire the real
implementations. No manual-only fixture path or 2xx/no-op success stub.

## Independent acceptance

Antigravity writes focused tests and hands off exact files/limitations. Codex then
formats and compiles, traces the real entry points using the Scrutinize workflow,
and runs the following before committing:

1. SQLite and real MariaDB mixed-batch contracts: duplicate/lost-ACK replay,
   permanent rejection followed by valid data, unknown stream, expired/stale lease,
   config and channel authority, retired generation history without live update,
   current-state sequence guards, conflicting incident/delivery identity, injected
   late failure rolling back receipts/history/cursor, and guarded 054 downgrade.
2. Edge storage contracts: exact byte preservation, bounded reads, missing rows,
   signed-64-bit boundary, stale ACK, ACK above sent range, retry/cancellation,
   late ACK-store failure, reopen/restart, concurrent source recording.
3. Real TLS transport: one in-flight batch, lost ACK/reconnect, retry, control
   fairness, stale generation, shutdown with active backlog, no duplicate sends.
4. Extend the real two-hub-worker smoke: backlog created offline, edge restart,
   reconnect drains history and outcomes, durable hub/edge cursors agree, no
   duplicate mirror rows and no extra provider notification caused by replay.
5. `make gate-full`, CGO-free binaries, zero new lint issues, clean diff check.

Do not claim a complete milestone from a helper test. Distinguish authored tests,
tests actually executed, skipped engines and test failures. No repeated full gates
without a changed implementation, failed gate or unresolved concern. Codex alone
owns shared MariaDB and process test ports. Final accepted changes are committed
in coherent conventional commits, with current status and learning notes.
