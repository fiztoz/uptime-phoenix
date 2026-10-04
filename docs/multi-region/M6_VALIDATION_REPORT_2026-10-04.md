# Validation summary — October 4, 2026

Candidate: `0.5.0-rc.local.20261004.1`. Code validation passed as recorded below;
full operational acceptance remains incomplete. These are historical execution
results. Subsequent PR CI passed as recorded below; deployment remains separate. Reproduction
instructions and the external UAT/load test recipe are in the single
[testing guide](../TESTING.md#11-reproduce-validation-and-create-external-uatload-tests).

| Executed check | Result |
| --- | --- |
| `make gate-full` | Passed: Go build, vet, race tests and lint; frontend type/build/lint, 263 unit tests and 29 browser cases; Helm and release-tool checks. The campaign also ran local harness tests, now excluded from the repository gate. |
| Strict backend race suites | Passed on SQLite and real MariaDB **11.8.9 and 12.3.3**. Each MariaDB run recorded 22 packages and 4,352 named passes, including **343 MariaDB passes with zero MariaDB skips/failures**. |
| History queue regression | Passed on both engines. On MariaDB 12, the measured selector changed from 65,083 examined rows / 121.79 ms to one row / 0.032 ms. The old implementation failed the constructed regression. |
| Published 0.4.5 migration | Passed on MariaDB 11 and a logical-transfer path to MariaDB 12: 100 monitors, 100,000 heartbeats, three populated rollups, migrations 34→74, held-reader lock verification, API ordering and no-op restart. Synthetic fixtures do not establish production peak disk or lock duration. |
| Release artifacts | Built and checksum-verified 44 CGO-free binaries; chart packaging and 44 SPDX 2.3 binary SBOMs passed. Four standalone-SBOM regression tests passed after the repair. |
| Actual image builds | Local Alpine package fetches timed out. Subsequent PR CI passed Linux amd64 builds for all-in-one, API, worker, web and probe. The full multi-platform release matrix and publication remain separate. |
| Final PR CI | [Run 37195065940](https://github.com/fiztoz/uptime-phoenix/actions/runs/37195065940) passed all seven required jobs and CodeQL on `e389cf9`; merged in PR #49 as `1802219`. Backend race tests, lint (zero issues), vulnerability scan and real MariaDB contract tests passed. |

The strict suites had four classified non-MariaDB skips: optional MongoDB and
Telegram targets, a crash-child helper, and a Linux ENOSPC case covered separately.
Named counts include subtests. The full local gate preceded the final SBOM and
history-test-hook edits; focused release checks and both strict backend matrices
passed afterward. Earlier failed runs were not relabeled as passes.

Changes covered include indexed history queue selection, deterministic heartbeat
ordering, migration/startup and monitor-mutation fixes, fleet admission for
backup/config changes, split API probe-key wiring, release tooling, and regional
Recent Checks selecting actual source observations instead of overall intervals.
Core Go/TypeScript regression tests remain with the code.

The owner accepts delayed or incomplete historical uptime logs. That exception
does not waive current monitoring or alert correctness. Representative off-host
backup/restore, the full 24-hour fault/recovery test, matched-candidate operational
checks and the historical AWS DELETE overlap remain unverified. Canary samples
used mixed binary versions and do not establish a release load-test pass.

Per the owner's repository scope, UAT/load/cloud campaign tooling and raw logs
stay local, including the 12 previously tracked smoke/evidence/rehearsal scripts
and two legacy load-test files found in the follow-up audit. Their originals and
SHA-256 inventory were preserved before untracking. This summary remains the
current validation report. Portable release, Helm and strict database-test
helpers remain, along with application unit/integration and CI browser tests.
Older milestone evidence describes historical executions; its removed harness
paths are not current checkout prerequisites. Use the testing guide to reproduce
coverage with external campaigns.

Results and an inventory were saved before cleanup removed 270 generated targets
containing 20.9 GB of logical data. Detailed originals, hashes and cleanup receipts
remain in ignored local storage. No cloud resources or credentials were removed;
operational expiry and cleanup holds were unchanged by that cleanup.

After this scope cleanup, `make release-image-gate m6-backend-harness-gate`,
`make helm-validate`, and `git diff --check` passed. Relative links in
changed Markdown were checked against the publishable tree, with no references to the
excluded files. Application and core-test source hashes are unchanged; full
application suites were not rerun for this documentation/tooling cleanup.

The tracked-script follow-up also passed Go build and the retained Helm, release
and coverage-parser checks from a tracked-files-only export with all 14 retired
files absent. Local lint reported zero issues. The replacement guide recipe
executed `TestEdgeDiskFullCriticalCommit` and both crash-around-commit subtests
on real Linux tmpfs (modernc SQLite), with four named passes and zero skips;
its temporary container was removed. Application logic and core test assertions
were unchanged. Full follow-up CI is recorded on PR #50.
