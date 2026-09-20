# Edge delivery persistence work contract

Antigravity owns ONLY `internal/adapters/repository/edge/delivery.go`,
`internal/adapters/repository/edge/delivery_test.go`, and
`docs/multi-region/M2_EDGE_DELIVERY_HANDOFF.md`. Other files are Codex-owned.
Keep Gemini 3.8 Flash High. Do not commit/push. Read AGENTS.md and existing
`edge/store.go`, `record.go`, `record_test.go`, migration 002, domain delivery
types, existing hub `delivery_outbox.go`, and the protocol delivery result codec.

Implement `ports.DeliveryOutboxRepository` on `*edge.Store` using the existing
schema. Freeze the three existing methods: ClaimDeliveries, FinishDelivery,
GetDeliveryIntent. No provider I/O and no new interfaces/migrations required.

Claim within `s.write` (writer lock before eligibility read), bound input (1–100,
lease 1 second–15 minutes), verify exact local probe scope, deterministic
available_at/created_at/delivery_id. Claim pending/retrying due rows or expired
leases. UUID via uuid.NewRandom (no panic); increment attempt without overflow.
Return immutable stored context with microsecond times converted UTC and
stream/probe identity from edge_identity. Reclaim uses a fresh persisted token.

Finish validates bounded diagnostic code, allowed status, UTC microsecond times,
retry time only for retrying and strictly later than outcome time. Only the
current unexpired claim may complete. Identical completed receipt is idempotent
(including timestamp/retry/error); conflicting receipt or superseded attempt
fails. Within ONE transaction update queue, allocate persistent global sequence,
encode a domain.RegionalDelivery via s.telemetry.EncodeDelivery, append exact
delivery.result via appendTelemetry and advance edge_identity.last_created_seq.
An encoder/queue-full/SQL failure or counter overflow rolls everything back.
No secrets, raw provider error or plaintext configs in persisted diagnostics.
Get reads the stored row only for exact scope and does not invent defaults.

Tests use REAL edge SQLite and actual telemetry encoder. Exercise due/not-due,
expired reclaim and stale finish, wrong scope, deterministic order, no overflow,
same completed receipt/no extra sequence, conflicting receipt, failed/sent/retry/
superseded outcomes, restart durability, and injected failure on outcome event or
counter update rolling back queue AND sequence. Reuse record_test.go's setup or
mirror it; do not edit it. Check the resulting stored event using real decoder.

Run focused `TestEdgeDelivery*` race once after implementation; targeted reruns
only for failures or changes. Go1.26.6. Prefix commands rtk. No whole-repo tests;
Codex runs integration gates. Lint the edge package and report exact outcome,
including unrelated concurrent-file failures. Do not weaken assertions to make
an implementation pass. Record remaining concerns honestly. A slice ≠ M2 done.
