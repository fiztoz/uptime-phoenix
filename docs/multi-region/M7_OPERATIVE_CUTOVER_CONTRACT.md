# M7 operative aggregate/both cutover — implementation contract

Authorized 2026-10-14 after `919ffd2`: implement the full operative lifecycle behind
an explicit opt-in, then publish the feature branch and open a PR. No deployment
or live activation is authorized. This document is a work contract, not acceptance.

## Feature and authority boundaries

- Hub `PROBES_AGGREGATE_PAGING_ENABLED=false` by default; true requires probes,
  protected configurations and the operative coordinator/provider stores.
- Edge advertises exact `availability.delivery_cutover.v1` only when the compiled
  executor, durable source gate and provider-attempt journal are configured. Hub
  welcome `delivery_cutover` is strict optional bool, absent=false. Desired-mode
  parsing, regional application and delivery.prepare capabilities do not substitute.
- Persisted gates are enforced even when the opt-in is subsequently disabled.
  Disable stops new aggregate admissions and drains old barriers; it must not
  silently restore regional starts. Probes must remain running to finish drain.
- All assigned members, including local/paused/disabled/offline/missing, are required.
  No standby plan is promoted. Genuine aggregate intents/incidents are separate.
- Every authority write rechecks its stored owner/epoch/expiry, current set,
  generations, streams, exact retained configuration and prerequisites atomically.
  Queue ordering, copied receipts or a caller clock do not establish authority.

## Lifecycle

1. PREPARING: current shared preparation round/nonce and complete immutable member
   set. Independently prove selected protected config/ACK/binding/regional fact and
   exact successful preparation for each member. Missing is pending, corruption
   explicitly fails. No mode change or send permission.
2. QUIESCING: append a UUID barrier with exact all-member tuples and issue protected
   delivery.quiesce commands. For aggregate, sources atomically block new regional
   availability provider starts and enqueue/escalation intents for the monitor,
   even across later config changes/reconnect/reset. BOTH retains regional work.
   Checks, certificate/capacity/probe/folder alerts remain independent.
3. QUIESCED: only after every exact source quiesce success commits and no unresolved
   earlier regional provider attempt remains. The source persists its hold before
   checking attempts; busy/ambiguous refusal is not a success. Retries use new
   immutable command IDs against the same barrier, never rewrite old outcomes.
4. APPLYING: issue delivery.apply only after the complete quiesced barrier commits.
   Source authenticates one captured config and proves the exact quiesced gate;
   atomically persists aggregate/both effective application and kind-specific receipt.
5. ACTIVE: only after all exact apply successes commit. All-source bindings remain
   current at each aggregate enqueue/claim/final provider authorization. Application
   evidence is distinct from configuration ACK and preparation. Pure cutover evaluator
   is reused through a core evaluator port, not treated as a stored permission.
6. DRAINING: revision/policy/mode/document/generation/stream changes, member retirement
   or feature disable stop aggregate admission. Supersede unstarted intents. No source
   can resume regional until every admitted hub provider attempt is resolved. Then
   delivery.resume releases each original source gate through an immutable receipt;
   old members are not dropped from drain membership. New barriers cannot bypass it.

A source hold is monitor-wide within the bound installation, not merely keyed to
an old document. Config changes, expiry, disconnect or reset cannot erase it.
Resume authenticates the stored barrier tuple; it may legitimately release an old
configuration after replacement, but never release a replacement barrier.

## Provider attempts and ambiguity

- At final availability authorization, journal an opaque UUID attempt grant in the
  same DB transaction as stored claim/config/gate/lifecycle checks. One deadline
  covers authorization plus provider I/O; the stored lease must cover that budget.
- Source and aggregate send services use the stored authorized work, not caller
  snapshots. Finish the exact grant only after Send returns; completion of this
  release may be valid after expiry and never affects a replacement grant.
- Provider success records succeeded; provider error records ambiguous, and an
  unfinished/crashed grant remains in_flight and unresolved. A genuinely pre-I/O
  refusal records not_started. No timer, lease reclaim, cancellation or restart
  proves no I/O.
  Ambiguous grants block quiescence/drain, not ordinary regional retry/recording.
- Upgrade must conservatively capture already-attempted legacy availability work
  whose I/O completion cannot be proven. No success/backfill claim is fabricated.
