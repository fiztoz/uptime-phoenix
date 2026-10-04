package edge

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func deliveryCheckRecord(availableAt time.Time) domain.EdgeCheckRecord {
	rec := checkRecord()
	rec.DeliveryIntents[0].AvailableAt = availableAt.UTC().Truncate(time.Microsecond)
	return rec
}

func setupEdgeDeliveryStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestEdgeDelivery_DueAndNotDue(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	dueTime := now.Add(10 * time.Second)
	rec := deliveryCheckRecord(dueTime)

	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatalf("commit check: %v", err)
	}

	// Claim before due time -> 0 claimed
	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 5*time.Second, 10)
	if err != nil {
		t.Fatalf("claim before due: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("expected 0 claimed before due, got %d", len(claimed))
	}

	// Claim at due time -> 1 claimed
	claimed, err = s.ClaimDeliveries(ctx, i.ProbeID, dueTime, 5*time.Second, 10)
	if err != nil {
		t.Fatalf("claim at due: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed at due, got %d", len(claimed))
	}

	item := claimed[0]
	if item.DeliveryID != "65f61ca6-9802-48b9-9189-377e7c6ec25b" {
		t.Fatalf("wrong delivery ID: %s", item.DeliveryID)
	}
	if item.ProbeID != i.ProbeID || item.StreamID != i.StreamID {
		t.Fatalf("wrong probe/stream: probe=%s stream=%s", item.ProbeID, item.StreamID)
	}
	if item.Status != domain.DeliveryStatusLeased || item.Attempt != 1 || len(item.LeaseToken) != 36 {
		t.Fatalf("invalid claimed state: status=%s attempt=%d token=%s", item.Status, item.Attempt, item.LeaseToken)
	}
	if item.LeasedAt == nil || item.LeaseUntil == nil || !item.LeaseUntil.After(*item.LeasedAt) {
		t.Fatalf("invalid lease bounds: %+v %+v", item.LeasedAt, item.LeaseUntil)
	}

	// Immediate second claim should find 0 due items
	claimed2, err := s.ClaimDeliveries(ctx, i.ProbeID, dueTime, 5*time.Second, 10)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(claimed2) != 0 {
		t.Fatalf("expected 0 on second claim, got %d", len(claimed2))
	}
}

func TestEdgeDelivery_ExpiredReclaimAndStaleFinish(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// Claim with 1 second lease
	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 1*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("initial claim: %v len=%d", err, len(claimed))
	}
	attempt1 := claimed[0]

	// Stale finish: try to complete after lease expired
	staleResult := domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(2 * time.Second),
	}
	err = s.FinishDelivery(ctx, domain.DeliveryClaim{
		DeliveryID: attempt1.DeliveryID,
		ProbeID:    attempt1.ProbeID,
		Attempt:    attempt1.Attempt,
		LeaseToken: attempt1.LeaseToken,
	}, staleResult)
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for stale finish, got %v", err)
	}

	// Reclaim after lease expiration
	reclaimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now.Add(2*time.Second), 5*time.Second, 1)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("reclaim: %v len=%d", err, len(reclaimed))
	}
	attempt2 := reclaimed[0]

	if attempt2.Attempt != 2 {
		t.Fatalf("expected attempt 2, got %d", attempt2.Attempt)
	}
	if attempt2.LeaseToken == attempt1.LeaseToken {
		t.Fatalf("expected fresh lease token on reclaim, got %s", attempt2.LeaseToken)
	}

	// Stale attempt 1 cannot finish now
	err = s.FinishDelivery(ctx, domain.DeliveryClaim{
		DeliveryID: attempt1.DeliveryID,
		ProbeID:    attempt1.ProbeID,
		Attempt:    attempt1.Attempt,
		LeaseToken: attempt1.LeaseToken,
	}, domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(3 * time.Second),
	})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for superseded attempt finish, got %v", err)
	}

	// Valid attempt 2 finish within lease
	validResult := domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(3 * time.Second),
	}
	if err := s.FinishDelivery(ctx, domain.DeliveryClaim{
		DeliveryID: attempt2.DeliveryID,
		ProbeID:    attempt2.ProbeID,
		Attempt:    attempt2.Attempt,
		LeaseToken: attempt2.LeaseToken,
	}, validResult); err != nil {
		t.Fatalf("finish attempt 2: %v", err)
	}

	// Verify finished state via GetDeliveryIntent
	intent, err := s.GetDeliveryIntent(ctx, i.ProbeID, attempt2.DeliveryID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if intent.Status != domain.DeliveryStatusSent || intent.OutcomeAt == nil {
		t.Fatalf("unexpected stored intent: %+v", intent)
	}
}

