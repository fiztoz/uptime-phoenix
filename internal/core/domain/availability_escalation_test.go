package domain

import (
	"testing"
	"time"
)

func TestAvailabilityEscalationArmsAdvancesAndSettles(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	policy := &EscalationPolicy{ID: 30, Enabled: true, Steps: []EscalationStep{{StepOrder: 1, WaitMinutes: 5, NotificationIDs: []int64{10}}, {StepOrder: 2, WaitMinutes: 10, NotificationIDs: []int64{11}}}}
	incident := &RegionalIncident{Status: AlertStatusFiring, TransitionVersion: 1, ConfigRevision: 4, SubjectKind: IncidentSubjectAvailability}
	ArmAvailabilityEscalation(incident, &EscalationPolicy{ID: 30, Enabled: false, Steps: policy.Steps}, 4, at)
	if incident.EscalationStatus != "" {
		t.Fatal("disabled policy armed a ladder")
	}
	ArmAvailabilityEscalation(incident, policy, 4, at)
	if incident.EscalationStatus != EscalationStatePending || incident.EscalationPolicyID != 30 || incident.EscalationPolicyVersion != 4 || incident.EscalationNextStep == nil || *incident.EscalationNextStep != 1 || !incident.EscalationNextRunAt.Equal(at.Add(5*time.Minute)) || !ValidIncidentEscalation(incident) {
		t.Fatalf("opening ladder: %+v", incident)
	}
	if step, changed := AdvanceAvailabilityEscalation(incident, policy, 4, at); step != nil || changed {
		t.Fatal("early step consumed the wait")
	}
	step, changed := AdvanceAvailabilityEscalation(incident, policy, 5, at.Add(5*time.Minute))
	if !changed || step == nil || step.StepOrder != 1 || incident.TransitionVersion != 2 || incident.ConfigRevision != 5 || incident.EscalationStatus != EscalationStatePending || *incident.EscalationNextStep != 2 {
		t.Fatalf("first rung: changed=%v step=%+v incident=%+v", changed, step, incident)
	}
	step, changed = AdvanceAvailabilityEscalation(incident, policy, 5, incident.EscalationNextRunAt.UTC())
	if !changed || step == nil || step.StepOrder != 2 || incident.EscalationStatus != EscalationStateDone || incident.EscalationNextStep != nil || incident.TransitionVersion != 3 {
		t.Fatalf("final rung: changed=%v step=%+v incident=%+v", changed, step, incident)
	}
	SettleAvailabilityEscalation(incident)
	if incident.EscalationStatus != EscalationStateDone {
		t.Fatal("finished ladder was reopened")
	}

	pending := &RegionalIncident{Status: AlertStatusFiring, TransitionVersion: 1, ConfigRevision: 4, SubjectKind: IncidentSubjectAvailability, EscalationPolicyID: 30, EscalationPolicyVersion: 4, EscalationStatus: EscalationStatePending, EscalationNextStep: incidentInt(1), EscalationNextRunAt: &at}
	if _, changed := AdvanceAvailabilityEscalation(pending, nil, 4, at); !changed || pending.EscalationStatus != EscalationStateCanceled || pending.TransitionVersion != 2 || pending.EscalationPolicyID != 30 {
		t.Fatalf("removed policy: %+v", pending)
	}
	acked := *pending
	acked.Status = AlertStatusAcked
	SettleAvailabilityEscalation(&acked)
	if acked.EscalationStatus != EscalationStateCanceled {
		t.Fatal("acknowledgement left the ladder pending")
	}
}

func incidentInt(v int64) *int64 { return &v }
