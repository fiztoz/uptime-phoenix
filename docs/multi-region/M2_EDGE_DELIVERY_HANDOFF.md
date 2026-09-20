# M2 Edge Delivery Persistence Handoff

## Summary

This handoff documents the implementation and verification of the edge delivery persistence slice for M2, complying with `docs/multi-region/M2_EDGE_DELIVERY_WORK_CONTRACT.md`.

## Owned Files

- `internal/adapters/repository/edge/delivery.go`
- `internal/adapters/repository/edge/delivery_test.go`
- `docs/multi-region/M2_EDGE_DELIVERY_HANDOFF.md`

No other files or shared documents were modified.

## Implemented Interface on `*edge.Store`

`*edge.Store` implements `ports.DeliveryOutboxRepository` using the existing schema (`edge_delivery_outbox` and `edge_telemetry_outbox` from migration 002):

```go
var _ ports.DeliveryOutboxRepository = (*Store)(nil)

func (s *Store) ClaimDeliveries(ctx context.Context, probeID string, at time.Time, lease time.Duration, limit int) ([]domain.QueuedDelivery, error)
func (s *Store) FinishDelivery(ctx context.Context, claim domain.DeliveryClaim, result domain.DeliveryResult) error
func (s *Store) GetDeliveryIntent(ctx context.Context, probeID, deliveryID string) (*domain.QueuedDelivery, error)
```

## Contract Requirements & Implementation Details

1. **Claim Mechanics (`ClaimDeliveries`)**:
   - Executes inside `s.write`, ensuring the SQLite database writer lock (`UPDATE edge_identity SET id = id WHERE id = 1`) is acquired prior to any eligibility query.
   - Bounded inputs: `limit` in `[1, 100]`, `lease` in `[1s, 15m]`, `at` non-zero, and `probeID == i.ProbeID`. Mismatched probe scope fails with `ports.ErrConflict`.
   - Eligibility query selects rows with `attempt < math.MaxInt64` that are due:
     `(status IN ('pending', 'retrying') AND available_at <= atMicro) OR (status = 'leased' AND lease_until <= atMicro)`
   - Deterministic ordering: `ORDER BY available_at ASC, created_at ASC, delivery_id ASC LIMIT limit`.
   - Each claimed row has its attempt incremented (`row.Attempt + 1`), receives a fresh 36-character UUID lease token via `uuid.NewRandom()` (no panics), and updates `leased_at` and `lease_until`.
   - Returns immutable stored context with microsecond timestamps converted to UTC, with `ProbeID` and `StreamID` populated from `edge_identity`.

2. **Outcome Completion & Telemetry (`FinishDelivery`)**:
   - Validates bounded diagnostic code (`validDeliveryErrorCode`: lowercase alphanumeric with underscores, length 1..128), allowed statuses (`sent`, `failed`, `retrying`, `superseded`), non-zero outcome time `result.At` in UTC microsecond precision, and `result.RetryAt` strictly later than `result.At` (only for `retrying`). Terminal statuses enforce `result.RetryAt.IsZero()`.
   - Verifies exact probe scope (`claim.ProbeID == i.ProbeID`), matching attempt (`row.Attempt == claim.Attempt`), and valid lease token (`row.LeaseToken == claim.LeaseToken`).
   - Verifies claim is unexpired at outcome time: `resultAtMicro >= *row.LeasedAt && resultAtMicro < *row.LeaseUntil`. Stale finishes fail with `ports.ErrConflict`.
   - **Idempotency**: Retrying with the exact same completed receipt (matching status, error code, outcome time, and retry time) returns `nil` without allocating a new sequence or emitting duplicate telemetry events. Conflicting receipts fail with `ports.ErrConflict`.
   - **Atomic Transaction**:
     1. Allocates next sequence counter `nextSeq := i.LastCreatedSeq + 1` with overflow check (`math.MaxInt64`).
     2. Encodes a `domain.RegionalDelivery` via `s.telemetry.EncodeDelivery(nextSeq, d)`.
     3. Appends exact `delivery.result` event payload to `edge_telemetry_outbox` via `appendTelemetry(ctx, tx, nextSeq, "delivery.result", resultAt, payload)`.
     4. Updates `edge_delivery_outbox` row: sets `status`, `outcome_at`, `error_code`, clears `lease_until = NULL`, and updates `available_at = retryAt` if retrying.
     5. Advances `edge_identity.last_created_seq = nextSeq`.
   - Any failure (encoder, queue full, SQL error, or trigger abort) rolls back the entire transaction.

