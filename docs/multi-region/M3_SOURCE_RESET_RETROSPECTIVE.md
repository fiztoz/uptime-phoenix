# Source reset: durable archive retry retrospective

Date: 2026-09-21. Owner: Codex. The defect was found in uncommitted M3 code;
there is no evidence it reached a deployment.

## Mechanism and correction

The first implementation correctly wrote and synced the archive database and
manifest, renamed their directory into place, and synced the publication parent
before clearing the live epoch in one SQLite transaction. Its exact-retry path
verified existing archive bytes but omitted the final filesystem durability step.
If rename succeeded and parent fsync failed, the first attempt safely stopped.
The next attempt could nevertheless treat that visible directory as durable and
delete active evidence before its archive name was guaranteed to survive power
loss. Transaction rollback alone cannot protect this separate filesystem boundary.

`TestEdgeStreamResetRetrySyncsPreviouslyPublishedArchiveBeforeCommit` reproduced
the problem: an injected parent sync failure was bypassed on retry, and reset
committed. `verifyResetArchive` now syncs the database, manifest, archive directory
and publication parent after validation on every reuse. If any sync fails, reset
leaves the original stream and live evidence intact. The failing test and complete
SQLite reset race subset passed after the correction.

The earlier tests proved first-attempt sync failures preserved data and that a
healthy retry succeeded. They did not require the retry itself to reestablish
durability. Future tests at publication boundaries must include a visible artifact
whose durability remains unresolved, then keep the same failure active on retry.

## Antigravity review and teaching

Antigravity conversation `3a36df2d-3f53-45aa-a1b6-ade3838c23d1` reviewed supplied
source in read-only mode. It owned no files and ran no tests. Its service result
was `SUCCESS`, which says nothing about correctness of the review.

Codex rejected three unsupported claims after tracing the source: staging cleanup
already preceded quota checks; all live-row deletion and epoch selection were
inside one transaction after archive publication; and a zero-byte file cannot be
a valid PHXE database with identity/schema/journal. Antigravity explicitly
retracted all three. Its corrective response included unverified line-number
references; those are not accepted evidence.

Codex then sent the actual reproduced fsync defect and fix as a concrete lesson:
follow statement order, identify each transaction/filesystem boundary, and test
effects under persistent failure. Do not label a hypothetical claim as proved or
claim execution from a code excerpt. No project rules, settings or file ownership
were changed through Antigravity. Completion still depends on Codex's executed
tests and the remaining hub/CLI integration.

The final teaching response returned `SUCCESS` and acknowledged that database
atomicity does not establish filesystem publication durability. It described
keeping the parent-sync fault active during retry and did not claim to have run
the test. The response log is retained in the checkpoint evidence ledger.
