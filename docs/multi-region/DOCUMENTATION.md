# Documentation maintenance

## Current reading order

[README](README.md) → [status](IMPLEMENTATION_STATUS.md) →
[continuation](CONTINUATION_GUIDE.md), then the relevant architecture/protocol and
implementation-plan sections. Operator instructions live in
[the runtime guide](M2_OPERATOR_GUIDE.md), [key provisioning](KEY_PROVISIONING.md)
and the explicit [reset workflow](M3_HUB_RESET_ACCEPTANCE.md).

## Keep in version control

- Design decisions, wire contracts, implementation plan and current status.
- Operator and test procedures that a fresh checkout needs.
- [M2 acceptance](M2_ACCEPTANCE_REPORT.md), [M3 final acceptance](M3_COMPLETION_ACCEPTANCE.md)
  and checkpoint acceptance/evidence files referenced by those records. A checkpoint's
  old remaining-work statement describes that checkpoint, not current status.
- [M0/M1 corrections](../postmortems/2026-09-20-m01-local-cutover.md),
  [M1 integration](../postmortems/2026-09-20-m1-integration-followup.md), and
  [consolidated M3 lessons](../postmortems/2026-09-21-m3-integration.md).

Evidence JSON is retained unchanged: old hashes, failures, skips and incomplete
checkpoint claims must not be rewritten to look like the final result. Temporary
log paths identify local execution artifacts; they are not portable dependencies.

## Local archive and removal — 2026-09-21

The pre-cleanup baseline is `8b455d4c534adf707b1085b95b255f9569165ef3`.
Thirty-five completed work/review contracts and detailed M3 retrospectives were
copied byte-for-byte to `docs/local/archive/2026-09-21-m3-complete/`, together with
the previous status and continuation ledgers (37 originals total). Its local
`manifest.json` records source paths and SHA-256 checksums. Its README explains
recovery and the historical scope of those notes. The existing `docs/local/`
ignore rule keeps this archive local; it is not shipped or required by live docs.

Thirteen superseded handoff/review drafts were deleted rather than copied locally.
Their accepted behavior is covered by retained acceptance reports, and the lasting
review lessons are consolidated in the postmortems. They remain recoverable from
the baseline commit. To inspect any removed file without changing the checkout:

```sh
rtk proxy git show 8b455d4:docs/multi-region/NAME.md
```

Future cleanup should preserve unique design decisions, runnable operator guidance
and evidence before removing a ledger. Archive unfinished research only when it is
explicitly inactive. Delete duplicate completion messages and obsolete agent ownership
instructions once acceptance supersedes them. Update incoming links in the same
commit; never make a public document depend on an ignored local archive.
