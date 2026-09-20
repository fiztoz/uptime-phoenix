package edge

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestEdgeDeliveryPressureRollsBackSourceRecording(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	r := checkRecord()
	if _, err := s.CommitEdgeCheck(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	// Simulate a full retained provider queue without constructing 60,000 alerts.
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_delivery_outbox SET check_output = CAST(zeroblob(?) AS TEXT)", maxDeliveryQueueBytes); err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.ExpectedStateSeq = 1
	r.Incident = nil
	r.DeliveryIntents[0].DeliveryID = "13eae8a2-7740-4c18-b09b-32cce4f1c4e9"
	if _, err := s.CommitEdgeCheck(t.Context(), r); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full queue reported success: %v", err)
	}
	after, err := s.ReadDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Identity.LastCreatedSeq != before.Identity.LastCreatedSeq || after.QueueBytes != before.QueueBytes || after.PendingDeliveries != 1 {
		t.Fatalf("pressure partly committed: %+v %+v", before, after)
	}
}

func TestEdgeDeliveryLateCounterFailureRollsBackOutcome(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	at := time.Now().UTC()
	r := deliveryCheckRecord(at)
	if _, err := s.CommitEdgeCheck(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	i := testIdentity()
	items, err := s.ClaimDeliveries(t.Context(), i.ProbeID, at, time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	claim := domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: i.ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
	before, err := s.ReadDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Scope the fault to the FINAL counter increment. A broad UPDATE trigger
	// would fire on s.write's initial no-op lock and prove no rollback at all.
	if _, err := s.db.ExecContext(t.Context(), "CREATE TRIGGER fail_final_counter BEFORE UPDATE OF last_created_seq ON edge_identity WHEN NEW.last_created_seq <> OLD.last_created_seq BEGIN SELECT RAISE(ABORT, 'final counter fault'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDelivery(t.Context(), claim, domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: at.Add(time.Second)}); err == nil {
		t.Fatal("counter failure reported success")
	}
	after, err := s.ReadDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.GetDeliveryIntent(t.Context(), i.ProbeID, claim.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Identity.LastCreatedSeq != before.Identity.LastCreatedSeq || after.QueueBytes != before.QueueBytes || item.Status != domain.DeliveryStatusLeased || item.OutcomeAt != nil {
		t.Fatal("late fault partially committed delivery")
	}
}

func TestEdgeDeliveryReservesOutcomeSpaceBeforeProviderIO(t *testing.T) {
	s, _ := setupEdgeDeliveryStore(t)
	at := time.Now().UTC()
	r := deliveryCheckRecord(at)
	if _, err := s.CommitEdgeCheck(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	// Fill retained payload storage up to exactly one maximum event below the
	// limit. Large synthetic events keep this a bounded storage-pressure test.
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_telemetry_outbox SET payload = zeroblob(?)", maxTelemetryEventBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "WITH RECURSIVE n(x) AS (SELECT 1000 UNION ALL SELECT x+1 FROM n WHERE x < 2020) INSERT INTO edge_telemetry_outbox (seq,kind,observed_at,payload) SELECT x,'observation',?,zeroblob(?) FROM n", at.UnixMicro(), maxTelemetryEventBytes); err != nil {
		t.Fatal(err)
	}
	i := testIdentity()
	items, err := s.ClaimDeliveries(t.Context(), i.ProbeID, at, time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("outcome reservation failed: %v", err)
	}
	r.ExpectedStateSeq, r.Incident, r.DeliveryIntents = 1, nil, nil
	if _, err := s.CommitEdgeCheck(t.Context(), r); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("observation consumed reserved outcome bytes: %v", err)
	}
	claim := domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: i.ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
	if err := s.FinishDelivery(t.Context(), claim, domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: at.Add(time.Second)}); err != nil {
		t.Fatalf("reserved provider outcome could not commit: %v", err)
	}
	stored, err := s.GetDeliveryIntent(t.Context(), i.ProbeID, claim.DeliveryID)
	if err != nil || stored.Status != domain.DeliveryStatusSent {
		t.Fatal("reserved outcome lost")
	}
}