func TestEdgeDelivery_WrongScope(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	wrongProbe := "4d5e29ba-a1c4-4e39-a0c7-f73b35a2c521"

	// Claim with wrong scope -> ErrConflict
	_, err := s.ClaimDeliveries(ctx, wrongProbe, now, 5*time.Second, 10)
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for wrong probe claim, got %v", err)
	}

	// Claim with correct scope
	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 5*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("correct claim: %v len=%d", err, len(claimed))
	}

	// Finish with wrong scope -> ErrConflict
	err = s.FinishDelivery(ctx, domain.DeliveryClaim{
		DeliveryID: claimed[0].DeliveryID,
		ProbeID:    wrongProbe,
		Attempt:    claimed[0].Attempt,
		LeaseToken: claimed[0].LeaseToken,
	}, domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(1 * time.Second),
	})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for wrong probe finish, got %v", err)
	}

	// Get with wrong scope -> ErrNotFound
	_, err = s.GetDeliveryIntent(ctx, wrongProbe, claimed[0].DeliveryID)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for wrong probe get, got %v", err)
	}

	// Get with correct scope -> success
	got, err := s.GetDeliveryIntent(ctx, i.ProbeID, claimed[0].DeliveryID)
	if err != nil || got.DeliveryID != claimed[0].DeliveryID {
		t.Fatalf("get delivery intent failed: %v", err)
	}
}

func TestEdgeDelivery_DeterministicOrder(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	t1 := time.Now().UTC().Truncate(time.Microsecond)
	t2 := t1.Add(5 * time.Second)

	// We insert 3 records into edge_delivery_outbox directly or via CommitEdgeCheck:
	// Record C: available_at = t1, created_at = t1, id = 00000000-0000-0000-0000-000000000003
	// Record B: available_at = t1, created_at = t2, id = 00000000-0000-0000-0000-000000000002
	// Record A: available_at = t2, created_at = t1, id = 00000000-0000-0000-0000-000000000001
	rec := checkRecord()
	rec.DeliveryIntents = []domain.DeliveryIntent{
		{DeliveryID: "00000000-0000-0000-0000-000000000001", SourceAlertID: rec.Incident.SourceAlertID, SourceTransitionVersion: 1, ProbeID: i.ProbeID, NotificationID: 10, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: t2},
		{DeliveryID: "00000000-0000-0000-0000-000000000002", SourceAlertID: rec.Incident.SourceAlertID, SourceTransitionVersion: 1, ProbeID: i.ProbeID, NotificationID: 11, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: t1},
		{DeliveryID: "00000000-0000-0000-0000-000000000003", SourceAlertID: rec.Incident.SourceAlertID, SourceTransitionVersion: 1, ProbeID: i.ProbeID, NotificationID: 12, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: t1},
	}
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// Adjust created_at on record B to t2
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_delivery_outbox SET created_at = ? WHERE delivery_id = ?", t2.UnixMicro(), "00000000-0000-0000-0000-000000000002"); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, t2, 10*time.Second, 10)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("claim all: %v len=%d", err, len(claimed))
	}

	// Expected order:
	// 1. C (t1, t1, ...03)
	// 2. B (t1, t2, ...02)
	// 3. A (t2, t1, ...01)
	expectedIDs := []string{
		"00000000-0000-0000-0000-000000000003",
		"00000000-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-000000000001",
	}
	for idx, exp := range expectedIDs {
		if claimed[idx].DeliveryID != exp {
			t.Errorf("claim[%d] = %s, want %s", idx, claimed[idx].DeliveryID, exp)
		}
	}
}

