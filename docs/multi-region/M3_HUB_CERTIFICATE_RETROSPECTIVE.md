# Hub certificate rotation retrospective

Date: 2026-09-21. Baseline: `34e3048`. Owner: Codex integration.
The implementation, regression and this record are committed together on
`codex/multi-region-probe-plan`. Antigravity supplied read-only review; no source
ownership was delegated. See [acceptance](M3_HUB_CERTIFICATE_ACCEPTANCE.md).

## Summary

During pre-commit validation, a certificate preparation result with a non-UTC
expiry was stored seven hours late on MariaDB. The repository normalized the
application time but omitted the newly added certificate expiry. Normalizing
both timestamps at the public repository boundary fixes the round trip and
immutable duplicate comparison. No deployment or customer incident is claimed.

## Symptom and root cause

`TestProbeCertificateRotation/mariadb/UTCReceiptBoundary` submitted the same
instants expressed in UTC+7. `CompleteCommand` normalized `AppliedAt`, but passed
`CertificateNotAfter` unchanged through `completeCertificateRotation` into Bun's
SQL formatting. MariaDB stored that local wall clock in `DATETIME(6)`. Reading it
back as UTC shifted expiry by seven hours. The command receipt and rotation
journal consequently disagreed with the original source expiry, so an identical
source retry could conflict as well as expose incorrect operator metadata.

The original reproduction is retained in
`/private/tmp/phoenix-m3-hub-certificate-utc-before.jsonl`. SQLite passed that same
case; it did not establish MariaDB correctness. The TLS verifier independently
checks the actual leaf expiry, so the reproduced storage defect does not prove
that an expired certificate was accepted on the network.

## Fix

`ProbeCommandStore.CompleteCommand` now copies `CertificateNotAfter.UTC()` before
entering the transaction, alongside existing application-time normalization.
No caller's pointed-to value is mutated. Validation still rejects fractional
certificate expiry and incorrect validity duration. The regression verifies both
stored timestamps, UTC locations, exact instants, and the identical retry.

## Why it slipped through

The first receipt fixtures used UTC, and the real wire decoder already normalizes
timestamps. Those paths passed while the repository port remained vulnerable to
a direct caller. This is the same engine boundary addressed by project rule 6:
an in-memory fake or SQLite-only pass cannot establish MariaDB wall-clock behavior.
Future receipt fields must receive the same boundary treatment as existing ones.

## Validation

The original UTC+7 regression now passes on SQLite and live MariaDB. The expanded
focused run passed all 80 named cases with no failures/skips, covering transaction
rollback, immutable retries, capability routing, exclusion, migration preservation,
retained commands, activation capacity and fallback fencing. The final full gate passed 3,608 named tests across 22 packages, with zero
failures and zero lint issues. The compiled process passed all 29 stages. Source,
binary and log hashes are recorded in the acceptance/evidence files.

Two additional fixture corrections kept production checks intact. Immediate
fallback assertions initially assumed the host and disposable database clocks
agreed to microseconds; the synthetic receipt now uses permitted source clock
skew. The strict-activation test supplies a time before preparation rather than
mistaking equality with preparation for an invalid ordering.

## Independent certificate capability dispatch

Final source tracing found a second integration defect: `HubTransport` populated
`CertificateRotation` from the peer hello but started its command sender only if
ACK or credential rotation was advertised. A compatible certificate-only peer
could connect indefinitely without receiving preparation. The full-featured
compiled edge advertises all three, so its successful process run hid the gap.

The real certificate fixture now advertises only snapshot and certificate command
support. Before the fix, the two-store TLS test timed out waiting for the command
receipt; `/private/tmp/phoenix-m3-certificate-only-before.jsonl` records the failure.
Including certificate support in the sender startup condition fixes the dispatch
path. The same regression continues through lost activation, both-store restart
and original-deadline retirement, rather than merely inspecting the capability
boolean. All ten selected certificate, credential and ACK runtime regressions then passed with the race detector.

The earlier full-gate source freeze is superseded because this production change
landed during validation. Only the later complete gate and compiled-process run
on the final unchanged source count toward acceptance. This was Codex's integration
defect, discovered independently of Antigravity's audit.

## Migration rehearsal column ordering

The first full suite failed `TestProbeRegistryContract/mariadb/LegacyBackfillAndDown`.
The preceding 062 rehearsal re-added its columns after the newer 063 columns;
rebuilding the complete migration suffix returned them to migration order. Every
column was preserved, but the older guard compared physical ordinal position.
The test now sorts both name inventories before comparison; missing/extra columns
still fail, and all existing populated-data preservation assertions remain.
The sequential 062 and registry rehearsals passed together on actual MariaDB.
This was a test assumption exposed by the additional migration, not data loss.

## Antigravity feedback and coding lessons

Conversation `ffae3bff-f1f2-4387-80d3-3c448efbb95d` produced useful provisional
review text, but both audit and follow-up returned service `ERROR` for output
limits. Process exit zero did not make either audit a successful gate.

Codex reproduced the narrower expiry normalization finding, fixed it and sent
the evidence back. Antigravity's follow-up acknowledged these corrections:

- Decode derives `PayloadHash` from exact wire bytes. Its absence in the
  pre-encoding struct is not a missing persisted digest. Trace the codec before
  declaring all activation impossible; both engines and real TLS exercised it.
- `AppliedAt` was already normalized by the public entry point. Only the new
  expiry field lacked normalization. Trace callers as well as helper bodies.
- Expiring the fallback context enforces the trust deadline. The owned reconnect
  loop continues afterward; an availability diagnostic does not prove a blocker.
- Source `NotAfter` is preparation time truncated to seconds plus validity days;
  backdating `NotBefore` does not change that contract.

The follow-up's unverified line numbers and broad acceptance statements are not
evidence. The retained regressions and independent full gate carry that burden.
A separate bounded teaching response succeeded in conversation
`ac639ecd-0944-42fe-9abb-799c259da79b` using Gemini 3.8 Flash Low. It acknowledged
UTC boundary coverage, independent capability subsets, tracing digest/reconnect
callees, and dual-engine effect tests. This is teaching acknowledgment, not a
replacement code audit. No `/learn` changes to project rules or agent settings
were authorized or made.

## Follow-up

Codex continues explicit stream-reset recovery, pressure/bounded shutdown checks
and the real fifteen-minute partition under
[the M3 contract](M3_COMPLETION_WORK_CONTRACT.md). This increment does not complete M3.
