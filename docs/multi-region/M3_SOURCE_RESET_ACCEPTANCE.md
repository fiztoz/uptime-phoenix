# M3 source stream-reset checkpoint

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Date: 2026-09-21. Baseline: `b112f07`. This checkpoint implements the stopped
source storage/runtime half of [subsequent acceptance](M3_HUB_RESET_ACCEPTANCE.md).
It does **not** complete reset or M3: hub preparation/activation/peer confirmation
and both operator CLI commands remain to be integrated. There is no deployed reset
endpoint and no new CLI command in this checkpoint. Codex owns all files.

## Implemented behavior

Edge migration 009 retains the immutable initial stream and authenticates a
bounded journal of explicit epoch changes. A prepared operation blocks normal
startup, and an exclusive SQLite recovery handle rejects runtime writes. The
composition root must retain the existing OS directory lock throughout recovery.
Ordinary restart never creates an epoch. Changed requests, retired epochs, wrong
binding, overlapping identity rotation and unauthenticated provenance fail closed.

Before changing epoch, reset preserves a consistent SQLite snapshot including
WAL-only committed content. Private archives contain both the old database and a
purpose-separated authenticated manifest. Backup copies 128 pages per step;
integrity, foreign keys, identity, journal, size and SHA-256 are verified. File and
directory syncs precede the transaction that clears old active queues/state and
selects the new stream. Exact retry repeats durability checks and returns the
original committed receipt. A late transaction failure rolls back all epoch,
counter, state and certificate effects together.

The archive lives at `stream-archives/<reset UUID>/edge.db` with `manifest.json`,
below the private data directory. Files are mode 0600; directories are mode 0700.
The application never rewrites published archives. Limits are 1 GiB per database,
4 GiB aggregate archive budget including manifest reservation, and 16 retained
reset operations. Capacity failure preserves the old epoch and its pending
reservation. There is no automatic evidence deletion. Keep the original identity
files and protection key with any exported evidence. An archive is historical
evidence, not an implicitly authorized running clone.

Config, enrollment, token digest, command receipts and credential/certificate
high-water remain intact. An active rotated certificate is authenticated under
the old stream and the same PEM is resealed under the new stream atomically.
`EdgeTLSManager` checks both the bootstrap anchor and verified current stream.
Inspection reports the current durable stream. Old incidents/deliveries remain in
the archive; reset invents neither a recovery nor a send confirmation. Recorded
source/hub bounds describe available evidence only; unobserved work after a
restore remains unknown, not a fabricated sequence-loss range.

## Verification

Targeted real SQLite race tests passed for WAL-only evidence, both publication
and commit interruption, exact and changed retries, proof/digest/symlink rejection,
storage faults, quota, migration downgrade guards, repeated epoch provenance,
retired epoch rejection and certificate resealing rollback. Real TLS/WebSocket
tests passed with bootstrap and rotated certificates after cold restart: the
same pin/token work, hello announces the new stream, old-stream welcome fails,
and immutable bootstrap files remain byte-identical.

An initial affected-package race run passed 1,619 named cases in five packages
with zero skips/failures. The subsequent durability regression reproduced a bug,
then passed after its fix along with all reset tests. The final CGO-free build,
full Go race suite and zero-issue lint passed: 3,655 named passes in 22 packages,
zero failures and two existing optional skips. All 305 MariaDB-named cases
passed, including 293 cases from the audited live-engine inventory; none skipped.
All 47 source-reset cases ran, and all 17 source hashes stayed unchanged.

The separately rebuilt process workflow passed 29 existing replay/certificate
stages against its own disposable MariaDB schema. That is a regression check,
not compiled reset CLI acceptance. Exact commands, source/binary/log hashes,
engine inventory and advisory statuses are in
[the evidence ledger](M3_SOURCE_RESET_EVIDENCE.json). No push or deployment occurred.

See [retrospective](../postmortems/2026-09-21-m3-integration.md#reset-and-archive-durability) for the reproduced durability
defect and the disposition of Antigravity's advisory review.
