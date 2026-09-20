# M3 first increment: automatic remote configuration synchronization

The user authorized continuing into M3 after the M2 audit on 2026-09-20.
Deliver one complete increment before replay: saved hub assignments and supported
dependencies become protected per-probe desired snapshots automatically, and a
validated `config.applied` receipt is durable under the current connector lease.
This increment does not claim completion of M3 replay, snapshots or watchdogs.

## Ownership — updated after input-control failure

Codex owns **all implementation files**, including the encoder files listed below.
The computer-use connection could read Antigravity but could not deliver the new
assignment (`noWindowsAvailable` / clipboard timeout). No implementation was
assigned successfully. Codex took over to keep the increment moving.

If Antigravity receives this contract later, its replacement task is read-only:
review the implementation and edit **only**
`docs/multi-region/M3_REMOTE_ENCODER_HANDOFF.md`. Use Gemini 3.8 Flash High;
no source edits, shared MariaDB, commits, pushes, deployment, model change or
paid overages. Do not follow the obsolete implementation ownership below.

Original encoder file scope, now owned by Codex:

- `internal/adapters/probe/config_encoder.go`
- `internal/adapters/probe/remote_config_encoder.go`
- `internal/adapters/probe/remote_config_encoder_test.go`

## Encoder implementation contract

Implement `RemoteConfigEncoder` with
`EncodeRemote(domain.LocalProbeConfigDefinition) ([]byte, error)`.
The existing domain definition is a resolved dependency graph; its historical
name does not grant local identity to remote content. Codex owns the source
reader, resolver and new port. Keep `LocalConfigEncoder.EncodeLocal` behavior.

Reuse one common explicit-DTO encoding helper extracted from `EncodeLocal`;
avoid a second copy of the entire encoder or marshaling any domain struct.
The remote method requires a valid non-local target, preserves exact canonical
probe identity, and validates with `DecodeConfigSnapshot`. Remote channels always
set `include_ack_url` false without mutating the source notification objects.
Keep paused assignments, disabled referenced dependencies, dependency versions
equal to snapshot revision, sorted deterministic output, limits, UTC times and
all existing graph validation. Do not silently strip unsupported monitors,
active escalation or certificate paging: reject unsupported semantics (the
integrator also runs the installed EdgeConfigDecoder before publication).
The current supported edge subset is HTTP/TCP/DNS, direct notification delivery,
maintenance, templates and proxies. Watchdog stays explicitly disabled until its
later implementation. Do not add capabilities, providers or monitor types.

Tests must prove: remote UUID remains in bytes; source IncludeAckURL=true becomes
false remotely but remains true locally and on the input object; deterministic
bytes across dependency ordering; exact revision on dependencies; unsupported
work fails visibly; local regression tests still pass. No checks/provider I/O.

Run focused probe tests with `rtk proxy env GOTOOLCHAIN=go1.26.6
GOCACHE=/private/tmp/phoenix-go-cache-template-race`. Record actual commands/results
and skipped suites. Do not run shared MariaDB tests. Stop after the handoff.

## Integrator design

- Generalize existing source graph reads and pure dependency resolution while
  retaining local wrappers and existing local semantics.
- Reconcile only enabled registered remote connections. Use one serializable
  hub transaction to read authorized source, compare with the last desired
  snapshot using fixed revision/timestamps, and insert a new encrypted snapshot
  atomically. The retained latest snapshot versus the existing active receipt is
  durable sync state; no extra queue or migration is needed. A no-op keeps revision and ciphertext.
- Persisted hub source remains the reconciliation input across missed events,
  API/worker splits and restarts. Connector reconciliation retries independently
  of in-memory hints. No successful network write is treated as application.
- Validate complete documents through installed edge semantic validators before
  publication. Invalid source retains prior accepted edge configuration and
  exposes a bounded error; it never silently drops unsupported assignments.
- Persist applied revision/hash/count/time only for an exact retained desired
  snapshot and the current unexpired connector owner/generation. Delayed receipts
  cannot regress state. A lost receipt is recovered by transferring identical
  content again and receiving the edge's idempotent application response.
- Run SQLite and live MariaDB contracts, actual hub/probe process verification
  for source edits and cold restart, then the repository gate.

## Review lesson carried from M2

Antigravity's final M2 report separates untested concerns from findings. The
integrator reran runtime/session/connector race tests successfully. Caller write
cancellation is checked at the writer boundary and linked to the actual socket;
an interrupted in-flight write closes the session. Transport uncertainty still
requires idempotent application receipts, which this increment persists.
Target fault injection at the final intended SQL statement, not an earlier
no-op writer lock. A database error before inserts does not prove late rollback.
