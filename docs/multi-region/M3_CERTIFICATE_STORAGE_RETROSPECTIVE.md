# Certificate preparation compatibility retrospective

## Summary

During Codex's certificate storage implementation on
`codex/multi-region-probe-plan` after `a525ca0`, the local prepare path could panic
when Go 1.26.6 ran with `GODEBUG=x509keypairleaf=0`. Explicitly parsing the
certificate leaf fixes generation, protected-material opening and bootstrap-file
reopening. The regression was found and fixed before this foundation was committed
or exposed through runtime capability negotiation. No deployed impact is claimed.

## Symptom

The direct Go 1.26.6 reproduction failed in
`TestEdgeCertificateMaterialValidatesExactLocalIdentity` with
`panic: runtime error: invalid memory address or nil pointer dereference` at
`EdgeCertificateMaterial.PrepareCertificate`, while constructing metadata from
`cert.Leaf.NotBefore`. The default configuration passed the same workload.

## Root cause

`generateRuntimeTLSFor` used `tls.X509KeyPair`. Go 1.23 and later populate
`Certificate.Leaf` by default, but the supported `x509keypairleaf=0` compatibility
setting leaves it nil. The new preparation code dereferenced it without explicitly
parsing DER. `OpenEdgeCertificate` also assumed it would be populated and rejected
otherwise valid protected material. The bootstrap reader parsed a local leaf for
its own checks but did not assign that leaf back to the returned TLS certificate.

This was a Codex implementation assumption. Antigravity's claim that X509KeyPair
*always* leaves Leaf nil was outdated; only the compatibility case was reproduced.
The installed compiler's `crypto/tls/tls.go` documents and implements this distinction.

## Fix

The branch explicitly parses DER and sets `cert.Leaf` in
`generateRuntimeTLSFor`, `OpenEdgeCertificate` and `readAndValidateTLSFile`.
Successful key-pair parsing no longer implies a populated optional field. All
existing fingerprint, key-pair, certificate-purpose and validity checks remain.
Errors return normally instead of dereferencing a nil pointer.

## How it was found

1. Antigravity raised an unconditional nil-leaf claim during the bounded storage
   audit. Existing default tests falsified the unconditional claim.
2. Codex read the actual Go 1.26.6 standard-library source and identified the
   compatibility switch as the remaining condition.
3. The first environment-based attempt failed in the newer system Go launcher
   before tests ran: `removed GODEBUG "x509keypairleaf" set to old value "0"`.
   That output is launcher behavior, not a certificate-source reproduction.
4. Invoking the repository's Go 1.26.6 binary directly under the setting reproduced
   the precise source panic. After the fix, the original material test passed.
5. The added identity reopen test initially used a mode-0755 temporary directory
   and correctly failed the production permission check. The fixture was corrected
   to mode 0700; the permission guard was not relaxed.

## Why it slipped through

The initial crypto and persistence tests ran only with default leaf population.
Their passing effects demonstrated working certificate preparation, but did not
exercise the compatibility setting. Review also needed toolchain-specific evidence:
remembering older Go behavior produced an overbroad diagnosis, while relying only
on default behavior missed the narrow panic.

## Validation

`TestEdgeCertificateMaterialValidatesExactLocalIdentity` now runs both default and
compatibility modes, covering real local generation, protected opening, fingerprint
and validity mismatch, malformed PEM and validity boundaries.
`TestRuntimeIdentityLeafWithCompatibilitySetting` initializes and reopens actual
private identity files and verifies both retain a parsed leaf and the same pin.
The direct Go 1.26.6 race-enabled compatibility rerun passed with exit zero in
`/private/tmp/phoenix-m3-certificate-leaf-compat-fixed-v2.jsonl`.

The broader final gate and immutable source hashes are recorded in
[storage acceptance](M3_CERTIFICATE_STORAGE_ACCEPTANCE.md) and its evidence file.
This correction does not prove live certificate switching or hub recovery; those
remain separately tracked in the certificate contract.

## Review lessons and follow-up

Antigravity acknowledged the corrections successfully in conversation
`95a7fbec-80fe-4f08-88d4-b5046ce8b1cc`. It changed no files. Reviewers must inspect
actual compiler behavior, read failure output before attributing an exit code,
and distinguish default assumptions from compatibility settings.

The suggestion to close an overlap immediately on activation was rejected: this
contract deliberately preserves a fixed ten-minute retirement/recovery window
and serializes further rotations until it ends. A documented `rotation_in_progress`
result during that window is expected. UTC microsecond timestamps also round-trip
through the chosen encoding; no timestamp defect was reproduced.

Codex owns the next runtime/hub implementation and its failure tests under
[the certificate work contract](M3_CERTIFICATE_ROTATION_WORK_CONTRACT.md). The
compatibility regression is in the ordinary test suite; no separate speculative
follow-up is required for the fixed nil-leaf issue.
