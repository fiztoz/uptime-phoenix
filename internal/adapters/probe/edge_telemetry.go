package probe

import (
	"encoding/json"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeTelemetryEncoder serializes source availability, watchdog and delivery events.
// Full V1 validators check every persisted event before its transaction commits.
type EdgeTelemetryEncoder struct{}

var _ ports.EdgeTelemetryEncoder = EdgeTelemetryEncoder{}

// EncodeObservation preserves raw/effective retry evidence with explicit DTOs.
// Raw checker condition evidence travels with the observation; freshness is
// already derived from the accepted monitor configuration by the evaluator.
func (EdgeTelemetryEncoder) EncodeObservation(o domain.RegionalObservation) ([]byte, error) {
	status, raw := edgeWireStatus(o.Status), edgeWireStatus(o.RawStatus)
	return encodeEdgeEvent(TelemetryEvent{Seq: Decimal(o.Seq), Kind: "observation", ObservedAt: Timestamp(o.ObservedAt.UTC()), Data: Observation{MonitorID: o.MonitorID, AssignmentGeneration: Decimal(o.AssignmentGeneration), ConfigRevision: Decimal(o.ConfigRevision), Status: status, RawStatus: raw, DownCount: int64(o.DownCount), Ping: int64(o.Ping), DurationMS: int64(o.DurationMS), Message: o.Message, Important: o.Important, Conditions: wireConditionObservations(o.Conditions), TLS: wireTLS(o.TLS)}})
}

// EncodeConditionTransition records one promoted auxiliary transition. It can
// never change primary availability or carry escalation intent.
func (EdgeTelemetryEncoder) EncodeConditionTransition(seq int64, at time.Time, transition domain.ConditionTransition) ([]byte, error) {
	if seq <= 0 || at.IsZero() || transition.MonitorID <= 0 || transition.AssignmentGeneration <= 0 || transition.ConfigRevision <= 0 {
		return nil, domain.ErrValidation
	}
	return encodeEdgeEvent(TelemetryEvent{Seq: Decimal(seq), Kind: "condition.transition", ObservedAt: Timestamp(at.UTC()), Data: wireConditionTransition(transition)})
}

// EncodeIncident records supported source transitions with explicit entity scope.
// A watchdog has no monitor/generation; acknowledgement metadata is preserved.
// Certificate subjects carry their immutable threshold and exact expiry identity.
func (EdgeTelemetryEncoder) EncodeIncident(seq int64, at time.Time, i domain.RegionalIncident) ([]byte, error) {
	if i.EscalationPolicyID != 0 || i.EscalationPolicyVersion != 0 || i.EscalationStatus != "" || i.EscalationNextStep != nil || i.EscalationNextRunAt != nil {
		return nil, domain.ErrValidation
	}
	if at.IsZero() {
		return nil, domain.ErrValidation
	}
	kind := "alert.transition"
	var monitorID *int64
	var generation *Decimal
	subject := IncidentSubject{Kind: i.SubjectKind}
	switch {
	case i.SubjectKind == domain.IncidentSubjectAvailability && i.Scope == domain.IncidentScopeRegional:
		if i.MonitorID <= 0 || i.AssignmentGeneration <= 0 || i.ConditionKind != "" || i.CertificateThreshold != 0 || i.CertificateNotAfter != nil {
			return nil, domain.ErrValidation
		}
		id, gen := i.MonitorID, Decimal(i.AssignmentGeneration)
		monitorID, generation = &id, &gen
	case i.SubjectKind == domain.IncidentSubjectCertificate && i.Scope == domain.IncidentScopeRegional:
		// The subject is the identity: threshold plus exact expiry. An incident that
		// lost either one cannot be re-derived later, so it never reaches the wire.
		if !domain.ValidCertificateIncident(&i) {
			return nil, domain.ErrValidation
		}
		id, gen, threshold := i.MonitorID, Decimal(i.AssignmentGeneration), i.CertificateThreshold
		monitorID, generation, subject.CertificateThreshold = &id, &gen, &threshold
		expiry := Timestamp(i.CertificateNotAfter.UTC())
		subject.CertificateNotAfter = &expiry
	case i.SubjectKind == domain.IncidentSubjectCapacity && i.Scope == domain.IncidentScopeRegional:
		// The subject is the identity: one monitor assignment and one condition
		// kind that survives state changes and recovery.
		if !domain.ValidCapacityIncident(&i) {
			return nil, domain.ErrValidation
		}
		id, gen, conditionKind := i.MonitorID, Decimal(i.AssignmentGeneration), i.ConditionKind
		monitorID, generation, subject.ConditionKind = &id, &gen, &conditionKind
	case i.SubjectKind == domain.IncidentSubjectWatchdog && i.Scope == domain.IncidentScopeProbeConnection:
		if i.MonitorID != 0 || i.AssignmentGeneration != 0 || i.ConditionKind != "" || i.CertificateThreshold != 0 || i.CertificateNotAfter != nil {
			return nil, domain.ErrValidation
		}
		kind = "watchdog.transition"
	default:
		return nil, domain.ErrValidation
	}
	var resolvedAt, ackedAt *Timestamp
	var ack *IncidentAcknowledgement
	if i.ResolvedAt != nil {
		t := Timestamp(i.ResolvedAt.UTC())
		resolvedAt = &t
	}
	if i.AckedAt != nil {
		t := Timestamp(i.AckedAt.UTC())
		ackedAt = &t
		ack = &IncidentAcknowledgement{CommandID: i.AckCommandID, ActorDisplayName: i.AckActorDisplayName, Note: i.AckNote}
	} else if i.AckCommandID != "" || i.AckActorDisplayName != "" || i.AckNote != nil {
		return nil, domain.ErrValidation
	}
	return encodeEdgeEvent(TelemetryEvent{Seq: Decimal(seq), Kind: kind, ObservedAt: Timestamp(at.UTC()), Data: IncidentTransition{SourceAlertID: i.SourceAlertID, Scope: string(i.Scope), MonitorID: monitorID, AssignmentGeneration: generation, Status: i.Status, TransitionVersion: Decimal(i.TransitionVersion), StartedAt: Timestamp(i.StartedAt.UTC()), ResolvedAt: resolvedAt, Reason: i.Reason, ConfigRevision: Decimal(i.ConfigRevision), Subject: subject, AckedAt: ackedAt, Acknowledgement: ack}})
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
