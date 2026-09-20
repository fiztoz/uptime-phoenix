# M3 hub certificate rotation acceptance

Date: 2026-09-21. Baseline: `34e3048`. Codex owns source and verification;
Antigravity review is advisory. This increment does not complete M3.

## Implemented behavior

Hub migration 063 persists bounded immutable certificate operations and
active/high-water/expiry metadata. Issuance reserves both command IDs, pending
slots and activation bytes in one transaction. Activation stays blocked with no
payload until the durable prepare result supplies the new fingerprint. It is
never reported as remotely confirmed merely because it was reserved or canceled.

The hub issues and dispatches certificate commands only through the existing
protected command/fenced connector path and peer capability. A successful
candidate TLS connection does not promote the pin. Only the immutable source
activation receipt atomically updates the operation/command, current pin and
same-token ciphertext resealed under the new fingerprint.

Candidate trust is separate from credential encryption metadata. Old-pin fallback
requires a typed TLS mismatch before HTTP/source admission, a fresh fenced
selection and a context deadline at the original ten-minute overlap. Observed
retirement is irreversible. After expiry the hub can recover a lost activation
receipt using the candidate only. If activation never reached the source before
expiry, explicit verified operator recovery can be necessary.

Credential/certificate operations exclude one another. Retained operation
commands cannot be pruned prematurely; each activation reserves shared bounded
storage before its payload exists. Both engines refuse downgrade while any
certificate state or high-water would be lost. Safe 063 down/up preserves existing
credential operations, ACKs and connection ciphertext.

The CLI exposes `rotate-certificate` and `certificate-rotation-status` with
explicit stable UUID/version/validity and metadata-only output. See the
[operator guide](M2_OPERATOR_GUIDE.md#certificate-rotation).

## Verification

The final CGO-free build, full race suite and lint passed on the unchanged source:
22 tested packages, 3,608 named passes, zero failures and two existing optional
skips. All 305 MariaDB-named passes ran; 293 explicitly select the actual live
engine, with zero engine skips. Lint reported zero issues. All 32 Go/SQL/Python
hashes stayed unchanged. The independent compiled process run passed all 29 stages.
See [the evidence ledger](M3_HUB_CERTIFICATE_EVIDENCE.json) for source, binary and
log hashes, exact engine inventory, skipped cases and failure dispositions.

The focused suite passed 80 named cases without failures/skips across SQLite,
actual MariaDB, services and CLI regressions. It includes the reproduced UTC+7
expiry fix, receipt/pin/ciphertext rollback, concurrent issuance, migration guards,
command capacity reservation, tamper rejection, exclusion and fallback fencing.

`TestHubCertificateRuntimeLostResultAcrossBothStoresRestartAfterRetirement`
uses actual TLS and both databases with a certificate-only command capability,
loses an activation result, cold-reopens both
sides and recovers after the original fixed deadline. Its immutable command starts
near the end of the ten-minute window; it does not rewrite a durable deadline.
The final independent compiled-process harness exercises ordinary full-window issuance,
offline restart, both source receipts, promoted-certificate restart and continued
ordered telemetry under production connector services.

## Scope and limitations

No new dependencies, frontend, checker/provider types or HTTP fleet APIs were
added. No push/deployment occurred. Existing `AGENTS.md` user changes are excluded.
Antigravity's audit/follow-up returned service errors; its useful findings were
independently tested. See [the retrospective](M3_HUB_CERTIFICATE_RETROSPECTIVE.md).

[Explicit stream reset](M3_STREAM_RESET_WORK_CONTRACT.md), remaining pressure/flush acceptance and the complete
fifteen-minute partition are unfinished M3 requirements. Existing short process
runs and near-expiry recovery tests do not substitute for that long partition.
