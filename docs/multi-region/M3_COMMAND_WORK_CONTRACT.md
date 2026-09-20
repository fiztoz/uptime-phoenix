# M3 durable command continuation contract

Start from the latest committed source ACK acceptance and verify HEAD/worktree. The
user's active objective remains full M3 completion. Codex owns source, tests,
migrations and verification; Antigravity owns no files. Preserve unrelated
`AGENTS.md` changes. Do not push or deploy.

## Traced starting points

- `internal/adapters/probe/commands.go` defines closed request/result DTOs for
  `alert.ack`, stop/history-clear and credential/certificate prepare/activate.
  Decoding is explicitly not execution. `runtime_session.go` has no command
  execution branch; no durable command result should be inferred from a socket.
- `domain.ProbeCommand` still holds hub metadata only. Hub migration 036 defines
  a command table without protected request/result storage. Edge migration 006
  now implements applied-command receipts; see the completed increment below.
  Inspect real migration maxima before adding hub storage.
- Source incident IDs, transition versions, ACK fields and replay receipts exist.
  Watchdog replay deliberately rejects uncorrelated ACK metadata. Extend the same
  `AccessService` authorization path with verified command facts, rather than
  trusting actor text sent by an edge.
- The current `alert.ack` wire target requires positive assignment generation.
  Begin with incident-specific regional ACK as required by the long-partition
  acceptance. If extending it to probe watchdog ACK, reconcile the explicit null
  monitor/generation contract first; never invent an assignment generation.

## Completed source ACK increment

See [source ACK acceptance](M3_EDGE_ACK_ACCEPTANCE.md). `edge.Store` now implements
`ports.EdgeCommandRepository.ApplyAlertAcknowledgement`. It accepts validated
`domain.ProbeAlertAcknowledgement` plus authenticated `EdgeCommandAuthority`.
Supply SHA-256 of the **exact command payload bytes**, excluding the reconnecting
session envelope, and preserve these bytes in the hub protected command row.
Current connection generation is checked on every call but excluded from immutable
request identity. Do not cache the original session generation in the command.

The source transaction persists the effect, ACK transition and immutable outcome;
retries recover it after reconnect/restart/expiry. Missing and wrong-generation
targets return durable `rejected/target_not_found`; resolved targets return
`already_resolved`; a second command on an acknowledged target returns
`already_applied` without replacing its original actor. Per-command lifetime is at
most seven days; creation more than thirty seconds ahead is rejected. Receipts
are capped at 16,384 rows and retained through expiry plus 365 days (the maximum
configurable edge telemetry horizon). Capacity pressure fails without applying.

`EdgeCheckRecord.ExpectedIncidentVersion` independently fences ACK changes.
Keep it correct in every source caller/test: no fake observation is allocated for
an ACK. Recovery preserves ACK fields and opens a fresh identity on a later
outage. `EdgeDeliveryRepository.AuthorizeEdgeDelivery` is wired into the live
regional provider service; it atomically rechecks source, exact applied config,
claim and lease budget before I/O. Already authorized external I/O may complete.

The source command port is deliberately **not wired into the network runtime**
yet. Hub ACK replay still rejects uncorrelated metadata. No CLI/HTTP path or
complete offline-ACK claim exists in this increment.

## Immediate integration boundaries

1. Extend hub `probe_commands` with immutable hub/probe/stream identity, protected
   exact request, digest, result and bounded dispatch/retry metadata. Both SQLite
   and MariaDB need real authority/contention/receipt tests. Reuse the installation
   key with a dedicated authenticated command purpose; credential protection is
   deliberately specific to a runtime token and cannot encrypt arbitrary payloads.
2. `RunProbeAdmin` is an existing local-operator composition root: DB/key access is
   its authority. Add explicit issue/read metadata commands without exposing
   ciphertext/plaintext secrets. An HTTP path, if added, must use `AccessService`
   and existing admin middleware. Do not introduce a new permission dimension.
3. Attach bounded command dispatch/results to `HubTransport` and command execution
   to `EdgeRuntime`. Preserve independent health, current-state transfer and
   ordered replay, fence current stored owner/session at each DB write, and cancel
   and join sender work. Reconnect retries must preserve request UUID and bytes.
   Do not mark an attempted command remotely expired merely because its hub TTL
   elapsed: the source might have applied it and lost the receipt. Resend the
   same request after expiry to recover its stored result; a source that never
   applied it returns a durable expired result. Keep remote confirmation explicit.
4. Extend `AccessService.AuthorizeEvent` with facts from durable issued commands.
   ACK actor text is never authority. Validate exact probe/stream/source incident,
   assignment generation and actor/note correlation; allow the issued command's
   transition before its result receipt arrives. Preserve that metadata through
   recovery. ACK of a retained old assignment must use the original authorized
   incident, not require it to still be actively scheduled. Opening new incidents
   keeps normal assignment/config authority. Test out-of-order control receipt vs
   telemetry and unissued/conflicting command metadata on both hub engines.
5. Only then enable the operator/transport path and run actual lost-request/result
   and restart integration. Do not claim command completion from decode, enqueue,
   socket write, source-only tests or a mirrored ACK without the required receipt.

## Required next behavior

Persist a hub-issued immutable command UUID, exact target and protected request
before sending. Reuse the fenced connector and independent control path. On the
edge, authenticated installation/stream/session authority, exact request identity,
expiry and target validation precede a transaction that applies the effect and
records its immutable result. Repeating an identical command returns that result;
reusing its UUID for different bytes/target is rejected. Receipt loss and both-side
restart cannot apply the action twice. No DB lock spans network/provider I/O.

ACK must reference the original source incident and assignment generation, not a
monitor's current open incident. The source transaction records its ACK transition,
actor/command correlation and suppression effects together. It preserves telemetry
sequence and cannot ACK a later outage. An already-resolved target returns the
specified `already_resolved` result; the hub shows pending until durable source
confirmation. Preserve local legacy acknowledgement links. Remote links remain
omitted. Expose a concrete operator path through the existing admin CLI, and use
existing authorization conventions if adding an HTTP path.

Retain idempotency records at least through expiry plus reconnect/history horizon.
Retries use bounded backoff and bounded outstanding work. Unknown/unsupported
commands fail honestly. Avoid a generic shell-command execution facility or a new
permission dimension. Secrets never appear in read results or logs.

Continue credential/certificate rotation as the specified prepare/activate protocol
with stable rotation ID, bounded overlap, persisted pending identity and lost-ACK
recovery. Certificate private keys stay on the edge. Explicit stream reset must
preserve evidence/gaps and reject implicit reset after a database rollback. These
remain required M3 work, not accepted by the watchdog process run.

## Acceptance

Unit tests cover target/lifecycle/expiry policy; real SQLite and MariaDB cover
immutable command identity, atomic effect/result/telemetry, rollback, competing
owners, stale sessions and durable duplicate receipts. Real transport tests lose
requests/results and reconnect with original UUIDs. The real fifteen-minute
partition queues an ACK while offline, restarts the edge, exercises target
failure/recovery, and proves the ACK applies once only to its original incident. Preserve
fresh current state ahead of replay, once-only retained history, provider isolation,
queue pressure reporting and bounded shutdown flushing. Run the full repository
gate, record actual engine cases and skips, and commit coherent increments without
calling the entire milestone done before its completion contract is satisfied.
