# M7 immutable binding through live configuration sessions

Executed 2026-10-12 on `feat/m7-receipt-backed-config`, following foundation commit
`d86e6f8` (`feat(probe): bind retained delivery evidence to assignment revisions`).
This is a production-lifecycle verification slice, not a new delivery capability
or runtime activation. See [binding contract](M7_ASSIGNMENT_REVISION_BINDING.md),
[first Colima acceptance](M7_COLIMA_ACCEPTANCE.md), and
[cutover authority limits](M7_CUTOVER_EVIDENCE.md). The subsequent
[authenticated regional reporting slice](M7_REGIONAL_APPLICATION_REPORTING.md)
extends the same real-session leaves with prepared hub connection/stream authority,
separate persisted regional reports, refusal-before-receipt, cold/reconnect/ABA
and exact duplicate checks. Its later evidence does not replace the historical
scope/counts recorded below; regional-only reports supply no aggregate support.

## What is exercised

`internal/adapters/repository/probe_assignment_revision_session_test.go` adds
`TestAssignmentRevisionSessionContract` for SQLite and MariaDB, each with desired
`regional`, `aggregate`, and `both`. Each leaf composes:

- A real monitor/remote assignment source and production `RefreshRemote`, protected
  retained hub snapshots, and a set revision of **9007199254740993** independent
  of document revision **1**.
- Real edge enrollment, pinned TLS 1.3 and WebSocket transports (`HubTransport.Run`
  and `EdgeRuntime`), `EdgeConfigService`, and protected edge SQLite persistence.
  Credentials are generated within the disposable fixture, never logged.
- A controlled refusal at the hub receipt callback after the edge has persisted
  its configuration. The hub's active document remains absent; merely receiving
  `config.applied` cannot create a durable acknowledgement.
- Edge runtime/store shutdown and reopen from the same private directory, followed
  by decoder revalidation of the exact original mode and assignment binding.
- Two production assignment CAS writes changing policy and restoring it without
  changing desired mode or source generation. Build-time set binding advances by
  two; document revision **2** has a different hash. Preparation alone does not
  acknowledge that new document.
- A real reconnect transferring the original document after that ABA/newer
  preparation. Production fenced `RecordRemoteApplied` and acknowledged-document
  reads retain its **original** binding, not the current desired revision.
- Subsequent transfer/receipt of the new bound document, then a repeated ACK.
  The repeated session succeeds while the edge's encrypted payload and original
  activation time remain unchanged.

For consolidated desired modes, the pure evaluator receives the actual document
ACK facts and hypothetical stronger application support, but **no separate
mode-application attestation**. It stays pending with the exact expected reason:
`assignment_revision_mismatch` for the old document after ABA, then
`mode_application_unreported` for the current bound document. This prevents a
pending result caused by an unrelated broken fixture from masquerading as proof.
Informational `paging.aggregate.v1` in the session hello is not application support.

## Executed verification

The strict engine gate now requires all three exact session leaves:

```text
TestAssignmentRevisionSessionContract/mariadb/regional
TestAssignmentRevisionSessionContract/mariadb/aggregate
TestAssignmentRevisionSessionContract/mariadb/both
```

These supplement the previously required 28 M7 leaves/API markers. Missing,
skipped, or failed session coverage cannot be replaced by parent or SQLite passes.

```bash
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-session-binding-verification/dual-engine.jsonl
# Internally: go test -race -count=1 -json -timeout 2400s -p 4 ./...
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
gofmt -l internal/
git diff --check
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py -v
```

Used cached Go 1.26.6 with `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`, and
`GOSUMDB=off`; no inherited provider/database credentials or downloads of Go
modules/toolchains. The existing Colima daemon was explicitly selected; the same
official MariaDB image digest as the first acceptance run was downloaded again.

| Evidence | Result |
|---|---:|
| Full dual-engine race gate | passed, exit 0 |
| Passed packages | 22 |
| Named test passes (parents and children, not unique functions) | 5,188 |
| Named MariaDB passes | 404 |
| Named failures / MariaDB skips / missing required markers | 0 / 0 / 0 |
| Required M7 real-engine markers | all 31 passed |
| Recognized optional/helper skips | 5 |
| Offline strict-parser regressions | 14 passed |
| CGO-free build / vet / lint / formatting / diff checks | passed; lint 0 issues |

Recognized skips are the same as the first acceptance record: MongoDB real-server,
Telegram optional send, edge disk-full platform fixture, and two helper-process
entry points. A bounded independent read-only review found no blocker in the
new session test and traced the actual transfer/receipt/activation/read paths.

An earlier focused attempt failed to compile a test call against the narrower
assignment port. It executed no tests; its failure log is retained. The corrected
focused SQLite run passed with MariaDB explicitly skipped. The later strict gate
above executed all three real MariaDB leaves and the final exact-reason assertions.
Artifacts: `tmp/m7-session-binding-verification/`.

## Cleanup and remaining limits

The gate removed its disposable container and anonymous MariaDB volume. The exact
owned image was removed without force after checking its ID and absence of users.
No fixture containers or volumes remained, and the owned gate shell exited.
Pre-existing containers and Colima configuration were untouched; Colima remains
running. No global prune, AWS resources, push, deployment, or paging activation.

No production behavior, schema, dependencies, HTTP Views, mode-application
producer, cutover caller, or send/suppression authority changed. This test does
not simulate arbitrary network ACK loss, assert equality of hub receipt time
with edge activation time, or add a crash/in-flight provider-I/O matrix. A retried
wire ACK may carry a later timestamp than the edge's retained activation.
Existing regional-delivery regressions remain part of the full backend gate.

`make gate-full` was not rerun. The previously reproduced baseline toolchain
advisories and remaining release/operational acceptance stay open. The stronger
source negotiation/application lifecycle and committed transactional cutover
barrier must precede any aggregate send or regional suppression. M7 activation
checkboxes therefore remain open.
