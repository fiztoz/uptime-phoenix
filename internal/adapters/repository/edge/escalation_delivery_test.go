package edge

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestEdgeEscalationPreservesEarlierDelivery(t *testing.T) {
	s, _ := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	config := protectedConfig(t, 1)
	if err := s.ActivateConfig(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	rec := checkRecord()
	at := rec.Observation.ObservedAt
	step := int64(1)
	rec.Incident.EscalationPolicyID = 30
	rec.Incident.EscalationPolicyVersion = 1
	rec.Incident.EscalationStatus = domain.EscalationStatePending
	rec.Incident.EscalationNextStep = &step
	rec.Incident.EscalationNextRunAt = &at
	if _, err := s.CommitEdgeCheck(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	items, err := s.ClaimDeliveries(t.Context(), rec.Incident.ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %v", items, err)
	}
	claim := domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: items[0].ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
	before, err := s.AuthorizeEdgeDelivery(t.Context(), claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || before == nil {
		t.Fatalf("before advance: %v %v", before, err)
	}
	policy := &domain.EscalationPolicy{ID: 30, Enabled: true, Steps: []domain.EscalationStep{{StepOrder: 1, NotificationIDs: []int64{10}}, {StepOrder: 2, NotificationIDs: []int64{10}}}}
	current := *rec.Incident
	if _, changed := domain.AdvanceAvailabilityEscalation(&current, policy, 1, at); !changed {
		t.Fatal("not advanced")
	}
	intent := domain.DeliveryIntent{DeliveryID: "11111111-1111-4111-8111-111111111111", SourceAlertID: current.SourceAlertID, SourceTransitionVersion: 2, ProbeID: current.ProbeID, NotificationID: 10, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at, EscalationPolicyID: 30, EscalationStep: 1}
	if err := s.CommitEscalationAdvance(t.Context(), 1, current, []domain.DeliveryIntent{intent}, "step one", at); err != nil {
		t.Fatal(err)
	}
	items, err = s.ClaimDeliveries(t.Context(), current.ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim first rung: %+v %v", items, err)
	}
	rungClaim := domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: items[0].ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
	if _, changed := domain.AdvanceAvailabilityEscalation(&current, policy, 1, at); !changed {
		t.Fatal("second rung did not advance")
	}
	if err := s.CommitEscalationAdvance(t.Context(), 2, current, nil, "", at); err != nil {
		t.Fatal(err)
	}
	after, err := s.AuthorizeEdgeDelivery(t.Context(), claim, config.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if after == nil {
		t.Fatal("earlier DOWN delivery rejected although same incident is still firing and unacknowledged")
	}
	if authorized, err := s.AuthorizeEdgeDelivery(t.Context(), rungClaim, config.Snapshot.ProbeConfigMetadata, 10*time.Second); err != nil || authorized == nil {
		t.Fatalf("earlier rung superseded: %+v %v", authorized, err)
	}
	// Recovery must still revoke both older firing versions at the final
	// database boundary, even though escalation alone no longer revokes them.
	up := checkRecord()
	up.ExpectedStateSeq, up.ExpectedIncidentVersion = 1, current.TransitionVersion
	up.Observation.Status, up.Observation.RawStatus, up.Observation.DownCount = domain.StatusUp, domain.StatusUp, 0
	current.TransitionVersion++
	current.Status, current.ResolvedAt = domain.AlertStatusResolved, &up.Observation.ObservedAt
	up.Incident, up.DeliveryIntents = &current, nil
	if _, err := s.CommitEdgeCheck(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	for _, old := range []domain.DeliveryClaim{claim, rungClaim} {
		if authorized, err := s.AuthorizeEdgeDelivery(t.Context(), old, config.Snapshot.ProbeConfigMetadata, 10*time.Second); err != nil || authorized != nil {
			t.Fatalf("recovery failed to suppress old DOWN work: %+v %v", authorized, err)
		}
	}
}
