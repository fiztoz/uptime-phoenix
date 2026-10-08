# M6 Conditional Lab Acceptance — 2026-10-07

**Verdict: PASS for the agreed bounded lab close-out, with the conditions below.**
The owner accepted this scope on 7 October and chose not to start another
24-hour campaign. This records conditional acceptance of the accumulated lab
evidence. Full M6 operational acceptance and production readiness remain open
under the [M6 plan](IMPLEMENTATION_PLAN.md#9-m6--failure-validation-and-v1-activation).

## Accepted candidate and scope

- Final main: [`fa6a76781936ddef392d41bc4e3e7dd22a3e56ea`](https://github.com/fiztoz/uptime-phoenix/commit/fa6a76781936ddef392d41bc4e3e7dd22a3e56ea).
  Accepted source tree: `63c74f0ba389f56dbbeec093cc3943bedcaad37a`.
- All eight remediation PRs are merged: [#72](https://github.com/fiztoz/uptime-phoenix/pull/72)
  maintenance observations, [#73](https://github.com/fiztoz/uptime-phoenix/pull/73)
  partitioned-history deletion, [#74](https://github.com/fiztoz/uptime-phoenix/pull/74)
  delayed outage summaries, [#75](https://github.com/fiztoz/uptime-phoenix/pull/75)
  durable-ingest health, [#76](https://github.com/fiztoz/uptime-phoenix/pull/76)
  WebSocket test race, [#77](https://github.com/fiztoz/uptime-phoenix/pull/77)
  observation lock ordering, [#78](https://github.com/fiztoz/uptime-phoenix/pull/78)
  history lock ordering, and [#79](https://github.com/fiztoz/uptime-phoenix/pull/79)
  status-page recovery reads. These fixes have regression and CI evidence;
  they do not retroactively change earlier campaign outcomes.
- The final short rehearsal rebuilt all three native binaries from this exact
  main commit. Embedded VCS revisions match, with `vcs.modified=false`.
  It used Go 1.26.6, `CGO_ENABLED=0`, and the `noweb` build tag.
  Browser/UI acceptance was outside this rehearsal.

## Passed checks

### Final-main partition and recovery

The native rehearsal ran from **15:08:14 to 15:25:04 UTC**
(**22:08:14 to 22:25:04 Asia/Bangkok**) on 7 October and exited 0.
**All 42 named assertions passed.** The topology was one real probe, two HTTP
monitors and one WebSocket monitor at one-second intervals, an API and two
competing workers, and disposable MariaDB 11.8.9. The telemetry cap was 512 MiB;
the executor had a 4 CPU/16 GiB quota and the database container a separate
2 CPU/2 GiB limit.

- **Real partition:** 900.000145 seconds; command-era recovery completed in
  **19.766806 seconds**, without restarting the follow-up harness.
- **Exact replay:** all **2,156 prefix records** and **2,151 observations with
  original microsecond timestamps** verified; no rejected receipts or hub
  provider sends for mirrored remote incidents.
- **Current state first:** a fresh persisted UP receipt applied **93.237 ms
  before** the first new replay receipt.
- **ACK isolation:** one source receipt and one ACK transition applied to the
  original incident; its recovery preserved the command. Repeating that command
  was idempotent, and a new ACK against the resolved old incident returned
  `already_resolved`. A later outage had its own incident, remained
  unacknowledged and resolved independently.
- **Durability and continuity:** offline DOWN/UP, provider retry and process
  restart retained evidence; reconnect preserved identity/configuration and
  fenced ownership. Final created and acknowledged sequence values both reached
  **2,334**. All three monitors recorded checks beyond the partition prefix.
- **Harness checks:** seven recovery-budget regressions passed, covering
  delayed ACK observation, one unchanged recovery deadline, strict control
  timeouts, deadline crossing and immutable first-state capture.

Fourteen partition checkpoints showed continuing checks and an unchanged hub
cursor during the link outage. Maximum sampled source age was 1.1724 seconds
and sampled directory size 5,223,118 bytes. These are samples, not a complete
latency distribution or true peak disk measurement. Candidate processes and
the database container were stopped; private retained evidence was not deleted.

### Final application CI

[CI run 37642285526](https://github.com/fiztoz/uptime-phoenix/actions/runs/37642285526)
completed successfully at **15:48:10 UTC** on the accepted main SHA. All seven
jobs passed: backend, frontend, MariaDB contract, Docker, Helm, actionlint and
E2E. This includes the configured backend race, lint and vulnerability checks.

[CodeQL run 37642284308](https://github.com/fiztoz/uptime-phoenix/actions/runs/37642284308)
failed in all four language jobs after SARIF generation during upload.
The attempted rerun returned HTTP 403, “This workflow run cannot be retried.”
Its root cause remains unresolved. The upload failure establishes neither a
vulnerability finding nor a clean security audit.

## Historical and supporting evidence

**Original 24-hour campaign, baseline `1107e6f` (v0.5.1):** the uninterrupted
partition lasted **86,400.000103 seconds**. The original harness then failed its
40-second ordered-ACK telemetry observation timeout after durable command
confirmation. That failure is preserved. A guarded recovery restart using the
same store and binaries verified **207,353 records / 207,348 original
timestamps** in **669.445896 seconds**, with created/ACK sequence
**209,043 / 209,043**, zero rejections and the original deadline unchanged.
This supports durable recovery after restart. It does not establish an
uninterrupted original-harness recovery pass or a 24-hour pass on final main.

**Local combined load after the #79 fix:** one successful 1,000-assignment,
10-probe, five-second-interval pair measured **2.032303× ACK/arrival**
against the unchanged 2× threshold, **49.7241 seconds** prefix drain and
**0.196874 seconds** API p95. The margin was only **1.615%**. The database ran
outside the executor's shared CPU quota. Repeatability and matched AWS capacity
remain unproven; earlier failures remain valid for their candidates/resources.

**Restore and upgrade:** bounded T35 synthetic full hub/edge restores on
SQLite/MariaDB and an old-version source-built upgrade with 100 monitors and
100,000 heartbeats passed. The existing [validation record](M6_VALIDATION_REPORT_2026-10-04.md),
[partitioned migration rehearsal](M6_PARTITIONED_MIGRATION_REHEARSAL.md) and
[application-runner rehearsal](M6_MIGRATION_RUNNER_REHEARSAL.md) retain their
own scope and limits. None establishes representative off-host recovery or
production-sized peak disk and lock bounds.

The original 40-row matrix remains **34 scoped passes, one partial and five
failures**. Candidate fixes and the 42-assertion follow-up are additional
evidence, not a rewritten all-pass matrix. Newly observed MariaDB 1020
write-batch retries recovered; those diagnostics do not explain the historical
AWS DELETE-overlap incident.

## Conditions and owner actions

The roles below identify who must close each gate; they do not authorize new
execution, spending or deployment.

| Owner | Evidence needed before unconditional or operational acceptance |
| --- | --- |
| Project owner and validation owner | Reconcile T01–T40 and remaining durable-boundary kill coverage against the exact candidate. Keep the full-candidate uninterrupted 24-hour gate unexecuted under today's no-new-24h decision; any later claim requires explicit scope approval and matching evidence. |
| Performance owner | Repeat the unchanged load cases and backlog-drain threshold on matched AWS resources, with database and application resource accounting, control/ingest latency and repeatability recorded. |
| Operations owner | Rehearse representative off-host full hub DB plus edge restore, including keys, stream epochs, fences and no duplicate identity. |
| Database owner | Rehearse the production-sized populated migration; capture true peak disk, lock/reader/writer impact, index validity and rollback refusal evidence. |
| Incident owner | Obtain the historical AWS DELETE-overlap logs and correlate the 1020 incident before claiming it resolved or explained. |
| Repository owner | Diagnose the CodeQL upload failure and obtain a completed result without weakening security settings. |
| Deployment owner | Separately approve a matched-candidate noncritical canary, deliberate partition/recovery and limited enrollment through the normal deployment process. |

The [operator requirements](M6_OPERATOR_REQUIREMENTS.md) and
[fleet activation gate](M6_FLEET_ACTIVATION_GATE.md) remain mandatory.
Current monitoring and alert correctness are not waived by accepting delayed
or incomplete historical logs. This report changes no acceptance threshold,
production setting, enrollment permission or milestone checkbox.

## Evidence record

The retained private `candidate900-evidence.tar.gz` contains the 42 assertions,
build provenance, checkpoints, terminal verification and seven harness-test
results. Its **32 manifest entries were hash-verified** for this report.
Archive SHA-256:
`86da6701067656c97673ca7b42250d789413bbf09cac3aafbb81c7030413e6a2`.

This documentation-only close-out adds a dated summary. Historical reports,
raw evidence and failed outcomes are preserved. No application, harness or
workflow changes accompany it.
