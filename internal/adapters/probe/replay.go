package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// decodeReplayBatch maps validated wire DTOs to pure core evidence. Unsupported
// V1 semantics retain their sequence but carry no authorized event body, producing
// a durable rejection rather than silently dropping auxiliary evidence.
func decodeReplayBatch(data []byte, probeID string) (domain.ProbeReplayBatch, error) {
	envelope, wire, err := DecodeTelemetryBatch(data)
	if err != nil {
		return domain.ProbeReplayBatch{}, err
	}
	var raw struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(envelope.Payload, &raw); err != nil {
		return domain.ProbeReplayBatch{}, err
	}
	batch := domain.ProbeReplayBatch{ProbeID: probeID, StreamID: wire.StreamID, FirstSeq: int64(wire.FirstSeq), LastSeq: int64(wire.LastSeq), Events: make([]domain.ProbeReplayEvent, 0, len(wire.Events))}
	for index, event := range wire.Events {
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw.Events[index]); err != nil {
			return domain.ProbeReplayBatch{}, err
		}
		digest := sha256.Sum256(compact.Bytes())
		e := domain.ProbeReplayEvent{Seq: int64(event.Seq), Digest: hex.EncodeToString(digest[:]), Kind: event.Kind, ObservedAt: time.Time(event.ObservedAt).UTC()}
		switch value := event.Data.(type) {
		case Observation:
			if len(value.Conditions) == 0 && value.TLS == nil {
				e.Observation = &domain.RegionalObservation{MonitorID: value.MonitorID, ProbeID: probeID, AssignmentGeneration: int64(value.AssignmentGeneration), StreamID: wire.StreamID, Seq: e.Seq, ConfigRevision: int64(value.ConfigRevision), Status: replayDomainStatus(value.Status), RawStatus: replayDomainStatus(value.RawStatus), DownCount: int(value.DownCount), Ping: int(value.Ping), DurationMS: int(value.DurationMS), Message: value.Message, Important: value.Important, ObservedAt: e.ObservedAt}
			}
		case IncidentTransition:
			if event.Kind == domain.ReplayKindAlertTransition && value.Subject.Kind == domain.IncidentSubjectAvailability && value.AckedAt == nil && value.Acknowledgement == nil && value.Escalation == nil && value.MonitorID != nil && value.AssignmentGeneration != nil {
				i := &domain.RegionalIncident{SourceAlertID: value.SourceAlertID, Scope: domain.IncidentScope(value.Scope), MonitorID: *value.MonitorID, ProbeID: probeID, AssignmentGeneration: int64(*value.AssignmentGeneration), Status: value.Status, TransitionVersion: int64(value.TransitionVersion), StartedAt: time.Time(value.StartedAt).UTC(), Reason: value.Reason, ConfigRevision: int64(value.ConfigRevision), SubjectKind: value.Subject.Kind}
				if value.ResolvedAt != nil {
					at := time.Time(*value.ResolvedAt).UTC()
					i.ResolvedAt = &at
				}
				e.Incident = i
			}
		case DeliveryResult:
			d := &domain.RegionalDelivery{DeliveryID: value.DeliveryID, SourceAlertID: value.SourceAlertID, SourceTransitionVersion: int64(value.SourceTransitionVersion), ProbeID: probeID, NotificationID: value.NotificationID, NotificationVersion: int64(value.NotificationVersion), EventKind: value.EventKind, Attempt: value.Attempt, Status: value.Status, ObservedAt: e.ObservedAt}
			if value.ErrorCode != nil {
				d.ErrorCode = *value.ErrorCode
			}
			e.Delivery = d
		}
		batch.Events = append(batch.Events, e)
	}
	return batch, nil
}

func replayDomainStatus(status string) domain.Status {
	switch status {
	case "UP":
		return domain.StatusUp
	case "DOWN":
		return domain.StatusDown
	case "PENDING":
		return domain.StatusPending
	case "MAINTENANCE":
		return domain.StatusMaintenance
	default:
		return domain.Status(-1)
	}
}

func replayACK(result *domain.ProbeReplayResult) TelemetryACK {
	ack := TelemetryACK{StreamID: result.StreamID, CommittedSeq: Decimal(result.CommittedSeq), AcceptedCount: result.AcceptedCount, DuplicateCount: result.DuplicateCount, Rejected: make([]RejectedSequence, 0, len(result.Rejected))}
	for _, r := range result.Rejected {
		ack.Rejected = append(ack.Rejected, RejectedSequence{Seq: Decimal(r.Seq), Code: r.Code})
	}
	return ack
}
