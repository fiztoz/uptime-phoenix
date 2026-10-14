# M7 repository CI sharding

## Failure and cause

PR #84 at `a8ceb4e` exhausted Go's **per-package test-binary** `-timeout 40m`
in both the backend and MariaDB jobs. This was not a demonstrated deadlock or
an individual test running for 40 minutes. At termination, the current parent
was 31/54 seconds old and the active leaf was 3/2 seconds old respectively.
No assertion failure or race report preceded those timeouts in the captured logs.

The last accepted local dual-engine run had 178 top-level repository tests,
about 1,887 seconds of summed parent elapsed time, and a largest parent of about
138 seconds. Each contract creates/migrates real fixtures; the hosted runner's
aggregate package duration exceeded its budget. Splitting source files would
not change that binary's timeout. Raising the timeout alone would defer the
same scaling problem and weaken the feedback loop.

## Fix

M7 incorporates the already merged dependency/security fixes from main, including
Go 1.26.9. No monitoring, cutover, provider authority or opt-in behavior changes.

`repository_test_shards.py` discovers the compiled parent repository inventory,
sorts it and assigns whole parents round-robin across four isolated runners.
There is no hand-maintained prefix allowlist: future tests, examples and fuzz
seed targets are discovered automatically. Each parent runs all subtests, still
with `-race`, `-count=1` and the original 40-minute timeout. The current plan is
45/45/44/44 parents; historical local dual-engine durations predict approximately
428/390/529/539 seconds of parent work, not measured hosted-runner timings.

Every matrix job owns its disposable MariaDB service. Destructive fixtures must
never run concurrently against one shared database. Shard 0 additionally runs
all repository subpackages with real-engine configuration, retaining the original
contract surface and including future subpackages. Backend continues to run
all other packages, build/vet/format/lint checks and vulnerability scanning.

The stable `mariadb-contract` required check executes even after matrix failure
or cancellation, checks the matrix result and verifies all four artifacts against
a freshly compiled inventory and the exact workflow commit. It refuses missing,
extra, duplicate, stale or partial reports, zero-selection/partial parent work,
malformed events, unexpected packages/skips, any MariaDB skip and missing exact
required real-engine leaves. Parent/package passes or aggregate engine counts
cannot replace required leaves. Artifacts contain only names, timings and
provenance, not raw output, settings or credentials.

The exact-SHA release gate requires the four expanded `repository-race-N` job
names as well as all existing required checks. A green aggregate cannot hide a
missing, failed, cancelled, skipped or pending shard. No branch protection policy
is changed and no administrative bypass is introduced.

## Verification

**Validated at implementation commit `a5e1736`, 2026-10-14.** Fresh CI passed
all required checks, including every matrix member and the aggregate verifier.
The compiled plan executed all 178 parents exactly once: 1,376 repository named
passes (parents and children), 569 named MariaDB passes, all 197 required leaves
applicable to this package, zero missing leaves, zero failures/skips/malformed
events. These package-scoped counts are not the historical full-suite counts.

Observed hosted-runner times (not estimates):

| Shard | Complete parent tests | Entire job, including setup/subpackages |
|---|---:|---:|
| 0 | 15.1 minutes | 23.4 minutes |
| 1 | 10.8 minutes | 11.3 minutes |
| 2 | 21.6 minutes | 22.3 minutes |
| 3 | 27.9 minutes | 28.9 minutes |

Every shard stayed below the unchanged 40-minute test timeout. Four shards are
not unlimited capacity: use the observed parent timings to increase the matrix
or rebalance work as the suite grows, before consuming this headroom.

The local `make gate-full` also passed on unchanged code/tooling inputs using
Go 1.26.9: 22 backend package rows, 324 frontend tests, 27 browser tests, build,
vet, golangci-lint (zero issues), frontend checks/lint/build, Helm and tooling
regressions. Vulnerability scanning reported zero reachable or imported-package
vulnerabilities; one advisory remains only in an unused required module.
Offline regressions passed: 16 strict-gate/pin tests, 17 shard tests and 48 release
tooling PASS cases. Three assertion mutants (dropped parent, omitted required
leaf, removed race detection) were caught with the unmodified control passing.

Private evidence is retained under ignored `tmp/m7-ci-sharding-verification/`;
no private logs, fixture credentials or environment-specific runners are published.
The final documentation commit must independently obtain fresh green CI before
merge; the implementation checkpoint is not permission to bypass that check.

Commands and maintenance guidance are in [docs/TESTING.md](../TESTING.md#repository-ci-shards-and-future-test-growth).
The unsharded local full/strict dual-engine gates remain available. Future growth
should be handled by changing the matrix/count/release requirements together;
split an individual parent only when that parent itself approaches the budget.

This CI fix does not authorize deployment, live aggregate paging, provider
ambiguity clearance or the outstanding operational acceptance campaigns.
Historical runtime acceptance remains recorded in
[M7_OPERATIVE_CUTOVER.md](M7_OPERATIVE_CUTOVER.md).
