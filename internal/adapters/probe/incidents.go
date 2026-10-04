package probe

import (
	"encoding/json"
	"errors"
	"strings"
)

// IncidentTransition mirrors one source-owned incident version. Its immutable
// identity and command/policy authority must be checked by transactional ingest.
type IncidentTransition struct {
	SourceAlertID        string                   `json:"source_alert_id"`
	Scope                string                   `json:"scope"`
	MonitorID            *int64                   `json:"monitor_id"`
	AssignmentGeneration *Decimal                 `json:"assignment_generation"`
	Status               string                   `json:"status"`
	TransitionVersion    Decimal                  `json:"transition_version"`
	StartedAt            Timestamp                `json:"started_at"`
	ResolvedAt           *Timestamp               `json:"resolved_at"`
	Reason               string                   `json:"reason"`
	ConfigRevision       Decimal                  `json:"config_revision"`
	Subject              IncidentSubject          `json:"subject"`
	AckedAt              *Timestamp               `json:"acked_at"`
	Acknowledgement      *IncidentAcknowledgement `json:"acknowledgement"`
	Escalation           *IncidentEscalation      `json:"escalation"`
}

// IncidentSubject is the immutable availability, capacity, certificate, or
// watchdog subject. Unrelated subject fields are explicitly null on the wire.
type IncidentSubject struct {
	Kind                 string     `json:"kind"`
	ConditionKind        *string    `json:"condition_kind"`
	CertificateThreshold *int64     `json:"certificate_threshold"`
	CertificateNotAfter  *Timestamp `json:"certificate_not_after"`
}

// IncidentAcknowledgement mirrors an applied command without tokens or hub keys.
type IncidentAcknowledgement struct {
	CommandID        string  `json:"command_id"`
	ActorDisplayName string  `json:"actor_display_name"`
	Note             *string `json:"note"`
}

// IncidentEscalation mirrors source progress without scheduling hub delivery.
// Step zero is dispatcher-owned and never appears as NextStep.
type IncidentEscalation struct {
	PolicyID      int64      `json:"policy_id"`
	PolicyVersion Decimal    `json:"policy_version"`
	Status        string     `json:"status"`
	NextStep      *int64     `json:"next_step"`
	NextRunAt     *Timestamp `json:"next_run_at"`
}

// DeliveryResult describes a source outbox outcome, never a provider send request.
// A zero attempt is valid only for an intent superseded before any provider I/O.
type DeliveryResult struct {
	DeliveryID              string  `json:"delivery_id"`
	SourceAlertID           string  `json:"source_alert_id"`
	SourceTransitionVersion Decimal `json:"source_transition_version"`
	NotificationID          int64   `json:"notification_id"`
	NotificationVersion     Decimal `json:"notification_version"`
	EventKind               string  `json:"event_kind"`
	Attempt                 int64   `json:"attempt"`
	Status                  string  `json:"status"`
	ErrorCode               *string `json:"error_code"`
}

// ConditionTransition carries a promoted auxiliary transition. It cannot change
// primary availability or authorize a regional incident/recovery on its own.
type ConditionTransition struct {
	MonitorID            int64   `json:"monitor_id"`
	AssignmentGeneration Decimal `json:"assignment_generation"`
	ConfigRevision       Decimal `json:"config_revision"`
	Kind                 string  `json:"kind"`
	PreviousState        *string `json:"previous_state"`
	State                string  `json:"state"`
	Message              string  `json:"message"`
	SourceAlertID        *string `json:"source_alert_id"`
}

func (IncidentTransition) isTelemetryData()  {}
func (DeliveryResult) isTelemetryData()      {}
func (ConditionTransition) isTelemetryData() {}

