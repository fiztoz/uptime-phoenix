# M3 explicit stream-reset work contract

Date: 2026-09-21. This is the next implementation contract, not an acceptance
claim. Finish and verify hub certificate rotation first. Codex owns all files,
implementation, tests and commits. Antigravity owns no files. No push/deployment.

## Scope and operator authority

An ordinary restart preserves the current stream and monotonic sequence. A
restored older edge database must fail reconciliation rather than silently reuse
sequence numbers. Explicit recovery uses a new UUID epoch, the same probe identity,
verified TLS pin and runtime credential, with deliberate authorization by both the
stopped-edge operator and hub administrator. Existing enrollment binding must match.
Full credential/key loss is separate verified reenrollment, not a pin bypass.

Provide concrete hub admin and stopped-edge CLI adapters for M3. Fleet HTTP/UI
remains later work; the existing reset HTTP decoder is not an implemented endpoint.
Every request has a stable reset UUID, expected old stream, new stream, trusted
hub/probe/enrollment identities and the recorded evidence needed to compare source
and hub progress. Repeating the same request recovers its original outcome;
changed arguments or a reused/retired new epoch conflict. Use explicit DTOs and
metadata-only operator receipts. Never include runtime tokens or private keys.

## Hub preparation, activation and peer confirmation

Preparation must serialize against current configuration/registration/session
writers, snapshot the original cursor/identity, and revoke existing connector
admission. Prevent old sessions from ingesting, confirming commands or renewing
usable authority while the prepared reset waits. Preserve its old cursor and
historical receipts/gaps; do not zero or delete the old stream. Reserve and persist
the new epoch/operation before directing source mutation.

After the stopped source commits the matching reset, hub activation atomically
retires the old epoch, creates the new cursor at zero, authenticates the current
token with old metadata and reseals the unchanged token under the new stream,
invalidates stale live projection and advances session fencing. Record explicit
reset/discontinuity metadata; do not invent a recovery observation or provider send.
The operation remains `awaiting_peer` until actual authenticated new-stream
admission confirms it. A CLI write or socket open cannot report completion.

Define every interruption: lost preparation output, source-only commit, hub-only
activation, lost first admission confirmation, both-side restart and old copied
worker reconnect. Reject unresolved or overlapping identity rotations before
reset; do not combine two uncertain changes to credential/certificate scope.
Old command bodies/results stay attributable to their original stream and can
never apply to the new epoch. Any local cancellation is explicitly unconfirmed.

Hub fences only control hub ingestion and authority. They cannot stop an offline
copied edge from checking or sending notifications. Running cloned identities is
unsupported; the operator must stop the old owner. Do not claim a database lease
can revoke effects on an unreachable host.

## Edge evidence and transaction

Require the existing exclusive data-directory lock with runtime stopped. Preserve
old local evidence before clearing the tables that assume one active epoch. The
selected direction is a bounded, durable SQLite archive before the reset transaction,
rather than retagging old telemetry into the new stream. Validate this direction
against real backup/restore behavior before implementation acceptance.

The archive must contain a consistent database including WAL-committed state,
original outbox bytes, gaps, command receipts, incidents/delivery history and
protected configuration/certificates. Keep it private, immutable after publication,
and bind its identity, size and digest to the reset journal. Bound archive count
and total bytes; refuse safely on capacity, storage, symlink or integrity failure.
Never auto-delete the sole preserved evidence to make reset succeed. Use bounded
copy/backup steps, verify driver completion, validate the resulting SQLite snapshot,
then sync the archive and parent directory before the current DB can change.

Archive publication and the DB commit are distinct boundaries. A crash between
them must leave the old DB authoritative and permit an exact retry to reuse a
verified archive. A failed final DB write must roll back epoch, counters, state and
certificate changes together. Preserve the original protection key/identity with
any exported archive; the archive must never become a new running clone implicitly.

Keep `identity.json` and its bootstrap stream immutable. Add a durable initial
stream anchor and authenticated reset journal that authorizes the current stream;
startup may accept a differing current epoch only through that verified record.
CLI inspection and runtime must use the verified current identity. Trace
`Store.initialize`, `ReadIdentity`, `EdgeTLSManager.reload` and actual admission
callers; the TLS manager currently compares the rotated certificate against the
bootstrap stream and must be adapted deliberately.

Authenticate the active rotated PEM with its old AAD, reseal the same material
under the new stream, and commit that change with reset/identity/counter effects.
Do not regenerate certificates, reset certificate/credential high-water or forget
retired identities. Configuration AAD currently excludes stream; confirm that
contract before deciding how its retained application evidence is reused. Clear
new-epoch live state without fabricating resolution of historical incidents.

The reset receipt records actual source/hub bounds and known gaps. After a backup
restore, unknown work beyond available evidence remains an explicit unknown
coverage discontinuity. Never fabricate a contiguous sequence-loss range from
unobserved counters. Archived old bytes retain their original stream attribution.

## Effect tests and gates

- Real SQLite archive with WAL-only committed content; ordinary cold restart keeps
  identity; reset survives reopening files/store and preserves readable old evidence.
- Wrong identity/key/digest, symlink, quota and backup/write/fsync failure leave
  current state unchanged. Crash after publication and after DB commit recover
  through exact idempotent requests. Late writes roll back resealing and selection.
- Active rotated certificate remains the same pin and key after reset; its new
  AAD opens and its old AAD does not. Expired/corrupt material never gets fallback.
- Both hub engines preserve old cursors, observations, gaps and commands; stale
  leases, concurrent admins, reused epochs and changed retries fail. Live state
  becomes UNKNOWN pending fresh source evidence without a false recovery event.
- Real TLS and compiled CLIs complete both orders of recoverable interruption,
  report `awaiting_peer` honestly and admit only the authorized new epoch. New
  sequence one cannot collide with or resurrect old sequence one.
- Paired migrations preserve populated state and reject destructive downgrade.
  Run full build/race/lint and actual engine cases, then update status/acceptance.

After reset, complete pressure diagnostics, bounded shutdown flushing and the real
fifteen-minute partition under [the M3 completion contract](M3_COMPLETION_WORK_CONTRACT.md).

## Advisory review disposition

Antigravity conversation `5b9f4830-2e3f-46bf-ac40-2708c26b0933` successfully reviewed
supplied design/source read-only and acknowledged follow-up corrections. Accept
archive-before-reset as the direction to prove. Correct its table-name typo,
lease-renewal assumptions, premature completion language and omission of the TLS
manager's bootstrap/current-stream comparison. Neither its prose nor this contract
is evidence of working reset code. No project rules or settings were changed.
