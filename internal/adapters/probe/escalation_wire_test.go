package probe

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestAvailabilityEscalationRoundTripsThroughReplay(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	due := at.Add(5 * time.Minute)
	step := int64(1)
	incident := domain.RegionalIncident{
		SourceAlertID: uuid.NewString(), ProbeID: "22222222-2222-4222-8222-222222222222", Scope: domain.IncidentScopeRegional,
		SubjectKind: domain.IncidentSubjectAvailability, MonitorID: 42, AssignmentGeneration: 3, Status: domain.AlertStatusFiring,
		TransitionVersion: 1, StartedAt: at, Reason: "down", ConfigRevision: 4,
		EscalationPolicyID: 30, EscalationPolicyVersion: 4, EscalationStatus: domain.EscalationStatePending,
		EscalationNextStep: &step, EscalationNextRunAt: &due,
	}
	payload, err := (EdgeTelemetryEncoder{}).EncodeIncident(2, at, incident)
	if err != nil {
		t.Fatal(err)
	}
	batch := &domain.EdgeReplayBatch{StreamID: uuid.NewString(), FirstSeq: 2, LastSeq: 2, Items: []domain.EdgeReplayItem{{Seq: 2, Kind: "alert.transition", ObservedAt: at, Payload: payload}}}
	frame, err := buildReplayBatchFrame(batch.StreamID, 2, batch)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeReplayBatch(frame, incident.ProbeID)
	if err != nil || len(decoded.Events) != 1 || decoded.Events[0].Incident == nil {
		t.Fatalf("escalation transition unmapped: %+v %v", decoded, err)
	}
	got := decoded.Events[0].Incident
	if got.EscalationStatus != domain.EscalationStatePending || got.EscalationPolicyID != 30 || got.EscalationPolicyVersion != 4 || got.EscalationNextStep == nil || *got.EscalationNextStep != 1 || got.EscalationNextRunAt == nil || !got.EscalationNextRunAt.Equal(due) {
		t.Fatalf("mapped ladder: %+v", got)
	}
}
