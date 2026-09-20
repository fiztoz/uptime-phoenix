# M3 certificate storage foundation acceptance

Baseline: `a525ca0`. This increment supplies local certificate storage and
cryptography. It does not advertise certificate execution, change the live TLS
certificate, or enable operator certificate issuance. Those integrations follow
the [certificate contract](M3_CERTIFICATE_ROTATION_WORK_CONTRACT.md).

## Implemented effects

Migration 008 stores bounded protected PEM and immutable scope, an active-version
pointer, persistent high-water and successful prepare result metadata. The source
generates a P-256 key/certificate locally, with protocol-bounded validity. AES-GCM
uses the existing local key with a separate purpose and complete metadata binding.
Opening material authenticates that scope, exact fingerprint, key pair, self-signature,
server usage and certificate validity. No new dependency is added.

Preparation and its original receipt commit together. Activation checks the exact
prepared version/pin, verifies protected material and commits the active pointer,
activation timestamp and receipt together. All effects and duplicate recovery
require current source session authority. Retries preserve original ciphertext
and result metadata; old successful commands cannot revert a newer active version.
Candidate-only preparation never changes active selection.

The overlap expires ten minutes after preparation command creation. Observed
expiry cannot revive on a backward clock step. Credential and certificate overlaps
exclude one another. Cleanup preserves active material, and pruning an expired
inactive candidate cannot lower the durable version high-water. The journal is
bounded to 1,024 entries and shares the existing 16,384 command-receipt limit.
Each PEM is at most 16 KiB before the 29-byte encryption envelope. Downgrade
refuses retained certificate state or receipts.

## Verification

Commands executed: focused race tests for `TestEdgeCertificate*`,
`TestEdgeCredentialRotation*` and `TestRuntimeIdentity*`; a direct Go 1.26.6
compatibility-setting regression; final `CGO_ENABLED=0 go build ./...`,
`go test -race -count=1 -timeout=20m -json ./...` with `TEST_MARIADB_DSN` set,
and `golangci-lint run`. All final commands exited zero. The
[evidence file](M3_CERTIFICATE_STORAGE_EVIDENCE.json) records exact source/log hashes.

Engines and named tests exercised: private SQLite persistence uses actual generated
certificates and encryption. `TestEdgeCertificateRotationRestartAndOriginalReceipts`
reopens storage after lost results, checks ciphertext/receipt equality, validates
the active TLS material and inspects DB/WAL bytes for private-key plaintext.
`TestEdgeCertificateRotationAtomicFailures` injects six late-write failures.
Further named cases cover stale authority, conflicting immutable input, concurrent
retry, expired overlap/clock rollback, mismatched/tampered material, capacity,
active-material retention, irreversible high-water and missing active rows.
`TestEdgeCertificateMigrationPreservesAppliedAcknowledgement` exercises a populated
up/down round trip and its real ACK/telemetry effects; populated certificate
downgrades are rejected. Existing hub receipt tests reject certificate-only fields
on credential commands on both SQLite and MariaDB.

Passed / failed / skipped: the final full gate passed in 22 packages with 3,527
named passes, zero failures and two existing optional skips
(`TestDatabaseChecker_Check_MongoDB_RealServer` and
`TestTelegramSender_Send_DownSeverity`). There are 283 MariaDB-named pass events;
271 explicitly select the live MariaDB engine (the broader name count includes
SQLite regression labels and unit validation). Neither count has a failure or
skip. Parent subtest passes are included. Lint reported zero issues; all 17
source/test/migration hashes remained unchanged during the final gate.

The earlier focused run had 99 passes and no failures/skips. The compatibility
reproduction subsequently exposed a real nil-leaf panic; the fixed direct-compiler
run passed. The first added identity regression failed on its fixture directory's
permissions, which were corrected. The initial full run is superseded because
source changed for the compatibility fix. See
[the retrospective](M3_CERTIFICATE_STORAGE_RETROSPECTIVE.md) for that failure ledger.

Acceptance criteria still unverified: live TLS switching and stale-handshake
admission, bootstrap-expiry recovery, hub certificate issuance/promotion/recovery,
certificate process acceptance, explicit stream reset, remaining pressure/bounded
flush and the full real fifteen-minute partition scenario. M3 remains incomplete.

## Review and continuation notes

Antigravity's design review, storage review and corrective teaching response all
succeeded. Its obsolete default-Go claim was narrowed to the reproduced
compatibility case; the immediate-overlap-closure suggestion was rejected.
It owns no files, and no review response substitutes for test execution. Do not
infer end-to-end certificate rotation from the new persistence methods.

Next: validate active protected material before startup; derive a separate active
TLS identity without changing the immutable bootstrap pairing check; fence cached
TLS publication and per-handshake session admission; wire the existing closed
wire DTOs and receipt-before-reconnect behavior. Then implement protected hub
operation state and SQLite/MariaDB effect tests, followed by process acceptance.
