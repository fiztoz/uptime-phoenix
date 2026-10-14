# M7 retained-document and assignment-binding acceptance

Executed 2026-10-12 on `feat/m7-receipt-backed-config`, against the working tree
based on `284bc35`. This record covers the local acknowledged-document diagnostic,
pure cutover contract and immutable assignment-set revision binding, including the
previously committed remote retained-document reader/diagnostic regressions.
It supersedes the **unexecuted MariaDB** qualification in their earlier handoffs
for the named cases below. It does not complete M7 or authorize rollout.

## Intent and authority boundary

- Exact protected configuration documents acknowledge desired content, not an
  applied runtime delivery mode. Diagnostic read checks are not write/send fences.
- Immutable assignment-set revision is captured in the source transaction at
  build time, not looked up when a receipt arrives. Document and assignment
  counters remain separate. Legacy absence remains zero/unbound.
- Pure cutover evidence still needs separate exact runtime application evidence;
  no producer, production evaluator caller, committed barrier, aggregate intent
  sender or regional suppression exists. Regional paging remains active.
- Existing GET/false/PUT behavior, confidentiality, scoped visibility and local
  execution freshness remain unchanged. No schema migration/dependency/UI change
  was added by these slices.

## Disposable infrastructure and cleanup

The existing Colima default VM was already running (Docker, Linux arm64, two
CPUs, approximately 2 GiB RAM). No VM configuration, Docker context, existing
container or existing volume was modified. The endpoint was explicitly selected
with `DOCKER_HOST`; an empty task-owned Docker config avoided inherited registry
credentials. No AWS resource was created or accessed.

`mariadb:11` was absent from this daemon and was pulled for the test. Image digest:

```text
mariadb@sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15
```

The committed gate created one uniquely named container, a fresh database and a
random localhost-only port. Repository tests are destructive only to that owned
fixture; no existing database or caller-provided DSN was used. Root/test database
credentials were generated/fixture-scoped and were not printed or inherited by
unrelated commands. The raw Go event log was exclusively created with mode 0600.

The gate's finally cleanup now uses `docker rm -f -v` so the official image's
anonymous `/var/lib/mysql` volume is removed as well as the container. Offline
regressions verify this on normal completion and refused observer grants. The Go
child now explicitly has `GOENV=off`, alongside disabled module/toolchain downloads.

After completion, inspection found no task fixture container or Docker volume.
The downloaded image was removed without force after verifying its exact owned
ID and absence of users. Both pre-existing stopped containers were unchanged;
Colima remained running. The owned gate shell exited. No global prune was used.

## Commands executed

Cached Go 1.26.6 was selected directly; no toolchain/dependency upgrade occurred.
The gate sanitizes the environment and supplies only its fresh disposable DSN.

```sh
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py -v
python3 -B scripts/m6_dual_engine_gate.py \
  --mariadb-image mariadb:11 \
  --go-json tmp/m7-colima-verification/dual-engine.jsonl
```

The real gate executes:

```sh
go test -race -count=1 -json -timeout 2400s -p 4 ./...
```

Result (`tmp/m7-colima-verification/summary.json`):

| Evidence | Executed result |
|---|---|
| Gate / Go exit | Passed / 0 |
| Duration | 1,220.8 seconds |
| Passing packages | 22 |
| Named race passes | 5,179 (parent/subtest events, not unique functions) |
| Named failures | 0 |
| Named `/mariadb` passes / skips | 400 / 0 |
| Missing required markers | None |
| Unrecognized skips / malformed JSON | None / 0 |
| Offline strict-parser regressions | 14 passed |

All 28 M7 real-engine markers passed: the original 12 exact remote-reader leaves,
nine local-reader leaves, both scoped HTTP API markers and all five immutable
assignment-binding leaves. Thus the actual InnoDB gate exercised coherent
concurrent retained tuple reads through production `RecordRemoteApplied` and
`ActivateLocal`, authentication/semantic refusal, cold retained reads, scoped
JWT/router evidence, grant/CAS refusal, legacy execution/refresh upgrade, stale
prepared activation refusal and same-mode ABA binding/publication.

The five recognized skips were optional MongoDB/provider integration, two
helper-process entry points invoked by their parent tests, and the dedicated
Linux tmpfs disk-full test. This gate does not prove those skipped external or
platform-specific acceptances. Both SQLite and MariaDB fixtures executed.

The existing CGO-free build, vet, zero-issue lint, formatting, baseline backend
race, entire-probe rerun and four private negative controls are recorded in
[M7 assignment revision binding](M7_ASSIGNMENT_REVISION_BINDING.md). Subsequent
commit-readiness checks are recorded with this run's artifacts.

## Read-only review and remaining limits

A read-only reviewer traced the exact tuple/source/codec/projection/API/cutover/
legacy-execution/refresh paths and reported no blocker or major defect. Inspection
is not execution; the gate above is the engine evidence. The returned-projection
revision check is retained as defense at the injected decoder port boundary,
although the production decoder already validates exact document metadata.

This is a backend regression/required-marker gate, **not** the full section-13
failure matrix, populated migration rehearsal, load/soak campaign or canary.
Actual stronger mode-application negotiation, durable applied-mode producers,
committed barriers, aggregate incidents/intents and final send/suppression fencing
remain unimplemented. Their M7 activation checkboxes remain open.

`make gate-full` was not rerun. Its previously reproduced baseline Go standard-
library advisory failure remains a separate release blocker. This bounded
foundation is ready for source commit, not deployment. Nothing was pushed,
deployed or activated by this acceptance run.
