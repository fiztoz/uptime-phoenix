package edge

import (
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestEdgeEscalationSurvivesRestartAndRefusesDowngrade(t *testing.T) {
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
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
	reopened, err := Open(t.Context(), dir, testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.telemetry = probe.EdgeTelemetryEncoder{}
	due, err := reopened.ListDueEscalations(t.Context(), at, 1)
	if err != nil || len(due) != 1 || due[0].EscalationStatus != domain.EscalationStatePending || due[0].TransitionVersion != 1 {
		t.Fatalf("restart lost the ladder: %+v %v", due, err)
	}
	policy := &domain.EscalationPolicy{ID: 30, Enabled: true, Steps: []domain.EscalationStep{{StepOrder: 1, WaitMinutes: 5, NotificationIDs: []int64{10}}}}
	current := due[0]
	sent, changed := domain.AdvanceAvailabilityEscalation(&current, policy, 1, at)
	if !changed || sent == nil || current.EscalationStatus != domain.EscalationStateDone || current.TransitionVersion != 2 {
		t.Fatalf("advance: changed=%v sent=%+v incident=%+v", changed, sent, current)
	}
	intent := domain.DeliveryIntent{DeliveryID: "11111111-1111-4111-8111-111111111111", SourceAlertID: current.SourceAlertID, SourceTransitionVersion: 2, ProbeID: current.ProbeID, NotificationID: 10, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at, EscalationPolicyID: 30, EscalationStep: 1}
	if err := reopened.CommitEscalationAdvance(t.Context(), 1, current, []domain.DeliveryIntent{intent}, "ESCALATION step 1 (policy 30): api is still DOWN and unacknowledged", at); err != nil {
		t.Fatal(err)
	}
	evidence, err := reopened.ReadEdgeEvidence(t.Context(), 17, 1)
	if err != nil || evidence.Incident == nil || evidence.Incident.EscalationStatus != domain.EscalationStateDone || evidence.Incident.TransitionVersion != 2 {
		t.Fatalf("stored advance: %+v %v", evidence.Incident, err)
	}
	queued, err := reopened.GetDeliveryIntent(t.Context(), current.ProbeID, intent.DeliveryID)
	if err != nil || queued.EscalationStep != 1 || queued.EscalationPolicyID != 30 || queued.CheckOutput == "" {
		t.Fatalf("step intent: %+v %v", queued, err)
	}
	if err := runEscalationMigration(t, reopened, "down"); err == nil {
		t.Fatal("downgrade discarded ladder progress")
	}
	if err := reopened.CommitEscalationAdvance(t.Context(), 1, current, nil, "", at); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatalf("stale advance: %v", err)
	}
}