func TestEdgeDelivery_NoOverflow(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// Claim it first so status = 'leased'
	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 1*time.Second, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v len=%d", err, len(claimed))
	}

	// Set attempt to MaxInt64 in database while leased
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_delivery_outbox SET attempt = ? WHERE delivery_id = ?", int64(math.MaxInt64), "65f61ca6-9802-48b9-9189-377e7c6ec25b"); err != nil {
		t.Fatal(err)
	}

	// ClaimDeliveries after lease expiration should not reclaim it without overflow
	claimedAfter, err := s.ClaimDeliveries(ctx, i.ProbeID, now.Add(2*time.Second), 5*time.Second, 10)
	if err != nil {
		t.Fatalf("claim with max attempt: %v", err)
	}
	if len(claimedAfter) != 0 {
		t.Fatalf("expected 0 claimed for max attempt, got %d", len(claimedAfter))
	}

	// Reset attempt to 1 so it can be reclaimed
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_delivery_outbox SET attempt = 1 WHERE delivery_id = ?", "65f61ca6-9802-48b9-9189-377e7c6ec25b"); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now.Add(2*time.Second), 5*time.Second, 10)
	if err != nil || len(reclaimed) != 1 {
		t.Fatalf("re-claim: %v len=%d", err, len(reclaimed))
	}

	// Set last_created_seq to MaxInt64
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_identity SET last_created_seq = ? WHERE id = 1", int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}

	// FinishDelivery should fail with ErrConflict due to counter overflow
	err = s.FinishDelivery(ctx, domain.DeliveryClaim{
		DeliveryID: reclaimed[0].DeliveryID,
		ProbeID:    reclaimed[0].ProbeID,
		Attempt:    reclaimed[0].Attempt,
		LeaseToken: reclaimed[0].LeaseToken,
	}, domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(3 * time.Second),
	})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict on sequence counter overflow, got %v", err)
	}

	// Verify row is still leased and no telemetry was appended
	var status string
	if err := s.db.NewRaw("SELECT status FROM edge_delivery_outbox WHERE delivery_id = ?", reclaimed[0].DeliveryID).Scan(ctx, &status); err != nil {
		t.Fatal(err)
	}
	if status != domain.DeliveryStatusLeased {
		t.Fatalf("expected row to remain leased after rollback, got %s", status)
	}
}

func TestEdgeDelivery_IdempotentCompletedReceipt(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 5*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v", err)
	}
	claim := domain.DeliveryClaim{
		DeliveryID: claimed[0].DeliveryID,
		ProbeID:    claimed[0].ProbeID,
		Attempt:    claimed[0].Attempt,
		LeaseToken: claimed[0].LeaseToken,
	}
	result := domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(1 * time.Second),
	}

	// First finish succeeds
	if err := s.FinishDelivery(ctx, claim, result); err != nil {
		t.Fatalf("first finish: %v", err)
	}

	idAfter1, err := s.ReadIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var count1 int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_telemetry_outbox WHERE kind = 'delivery.result'").Scan(ctx, &count1); err != nil || count1 != 1 {
		t.Fatalf("expected 1 delivery.result in outbox, got %d (err: %v)", count1, err)
	}

	// Second finish with exact same receipt is idempotent no-op
	if err := s.FinishDelivery(ctx, claim, result); err != nil {
		t.Fatalf("idempotent finish failed: %v", err)
	}

	idAfter2, err := s.ReadIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if idAfter2.LastCreatedSeq != idAfter1.LastCreatedSeq {
		t.Fatalf("sequence advanced on idempotent repeat! %d != %d", idAfter2.LastCreatedSeq, idAfter1.LastCreatedSeq)
	}

	var count2 int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_telemetry_outbox WHERE kind = 'delivery.result'").Scan(ctx, &count2); err != nil || count2 != 1 {
		t.Fatalf("expected still 1 delivery.result in outbox, got %d", count2)
	}
}

func TestEdgeDelivery_ConflictingReceipt(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 5*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	claim := domain.DeliveryClaim{
		DeliveryID: claimed[0].DeliveryID,
		ProbeID:    claimed[0].ProbeID,
		Attempt:    claimed[0].Attempt,
		LeaseToken: claimed[0].LeaseToken,
	}
	result := domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(1 * time.Second),
	}
	if err := s.FinishDelivery(ctx, claim, result); err != nil {
		t.Fatal(err)
	}

	// Conflicting status
	conflictStatus := domain.DeliveryResult{
		Status:    domain.DeliveryStatusFailed,
		ErrorCode: "provider_error",
		At:        now.Add(1 * time.Second),
	}
	if err := s.FinishDelivery(ctx, claim, conflictStatus); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict on conflicting status, got %v", err)
	}

	// Conflicting outcome time
	conflictTime := domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(2 * time.Second),
	}
	if err := s.FinishDelivery(ctx, claim, conflictTime); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict on conflicting time, got %v", err)
	}
}

