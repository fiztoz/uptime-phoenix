package probe

import (
	"encoding/json"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeTelemetryEncoder serializes the M2 availability/delivery source events.
// Full V1 validators check every persisted event before its transaction commits.
type EdgeTelemetryEncoder struct{}

var _ ports.EdgeTelemetryEncoder = EdgeTelemetryEncoder{}

// EncodeObservation preserves raw/effective retry evidence with explicit DTOs.
func (EdgeTelemetryEncoder) EncodeObservation(o domain.RegionalObservation) ([]byte, error) {
	status, raw := edgeWireStatus(o.Status), edgeWireStatus(o.RawStatus)
	return encodeEdgeEvent(TelemetryEvent{Seq: Decimal(o.Seq), Kind: "observation", ObservedAt: Timestamp(o.ObservedAt.UTC()), Data: Observation{MonitorID: o.MonitorID, AssignmentGeneration: Decimal(o.AssignmentGeneration), ConfigRevision: Decimal(o.ConfigRevision), Status: status, RawStatus: raw, DownCount: int64(o.DownCount), Ping: int64(o.Ping), DurationMS: int64(o.DurationMS), Message: o.Message, Important: o.Important, Conditions: []ConditionObservation{}}})
}

// EncodeIncident records only availability transitions supported by this slice.
func (EdgeTelemetryEncoder) EncodeIncident(seq int64, at time.Time, i domain.RegionalIncident) ([]byte, error) {
	if i.SubjectKind != domain.IncidentSubjectAvailability || i.Scope != domain.IncidentScopeRegional || i.AckedAt != nil || i.EscalationPolicyID != 0 {
		return nil, domain.ErrValidation
	}
	monitorID, generation := i.MonitorID, Decimal(i.AssignmentGeneration)
	var resolvedAt *Timestamp
	if i.ResolvedAt != nil {
		t := Timestamp(i.ResolvedAt.UTC())
		resolvedAt = &t
	}
	return encodeEdgeEvent(TelemetryEvent{Seq: Decimal(seq), Kind: "alert.transition", ObservedAt: Timestamp(at.UTC()), Data: IncidentTransition{SourceAlertID: i.SourceAlertID, Scope: string(i.Scope), MonitorID: &monitorID, AssignmentGeneration: &generation, Status: i.Status, TransitionVersion: Decimal(i.TransitionVersion), StartedAt: Timestamp(i.StartedAt.UTC()), ResolvedAt: resolvedAt, Reason: i.Reason, ConfigRevision: Decimal(i.ConfigRevision), Subject: IncidentSubject{Kind: domain.IncidentSubjectAvailability}}})
}

// EncodeDelivery stores a redacted provider outcome under a new stream sequence.
func (EdgeTelemetryEncoder) EncodeDelivery(seq int64, d domain.RegionalDelivery) ([]byte, error) {
	var code *string
	if d.ErrorCode != "" {
		value := d.ErrorCode
		code = &value
	}
	return encodeEdgeEvent(TelemetryEvent{Seq: Decimal(seq), Kind: "delivery.result", ObservedAt: Timestamp(d.ObservedAt.UTC()), Data: DeliveryResult{DeliveryID: d.DeliveryID, SourceAlertID: d.SourceAlertID, SourceTransitionVersion: Decimal(d.SourceTransitionVersion), NotificationID: d.NotificationID, NotificationVersion: Decimal(d.NotificationVersion), EventKind: d.EventKind, Attempt: d.Attempt, Status: d.Status, ErrorCode: code}})
}

func encodeEdgeEvent(event TelemetryEvent) ([]byte, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, domain.ErrValidation
	}
	if _, err := decodeTelemetryEvent(data); err != nil {
		return nil, domain.ErrValidation
	}
	return data, nil
}

func edgeWireStatus(s domain.Status) string {
	switch s {
	case domain.StatusUp:
		return "UP"
	case domain.StatusDown:
		return "DOWN"
	case domain.StatusPending:
		return "PENDING"
	case domain.StatusMaintenance:
		return "MAINTENANCE"
	default:
		return ""
	}
}