- No DB lock spans provider I/O. No exactly-once external delivery is claimed:
  provider acceptance and local outcome persistence are not one transaction. An
  accepted remote notification can appear later. The guarantee is fenced new
  request admission and honest unresolved ambiguity, not external effect timing.
- This slice supplies neither automatic nor operator ambiguity clearance. A
  future safe recovery contract needs separate authorization and verification;
  current holds may remain blocked indefinitely. Never invent success or historical
  completion times to unstick drain or permit downgrade.

## New shared core contracts (main owns these files)

`internal/core/domain/operative_delivery.go` defines:
- DeliveryCutoverCommand: embedded ProbeDeliveryModeApplication; CommandID,
  BarrierID, PreparationCommandID, Kind, CreatedAt, ExpiresAt, PayloadHash.
- ProviderAttemptState: in_flight/succeeded/ambiguous/not_started.
- AggregateCutoverProgress: MonitorID, BarrierID, Phase, Changed.
- AggregateProviderGrant: AttemptID, Delivery QueuedDelivery, Monitor, Notification,
  Template, IncludeTarget, regions and policy; internal settings, never HTTP DTO.

`internal/core/ports/operative_delivery.go` defines codec, edge executor/repository,
source provider journal, local final authorization, hub coordinator and aggregate
provider repository/evaluator ports. Signatures are authoritative; read them first.
QueuedDelivery.ProviderAttemptID is an internal returned grant identity; it is not
serialized into existing wire frames or HTTP views.

## Wire additions (wire agent owns)

Kinds delivery.quiesce/delivery.apply/delivery.resume. Closed data is the existing
preparation tuple plus barrier_id and preparation_command_id (canonical UUID).
Counters remain canonical positive signed-int64 decimal strings; monitor_id is
positive JSON integer. Target generation required, target source_alert_id null.
Success codes are kind-specific delivery_quiesced/delivery_applied/delivery_resumed;
applied/already_applied have applied_at, null details. Code-empty success for these
kinds is invalid. Existing kinds and compatibility fixtures remain unchanged.
Hub commands reuse protection, session fences, dispatch/result journals and capacity.
Source duplicates recover their exact historical kind receipt before new config
load, under accepted stored session authority. Recovery is not new eligibility.

## File ownership

- Main: this contract, new shared domain/ports, QueuedDelivery grant field, bootstrap
  config/run, cmd/probe composition, source/local provider service integrations,
  operative aggregate service, complete integration/acceptance, docs/gate scripts/PR.
- Edge worker: NEW edge operative-delivery files/migration017/tests; existing edge
  delivery_authority.go, record.go, escalation.go; NEW core/services/edge_delivery_cutover.go
  and tests. No existing core service or domain/port edits.
- Hub worker: NEW repository operative cutover/local authorization/aggregate delivery
  files/tests; SQLite/MariaDB082 up/down; existing repository/probe_command.go for
  capability-filtered new kinds. No bootstrap, services, probe wire or shared edits.
- Wire worker: NEW probe delivery_cutover codec/wire/tests; existing probe commands.go,
  command_runtime.go, handshake/envelope/hub_transport/runtime_session; existing
  domain probe_command.go/probe_credential_rotation.go validation only. No repository,
  bootstrap, cmd, shared operative types or source service edits.

## Acceptance required before PR

Real SQLite and MariaDB exact leaves; encrypted edge SQLite; pinned TLS sessions;
actual recording providers. Regional default unchanged; BOTH sends both scopes;
AGGREGATE starts only after all-source application, then sends aggregate only.
Unknown/PENDING/maintenance never fake recovery; ANY/ALL complete quorum; large
revision precision; cold restart, reconnect/duplicate/lost receipts; worker before
enrollment; mixed unsupported/offline members; stale owner/epoch/expiry/stream/config;
queued and in-flight switch/drain; provider error/crash ambiguity blocks progress;
ABA and removed members drain safely; certificate/capacity/watchdog independence;
transaction rollback/concurrency/clock rollback and populated migration/downgrade.
Main executes build/full race/lint/vet/parser/mutation gates and reports skipped
coverage separately. Release vulnerabilities and operational/load/soak/canary gates
remain separate; no live activation or release acceptance is implied by a PR.
