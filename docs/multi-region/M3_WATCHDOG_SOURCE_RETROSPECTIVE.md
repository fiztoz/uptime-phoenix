# Edge watchdog source lifecycle retrospective

## Recovery could invent acknowledgement

The initial source validator checked that ACKed-to-resolved transitions preserved
ACK metadata, but did not apply the complementary rule to firing-to-resolved.
A caller could supply valid-looking command/actor/time fields on a recovery and
persist an acknowledgement that had never had its own fenced transition.

`TestProbeWatchdogRecoveryCannotInventAcknowledgement` failed against the draft.
The validator now rejects ACK metadata on recovery from firing; an ACKed incident
still resolves with its exact committed command, actor, note and time. Tests also
reject alterations to each field and reopening of ACKed identity.

## A healthy checkpoint could repeat a recovery delivery

The draft accepted intents whenever the current incident was resolved and the
watchdog status was healthy. It did not require a new resolution transition.
A subsequent checkpoint with a fresh delivery UUID could therefore enqueue the
same recovery again even though the source lifecycle had not changed.

`TestProbeWatchdogRecoveryCheckpointCannotPageAgain` failed against the draft.
Resolved delivery creation now requires the incident transition in the same
commit. Firing-loss resends remain valid and keep their source version/sequence.
A repeated healthy checkpoint remains useful but has no send effect.

## Why the first tests missed these cases

The initial integration test covered the intended firing -> ACKed -> resolved
sequence, restarts and transaction faults. It did not try to skip ACK or attach
work to a later healthy checkpoint. Atomicity alone cannot prove the proposed
business effects are valid. For lifecycle code, test forbidden transitions and
unchanged-state side effects as well as the happy path.

The first new ACK-mutation test also had an aliasing error: it pointed the stored
ACK timestamp at a record field later changed to simulate clock rollback. That
changed the fixture's prior evidence too. The fixture now owns an independent
ACK timestamp; each adversarial case mutates only the proposed transition.

## Antigravity review and coaching

The read-only audit suggested retaining the last resolved identity while unarmed
was a defect. It is intentional: status determines whether an incident is open,
and the retained UUID prevents reopening the old outage. A real-store test now
proves disable -> unarmed -> starting -> healthy -> a new outage, rejecting old
UUID reuse and showing no extra send on settings changes.

The audit also suggested an explicit downgrade delivery guard was missing. Under
the provided foreign-key invariant a watchdog delivery references an incident
that the existing guard already rejects. Even its hypothetical corrupted-table
case would fail a constraint inside the transactional migration and roll back.
It did not demonstrate data loss or a supported failing input. No migration
change was made for that claim.

Useful suggestions became independent tests: competing writers, duplicate resend
ID rollback and rearm behavior. Codex sent the verified findings and rejected
claims back to the same Antigravity conversation, with instructions to distinguish
supported defects from defensive suggestions and never claim tests it did not run.
Antigravity owns no source files. Audit reports are evidence to investigate, not
acceptance by themselves.

## Follow-through

Keep enabled watchdogs guarded until source storage, replay authorization, config,
health callbacks and provider reconciliation are integrated. Do not count helper
or storage tests as end-to-end watchdog acceptance. Preserve generation/version
fences when wiring health; do not bypass them by relabeling stale health as a
local timer tick. Use measured monotonic loss across runtime ownership changes,
not subtraction of saved wall timestamps.