func decodeIncidentTransition(data []byte, eventKind string) (IncidentTransition, error) {
	var incident IncidentTransition
	fields, err := objectFields(data)
	if err != nil {
		return incident, err
	}
	if err := requiredUUID(fields, "source_alert_id", &incident.SourceAlertID); err != nil {
		return incident, err
	}
	if err := decodeRequiredFields(fields,
		field{"scope", &incident.Scope}, field{"status", &incident.Status},
		field{"transition_version", &incident.TransitionVersion}, field{"started_at", &incident.StartedAt},
		field{"reason", &incident.Reason}, field{"config_revision", &incident.ConfigRevision},
	); err != nil {
		return incident, err
	}
	if incident.TransitionVersion <= 0 || incident.ConfigRevision <= 0 || len(incident.Reason) > MaxMessageBytes {
		return incident, errors.New("invalid incident version or reason length")
	}
	if err := nullable(fields, "monitor_id", &incident.MonitorID); err != nil {
		return incident, err
	}
	if err := nullable(fields, "assignment_generation", &incident.AssignmentGeneration); err != nil {
		return incident, err
	}
	if err := nullable(fields, "resolved_at", &incident.ResolvedAt); err != nil {
		return incident, err
	}
	if err := nullable(fields, "acked_at", &incident.AckedAt); err != nil {
		return incident, err
	}
	var subjectRaw json.RawMessage
	if err := required(fields, "subject", &subjectRaw); err != nil {
		return incident, err
	}
	incident.Subject, err = decodeIncidentSubject(subjectRaw)
	if err != nil {
		return incident, err
	}
	if eventKind == "watchdog.transition" {
		if incident.Scope != "probe_connection" || incident.Subject.Kind != "watchdog" || incident.MonitorID != nil || incident.AssignmentGeneration != nil {
			return incident, errors.New("watchdog requires probe_connection scope and null monitor assignment")
		}
	} else if incident.Scope != "regional" || incident.Subject.Kind == "watchdog" ||
		incident.MonitorID == nil || *incident.MonitorID <= 0 || incident.AssignmentGeneration == nil || *incident.AssignmentGeneration <= 0 {
		return incident, errors.New("regional incident requires a positive monitor assignment and monitor subject")
	}
	var ackRaw *json.RawMessage
	if err := nullable(fields, "acknowledgement", &ackRaw); err != nil {
		return incident, err
	}
	if ackRaw != nil {
		ack, err := decodeIncidentAcknowledgement(*ackRaw)
		if err != nil {
			return incident, err
		}
		incident.Acknowledgement = &ack
	}
	if (incident.AckedAt == nil) != (incident.Acknowledgement == nil) {
		return incident, errors.New("incident acknowledgement and acked_at must be present together")
	}
	switch incident.Status {
	case "firing":
		if incident.ResolvedAt != nil || incident.AckedAt != nil {
			return incident, errors.New("firing incident cannot carry acknowledgement or resolution")
		}
	case "acked":
		if incident.AckedAt == nil || incident.ResolvedAt != nil {
			return incident, errors.New("acked incident requires acknowledgement without resolution")
		}
	case "resolved":
		if incident.ResolvedAt == nil {
			return incident, errors.New("resolved incident requires resolved_at")
		}
	default:
		return incident, errors.New("invalid incident status")
	}
	var escalationRaw *json.RawMessage
	if err := nullable(fields, "escalation", &escalationRaw); err != nil {
		return incident, err
	}
	if escalationRaw != nil {
		escalation, err := decodeIncidentEscalation(*escalationRaw)
		if err != nil {
			return incident, err
		}
		if incident.Subject.Kind != "availability" || escalation.PolicyVersion > incident.ConfigRevision ||
			escalation.Status == "pending" && incident.Status != "firing" {
			return incident, errors.New("escalation conflicts with incident subject, lifecycle or config revision")
		}
		incident.Escalation = &escalation
	}
	return incident, nil
}

func decodeIncidentSubject(data []byte) (IncidentSubject, error) {
	var subject IncidentSubject
	fields, err := objectFields(data)
	if err != nil {
		return subject, err
	}
	if err := required(fields, "kind", &subject.Kind); err != nil {
		return subject, err
	}
	if err := nullable(fields, "condition_kind", &subject.ConditionKind); err != nil {
		return subject, err
	}
	if err := nullable(fields, "certificate_threshold", &subject.CertificateThreshold); err != nil {
		return subject, err
	}
	if err := nullable(fields, "certificate_not_after", &subject.CertificateNotAfter); err != nil {
		return subject, err
	}
	switch subject.Kind {
	case "availability", "watchdog":
		if subject.ConditionKind != nil || subject.CertificateThreshold != nil || subject.CertificateNotAfter != nil {
			return subject, errors.New("availability/watchdog subject cannot carry auxiliary identity")
		}
	case "capacity":
		if subject.ConditionKind == nil || !validConditionKind(*subject.ConditionKind) || subject.CertificateThreshold != nil || subject.CertificateNotAfter != nil {
			return subject, errors.New("capacity subject requires only a supported condition kind")
		}
	case "certificate":
		if subject.ConditionKind != nil || subject.CertificateNotAfter == nil || subject.CertificateThreshold == nil {
			return subject, errors.New("certificate subject requires threshold and exact expiry only")
		}
		if value := *subject.CertificateThreshold; value != 30 && value != 14 && value != 7 {
			return subject, errors.New("unsupported certificate threshold")
		}
	default:
		return subject, errors.New("invalid incident subject kind")
	}
	return subject, nil
}

func decodeIncidentAcknowledgement(data []byte) (IncidentAcknowledgement, error) {
	var ack IncidentAcknowledgement
	fields, err := objectFields(data)
	if err != nil {
		return ack, err
	}
	if err := requiredUUID(fields, "command_id", &ack.CommandID); err != nil {
		return ack, err
	}
	if err := required(fields, "actor_display_name", &ack.ActorDisplayName); err != nil {
		return ack, err
	}
	if err := nullable(fields, "note", &ack.Note); err != nil {
		return ack, err
	}
	if strings.TrimSpace(ack.ActorDisplayName) == "" || len(ack.ActorDisplayName) > MaxMetadataBytes || ack.Note != nil && len(*ack.Note) > MaxMessageBytes {
		return ack, errors.New("invalid acknowledgement actor or note length")
	}
	return ack, nil
}

