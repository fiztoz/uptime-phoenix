package repository

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestIncidentEscalationRoundTripAndDuplicateSnapshot(t *testing.T) {
	due := time.Date(2026, 9, 25, 12, 5, 0, 0, time.UTC)
	step := int64(1)
	incident := domain.RegionalIncident{
		SourceAlertID: "11111111-1111-4111-8111-111111111111", ProbeID: "probe", Scope: domain.IncidentScopeRegional,
		SubjectKind: domain.IncidentSubjectAvailability, MonitorID: 7, AssignmentGeneration: 2, Status: domain.AlertStatusFiring,
		TransitionVersion: 1, StartedAt: due.Add(-time.Minute), Reason: "down", ConfigRevision: 4,
		EscalationPolicyID: 30, EscalationPolicyVersion: 4, EscalationStatus: domain.EscalationStatePending,
		EscalationNextStep: &step, EscalationNextRunAt: &due,
	}
	if err := validateIncident(&incident); err != nil {
		t.Fatal(err)
	}
	model := incidentModel(incident)
	back := incidentFromModel(&model)
	if back.EscalationPolicyID != 30 || back.EscalationPolicyVersion != 4 || back.EscalationStatus != domain.EscalationStatePending || back.EscalationNextStep == nil || *back.EscalationNextStep != 1 || back.EscalationNextRunAt == nil || !back.EscalationNextRunAt.Equal(due) {
		t.Fatalf("model dropped the ladder: %+v", back)
	}
	if !sameIncidentSnapshot(&model, &model) {
		t.Fatal("duplicate ladder snapshot mismatched")
	}
	other := model
	done := domain.EscalationStateDone
	other.EscalationStatus = &done
	other.EscalationNextStep = nil
	other.EscalationNextRunAt = nil
	if sameIncidentSnapshot(&model, &other) {
		t.Fatal("different ladder progress compared equal")
	}
	incident.Status = domain.AlertStatusResolved
	resolved := due
	incident.ResolvedAt = &resolved
	if err := validateIncident(&incident); err == nil {
		t.Fatal("pending escalation survived resolution")
	}
}