3. **Scoped Intent Retrieval (`GetDeliveryIntent`)**:
   - Validates `probeID` and `deliveryID` (UUID).
   - Validates exact probe scope (`probeID == i.ProbeID`). Searches for other probe IDs return `ports.ErrNotFound` without inventing defaults.
   - Returns the stored `domain.QueuedDelivery` with UTC microsecond timestamps and identity from `edge_identity`.

## Observed Verification Evidence

### 1. Focused Unit Tests (Race Detector Enabled)

Command:
```bash
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -v -race -count=1 ./internal/adapters/repository/edge -run "^TestEdgeDelivery"
```

Output:
```
=== RUN   TestEdgeDelivery_DueAndNotDue
--- PASS: TestEdgeDelivery_DueAndNotDue (0.05s)
=== RUN   TestEdgeDelivery_ExpiredReclaimAndStaleFinish
--- PASS: TestEdgeDelivery_ExpiredReclaimAndStaleFinish (0.04s)
=== RUN   TestEdgeDelivery_WrongScope
--- PASS: TestEdgeDelivery_WrongScope (0.03s)
=== RUN   TestEdgeDelivery_DeterministicOrder
--- PASS: TestEdgeDelivery_DeterministicOrder (0.03s)
=== RUN   TestEdgeDelivery_NoOverflow
--- PASS: TestEdgeDelivery_NoOverflow (0.04s)
=== RUN   TestEdgeDelivery_IdempotentCompletedReceipt
--- PASS: TestEdgeDelivery_IdempotentCompletedReceipt (0.04s)
=== RUN   TestEdgeDelivery_ConflictingReceipt
--- PASS: TestEdgeDelivery_ConflictingReceipt (0.04s)
=== RUN   TestEdgeDelivery_OutcomesAndTelemetryDecoding
=== RUN   TestEdgeDelivery_OutcomesAndTelemetryDecoding/sent
=== RUN   TestEdgeDelivery_OutcomesAndTelemetryDecoding/failed
=== RUN   TestEdgeDelivery_OutcomesAndTelemetryDecoding/retrying
=== RUN   TestEdgeDelivery_OutcomesAndTelemetryDecoding/superseded
--- PASS: TestEdgeDelivery_OutcomesAndTelemetryDecoding (0.15s)
    --- PASS: TestEdgeDelivery_OutcomesAndTelemetryDecoding/sent (0.04s)
    --- PASS: TestEdgeDelivery_OutcomesAndTelemetryDecoding/failed (0.04s)
    --- PASS: TestEdgeDelivery_OutcomesAndTelemetryDecoding/retrying (0.04s)
    --- PASS: TestEdgeDelivery_OutcomesAndTelemetryDecoding/superseded (0.04s)
=== RUN   TestEdgeDelivery_RestartDurability
--- PASS: TestEdgeDelivery_RestartDurability (0.04s)
=== RUN   TestEdgeDelivery_InjectedFailureRollback
--- PASS: TestEdgeDelivery_InjectedFailureRollback (0.04s)
PASS
ok  	github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge	2.052s
```
Exit Code: 0.

### 2. Static Analysis & Linter Verification

- `gofmt -l internal/adapters/repository/edge/delivery*.go`: output is empty.
- `rtk proxy env GOTOOLCHAIN=go1.26.6 go vet ./internal/adapters/repository/edge/...`: exited 0 with no warnings.
- `GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run ./internal/adapters/repository/edge/...`: exited 0 with `0 issues.`.

## Boundary Commitments

- No commits, pushes, or deployments.
- No modifications to unowned files.
- This slice alone does not complete M2.