func TestEdgeDelivery_OutcomesAndTelemetryDecoding(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		errorCode string
		isRetry   bool
	}{
		{name: "sent", status: domain.DeliveryStatusSent},
		{name: "failed", status: domain.DeliveryStatusFailed, errorCode: "network_timeout"},
		{name: "retrying", status: domain.DeliveryStatusRetrying, errorCode: "rate_limited", isRetry: true},
		{name: "superseded", status: domain.DeliveryStatusSuperseded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setupEdgeDeliveryStore(t)
			ctx := t.Context()
			i := testIdentity()

			now := time.Now().UTC().Truncate(time.Microsecond)
			rec := deliveryCheckRecord(now)
			if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
				t.Fatal(err)
			}

			claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 5*time.Second, 1)
			if err != nil || len(claimed) != 1 {
				t.Fatal(err)
			}

			claim := domain.DeliveryClaim{
				DeliveryID: claimed[0].DeliveryID,
				ProbeID:    claimed[0].ProbeID,
				Attempt:    claimed[0].Attempt,
				LeaseToken: claimed[0].LeaseToken,
			}
			result := domain.DeliveryResult{
				Status:    tc.status,
				ErrorCode: tc.errorCode,
				At:        now.Add(1 * time.Second),
			}
			if tc.isRetry {
				result.RetryAt = now.Add(10 * time.Second)
			}

			if err := s.FinishDelivery(ctx, claim, result); err != nil {
				t.Fatalf("finish delivery: %v", err)
			}

			// Verify stored intent
			intent, err := s.GetDeliveryIntent(ctx, i.ProbeID, claim.DeliveryID)
			if err != nil {
				t.Fatalf("get intent: %v", err)
			}
			if intent.Status != tc.status || intent.ErrorCode != tc.errorCode {
				t.Fatalf("stored intent mismatch: status=%s, error_code=%s", intent.Status, intent.ErrorCode)
			}
			if tc.isRetry {
				if intent.AvailableAt.UnixMicro() != result.RetryAt.UnixMicro() {
					t.Fatalf("available_at not updated to retry_at: got %v, want %v", intent.AvailableAt, result.RetryAt)
				}
				// Re-claim after retry_at
				reclaimed, err := s.ClaimDeliveries(ctx, i.ProbeID, result.RetryAt, 5*time.Second, 1)
				if err != nil || len(reclaimed) != 1 {
					t.Fatalf("re-claim retried item: %v len=%d", err, len(reclaimed))
				}
			}

			// Verify edge_telemetry_outbox event
			var payload []byte
			var seq int64
			var kind string
			err = s.db.NewRaw("SELECT seq, kind, payload FROM edge_telemetry_outbox WHERE kind = 'delivery.result' ORDER BY seq DESC LIMIT 1").
				Scan(ctx, &seq, &kind, &payload)
			if err != nil {
				t.Fatalf("read telemetry outbox: %v", err)
			}
			if kind != "delivery.result" {
				t.Fatalf("unexpected kind: %s", kind)
			}

			var event struct {
				Seq        string `json:"seq"`
				Kind       string `json:"kind"`
				ObservedAt string `json:"observed_at"`
				Data       struct {
					DeliveryID              string  `json:"delivery_id"`
					SourceAlertID           string  `json:"source_alert_id"`
					SourceTransitionVersion string  `json:"source_transition_version"`
					NotificationID          int64   `json:"notification_id"`
					NotificationVersion     string  `json:"notification_version"`
					EventKind               string  `json:"event_kind"`
					Attempt                 int64   `json:"attempt"`
					Status                  string  `json:"status"`
					ErrorCode               *string `json:"error_code"`
				} `json:"data"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatalf("unmarshal telemetry event payload: %v", err)
			}
			if event.Kind != "delivery.result" || event.Data.DeliveryID != claim.DeliveryID || event.Data.Status != tc.status {
				t.Fatalf("telemetry payload content mismatch: %+v", event)
			}
			if tc.errorCode == "" {
				if event.Data.ErrorCode != nil {
					t.Fatalf("expected nil error_code, got %v", *event.Data.ErrorCode)
				}
			} else {
				if event.Data.ErrorCode == nil || *event.Data.ErrorCode != tc.errorCode {
					t.Fatalf("expected error_code %s, got %v", tc.errorCode, event.Data.ErrorCode)
				}
			}
		})
	}
}

func TestEdgeDelivery_RestartDurability(t *testing.T) {
	s, dir := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 5*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	claim := domain.DeliveryClaim{
		DeliveryID: claimed[0].DeliveryID,
		ProbeID:    claimed[0].ProbeID,
		Attempt:    claimed[0].Attempt,
		LeaseToken: claimed[0].LeaseToken,
	}
	result := domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(1 * time.Second),
	}
	if err := s.FinishDelivery(ctx, claim, result); err != nil {
		t.Fatal(err)
	}

	// Close the store
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen store
	reopened, err := Open(ctx, dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()

	// Verify intent is durable
	intent, err := reopened.GetDeliveryIntent(ctx, i.ProbeID, claim.DeliveryID)
	if err != nil {
		t.Fatalf("get intent on reopened: %v", err)
	}
	if intent.Status != domain.DeliveryStatusSent || intent.OutcomeAt == nil {
		t.Fatalf("durable intent state mismatch: %+v", intent)
	}

	// Verify telemetry row is durable
	var count int
	if err := reopened.db.NewRaw("SELECT COUNT(*) FROM edge_telemetry_outbox WHERE kind = 'delivery.result'").Scan(ctx, &count); err != nil || count != 1 {
		t.Fatalf("durable telemetry count: %d, err: %v", count, err)
	}
}

func TestEdgeDelivery_InjectedFailureRollback(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	ctx := t.Context()
	i := testIdentity()

	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := deliveryCheckRecord(now)
	if _, err := s.CommitEdgeCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimDeliveries(ctx, i.ProbeID, now, 5*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	claim := domain.DeliveryClaim{
		DeliveryID: claimed[0].DeliveryID,
		ProbeID:    claimed[0].ProbeID,
		Attempt:    claimed[0].Attempt,
		LeaseToken: claimed[0].LeaseToken,
	}
	result := domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     now.Add(1 * time.Second),
	}

	idBefore, err := s.ReadIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Inject failure on edge_telemetry_outbox insert
	if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_telemetry BEFORE INSERT ON edge_telemetry_outbox BEGIN SELECT RAISE(ABORT, 'injected telemetry fault'); END"); err != nil {
		t.Fatal(err)
	}

	err = s.FinishDelivery(ctx, claim, result)
	if err == nil {
		t.Fatal("expected failure on injected telemetry fault, got nil")
	}

	// Verify rollback: row still leased, last_created_seq unchanged
	intent, err := s.GetDeliveryIntent(ctx, i.ProbeID, claim.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Status != domain.DeliveryStatusLeased {
		t.Fatalf("row not rolled back to leased, got %s", intent.Status)
	}

	idAfter1, err := s.ReadIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if idAfter1.LastCreatedSeq != idBefore.LastCreatedSeq {
		t.Fatalf("sequence advanced on failed finish: %d != %d", idAfter1.LastCreatedSeq, idBefore.LastCreatedSeq)
	}

	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_telemetry"); err != nil {
		t.Fatal(err)
	}

	// 2. Inject failure on edge_identity update
	if _, err := s.db.ExecContext(ctx, "CREATE TRIGGER fail_identity BEFORE UPDATE ON edge_identity BEGIN SELECT RAISE(ABORT, 'injected identity fault'); END"); err != nil {
		t.Fatal(err)
	}

	err = s.FinishDelivery(ctx, claim, result)
	if err == nil {
		t.Fatal("expected failure on injected identity fault, got nil")
	}

	// Verify rollback
	intent, err = s.GetDeliveryIntent(ctx, i.ProbeID, claim.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Status != domain.DeliveryStatusLeased {
		t.Fatalf("row not rolled back after identity fault, got %s", intent.Status)
	}

	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_identity"); err != nil {
		t.Fatal(err)
	}

	// 3. Now finish normally
	if err := s.FinishDelivery(ctx, claim, result); err != nil {
		t.Fatalf("normal finish after faults: %v", err)
	}

	intent, err = s.GetDeliveryIntent(ctx, i.ProbeID, claim.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Status != domain.DeliveryStatusSent {
		t.Fatalf("expected sent status, got %s", intent.Status)
	}
}
