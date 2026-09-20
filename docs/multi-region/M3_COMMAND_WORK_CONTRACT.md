# M3 durable command continuation contract

Start from the latest committed watchdog acceptance and verify HEAD/worktree. The
user's active objective remains full M3 completion. Codex owns source, tests,
migrations and verification; Antigravity owns no files. Preserve unrelated
`AGENTS.md` changes. Do not push or deploy.

## Traced starting points

- `internal/adapters/probe/commands.go` defines closed request/result DTOs for
  `alert.ack`, stop/history-clear and credential/certificate prepare/activate.
  Decoding is explicitly not execution. `runtime_session.go` has no command
  execution branch; no durable command result should be inferred from a socket.
- `domain.ProbeCommand` holds metadata only. Hub migration 036 defines a command
  table, but there is no edge applied-command table yet. Inspect the real schemas
  and current migration maxima before choosing additions; do not treat the design
  schema table as implemented storage.
- Source incident IDs, transition versions, ACK fields and replay receipts exist.
  Watchdog replay deliberately rejects uncorrelated ACK metadata. Extend the same
  `AccessService` authorization path with verified command facts, rather than
  trusting actor text sent by an edge.
- The current `alert.ack` wire target requires positive assignment generation.
  Begin with incident-specific regional ACK as required by the long-partition
  acceptance. If extending it to probe watchdog ACK, reconcile the explicit null
  monitor/generation contract first; never invent an assignment generation.

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
