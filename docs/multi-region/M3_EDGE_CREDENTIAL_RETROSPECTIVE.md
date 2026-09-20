# Source credential rotation retrospective

## Summary

During development after `2a4e198`, Codex reproduced a credential overlap that
could revive after a clock rollback. An activation rejected at the deadline did
not retire its pending rotation unless authentication had also read the expired
window. A new activation command ID at an earlier clock time then succeeded.
This defect was found and fixed before committing the source rotation increment.

## Root cause and symptom

`runtimeCredentials` persisted `overlap_closed` when authentication observed
expiry. `activateCredential` checked the deadline but originally returned a
rejected receipt without changing that flag. The shared `applyCommand` helper
persisted the rejection, which protected only that command UUID. A new command ID
reached the still-open preparation after the clock moved backward.

`TestEdgeCredentialRotationExpiredCommandCannotReviveOverlap` avoids any
intervening authentication read: prepare, set the store clock to the exact
deadline, reject activation, move the clock backward, retry with a new command ID.
The initial run failed because the final outcome was `applied`. This distinguished
the defect from duplicate-receipt behavior and from the already-passing
authentication expiry tests.

## Fix

`applyCommand` now calls `expireCredentialOverlaps` inside its source writer
transaction, after current session fencing and before duplicate/expiry lookup.
Successful command processing, including a rejected or recovered receipt,
commits observed retirement. Authentication uses the same helper. A rejected
credential admission commits retirement without claiming a new session generation.
The source retains the current credential when an unactivated candidate expires.

This does not turn wall-clock checks into a trusted global clock: retirement is
irreversible once storage observes expiry. A source restored from an earlier
database or a clock that was never observed past the deadline needs the separate
restore/clock contracts; this increment does not claim to solve those scenarios.

## Why the initial tests missed it

The first expiry tests called authentication before retrying. That call performed
the missing persistent retirement and hid the command-only path. The added test
removes the helpful intervening operation and asserts the actual credential effect.
No production incident or released regression is claimed.

## Review and coding feedback

Antigravity's first file-read attempt was denied and produced no review. Codex
supplied bounded source excerpts directly, without changing permissions or file
ownership. Conversation `e8375814-8a8f-4fb2-b0f7-91948e987240` then reviewed the
source admission, command transaction and session lifetime paths.

Its suggested deadline on the newly promoted active credential followed ambiguous
contract prose. The operational requirement is that the new active identity
survives the overlap; only the previous identity expires after activation. Codex
clarified the prose and kept the intended source behavior. Antigravity acknowledged
the correction, reviewed the reproduced rollback fix and returned no further
concrete source findings. It edited no files and did not execute acceptance tests.

The reusable lessons are to distinguish pending, active and previous identities;
trace both header authentication and durable session admission; and test an expiry
path without another operation silently repairing state. A review assertion needs
a concrete effect or violated requirement before it justifies a source change.

## Validation and continuation

The original regression now passes. Focused race checks also passed real SQLite
rollback, restart, duplicate, concurrent admission and bounded-storage cases, plus
real pinned-TLS preparation/activation, lost activation reply recovery after
restart, expired prepared sessions and expired previous sessions. The acceptance
record contains final full-gate evidence and limitations.

Codex owns the remaining protected hub rotation issuer, candidate selection and
automatic recovery work under [the rotation contract](M3_CREDENTIAL_ROTATION_WORK_CONTRACT.md).
Certificate rotation, explicit stream reset, remaining pressure/shutdown behavior
and the complete fifteen-minute partition acceptance still belong to M3.