func decodeIncidentEscalation(data []byte) (IncidentEscalation, error) {
	var escalation IncidentEscalation
	fields, err := objectFields(data)
	if err != nil {
		return escalation, err
	}
	if err := decodeRequiredFields(fields,
		field{"policy_id", &escalation.PolicyID}, field{"policy_version", &escalation.PolicyVersion}, field{"status", &escalation.Status},
	); err != nil {
		return escalation, err
	}
	if err := nullable(fields, "next_step", &escalation.NextStep); err != nil {
		return escalation, err
	}
	if err := nullable(fields, "next_run_at", &escalation.NextRunAt); err != nil {
		return escalation, err
	}
	if escalation.PolicyID <= 0 || escalation.PolicyVersion <= 0 {
		return escalation, errors.New("escalation policy identity/version must be positive")
	}
	switch escalation.Status {
	case "pending":
		if escalation.NextStep == nil || *escalation.NextStep <= 0 || escalation.NextRunAt == nil {
			return escalation, errors.New("pending escalation requires positive next_step and next_run_at")
		}
	case "done", "canceled":
		if escalation.NextStep != nil || escalation.NextRunAt != nil {
			return escalation, errors.New("terminal escalation cannot retain pending work")
		}
	default:
		return escalation, errors.New("invalid escalation status")
	}
	return escalation, nil
}

func decodeDeliveryResult(data []byte) (DeliveryResult, error) {
	var delivery DeliveryResult
	fields, err := objectFields(data)
	if err != nil {
		return delivery, err
	}
	if err := requiredUUID(fields, "delivery_id", &delivery.DeliveryID); err != nil {
		return delivery, err
	}
	if err := requiredUUID(fields, "source_alert_id", &delivery.SourceAlertID); err != nil {
		return delivery, err
	}
	if err := decodeRequiredFields(fields,
		field{"source_transition_version", &delivery.SourceTransitionVersion}, field{"notification_id", &delivery.NotificationID},
		field{"notification_version", &delivery.NotificationVersion}, field{"event_kind", &delivery.EventKind},
		field{"attempt", &delivery.Attempt}, field{"status", &delivery.Status},
	); err != nil {
		return delivery, err
	}
	if err := nullable(fields, "error_code", &delivery.ErrorCode); err != nil {
		return delivery, err
	}
	if delivery.SourceTransitionVersion <= 0 || delivery.NotificationID <= 0 || delivery.NotificationVersion <= 0 || delivery.Attempt < 0 {
		return delivery, errors.New("invalid delivery identity, version or attempt")
	}
	switch delivery.EventKind {
	case "status_change", "certificate_expiry", "capacity_condition", "probe_connection", "incident_summary":
	default:
		return delivery, errors.New("invalid delivery event_kind")
	}
	switch delivery.Status {
	case "sent", "superseded":
		if delivery.ErrorCode != nil {
			return delivery, errors.New("sent/superseded delivery requires null error_code")
		}
	case "retrying", "failed":
		if delivery.ErrorCode == nil || !validErrorCode(*delivery.ErrorCode) {
			return delivery, errors.New("failed/retrying delivery requires a redacted machine error code")
		}
	default:
		return delivery, errors.New("invalid delivery status")
	}
	if delivery.Status != "superseded" && delivery.Attempt == 0 {
		return delivery, errors.New("provider outcome requires a positive attempt")
	}
	return delivery, nil
}

func decodeConditionTransition(data []byte) (ConditionTransition, error) {
	var transition ConditionTransition
	fields, err := objectFields(data)
	if err != nil {
		return transition, err
	}
	if err := decodeRequiredFields(fields,
		field{"monitor_id", &transition.MonitorID}, field{"assignment_generation", &transition.AssignmentGeneration},
		field{"config_revision", &transition.ConfigRevision}, field{"kind", &transition.Kind},
		field{"state", &transition.State}, field{"message", &transition.Message},
	); err != nil {
		return transition, err
	}
	if err := nullable(fields, "previous_state", &transition.PreviousState); err != nil {
		return transition, err
	}
	if err := nullableUUID(fields, "source_alert_id", &transition.SourceAlertID); err != nil {
		return transition, err
	}
	if transition.MonitorID <= 0 || transition.AssignmentGeneration <= 0 || transition.ConfigRevision <= 0 || len(transition.Message) > MaxMessageBytes {
		return transition, errors.New("invalid condition transition identity or message length")
	}
	if !validConditionKind(transition.Kind) || !validConditionState(transition.State) ||
		transition.PreviousState != nil && (!validConditionState(*transition.PreviousState) || *transition.PreviousState == transition.State) {
		return transition, errors.New("invalid promoted condition transition")
	}
	return transition, nil
}

func validConditionKind(kind string) bool {
	return kind == "session_pool" || kind == "storage"
}

func nullableUUID(fields map[string]json.RawMessage, name string, target **string) error {
	if err := nullable(fields, name, target); err != nil {
		return err
	}
	if *target != nil {
		return requiredUUID(fields, name, *target)
	}
	return nil
}
