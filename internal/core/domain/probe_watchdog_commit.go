package domain

import (
	"math"
	"time"
	"unicode/utf8"
)

// ValidateProbeWatchdogCommit checks the source lifecycle independently of its
// storage adapter. Repositories additionally fence installation, ownership,
// configuration and state version inside their transaction.
func ValidateProbeWatchdogCommit(probeID string, before ProbeWatchdogState, record ProbeWatchdogRecord) error {
	if !ValidHubID(probeID) || record.ExpectedVersion < 0 || record.ExpectedVersion == math.MaxInt64 || record.ConfigRevision <= 0 || record.At.IsZero() || len(record.DeliveryIntents) > 1000 || record.Checkpoint.LossElapsed < 0 || !record.Checkpoint.Armed && (record.Checkpoint.LossElapsed != 0 || record.Checkpoint.PendingLoss) {
		return ErrValidation
	}
	incident := before.Incident
	if record.Incident != nil {
		if err := validateWatchdogTransition(probeID, record.ConfigRevision, before.Incident, *record.Incident); err != nil {
			return err
		}
		incident = record.Incident
	}
	open := incident != nil && incident.Status != AlertStatusResolved
	switch record.Status {
	case ProbeWatchdogUnarmed:
		if record.Checkpoint.Armed || open {
			return ErrValidation
		}
	case ProbeWatchdogStarting, ProbeWatchdogHealthy, ProbeWatchdogSuspect:
		if !record.Checkpoint.Armed || open || record.Checkpoint.PendingLoss {
			return ErrValidation
		}
	case ProbeWatchdogLost, ProbeWatchdogRecovering:
		if !record.Checkpoint.Armed || !open {
			return ErrValidation
		}
	default:
		return ErrValidation
	}
	if len(record.DeliveryIntents) > 0 && (incident == nil || incident.Status == AlertStatusAcked || record.Status != ProbeWatchdogLost && record.Status != ProbeWatchdogHealthy) {
		return ErrValidation
	}
	// Only a newly committed recovery may create its UP delivery. Repeated
	// healthy checkpoints must not page again; resends belong to firing loss.
	if len(record.DeliveryIntents) > 0 && incident.Status == AlertStatusResolved && record.Incident == nil {
		return ErrValidation
	}
	ids := make(map[string]bool, len(record.DeliveryIntents))
	channels := make(map[int64]bool, len(record.DeliveryIntents))
	for _, intent := range record.DeliveryIntents {
		if !ValidHubID(intent.DeliveryID) || ids[intent.DeliveryID] || channels[intent.NotificationID] || intent.ProbeID != probeID || intent.SourceAlertID != incident.SourceAlertID || intent.SourceTransitionVersion != incident.TransitionVersion || intent.NotificationID <= 0 || intent.NotificationVersion != record.ConfigRevision || intent.EventKind != DeliveryEventProbeConnection || intent.AvailableAt.IsZero() || intent.EscalationPolicyID != 0 || intent.EscalationStep != 0 {
			return ErrValidation
		}
		ids[intent.DeliveryID], channels[intent.NotificationID] = true, true
	}
	return nil
}

func validateWatchdogTransition(probeID string, revision int64, prior *RegionalIncident, inc RegionalIncident) error {
	if !ValidHubID(inc.SourceAlertID) || inc.ProbeID != probeID || inc.MonitorID != 0 || inc.AssignmentGeneration != 0 || inc.Scope != IncidentScopeProbeConnection || inc.SubjectKind != IncidentSubjectWatchdog || inc.ConfigRevision != revision || inc.StartedAt.IsZero() || inc.TransitionVersion <= 0 || len(inc.Reason) > 4096 || !utf8.ValidString(inc.Reason) || inc.ConditionKind != "" || inc.CertificateThreshold != 0 || inc.EscalationPolicyID != 0 || inc.EscalationPolicyVersion != 0 || inc.EscalationStatus != "" || inc.EscalationNextStep != nil || inc.EscalationNextRunAt != nil {
		return ErrValidation
	}
	switch inc.Status {
	case AlertStatusFiring:
		if inc.ResolvedAt != nil || inc.AckedAt != nil {
			return ErrValidation
		}
	case AlertStatusAcked:
		if inc.ResolvedAt != nil || inc.AckedAt == nil {
			return ErrValidation
		}
	case AlertStatusResolved:
		if inc.ResolvedAt == nil || inc.ResolvedAt.IsZero() {
			return ErrValidation
		}
	default:
		return ErrValidation
	}
	if inc.AckedAt == nil {
		if inc.AckCommandID != "" || inc.AckActorDisplayName != "" || inc.AckNote != nil {
			return ErrValidation
		}
	} else if inc.AckedAt.IsZero() || !ValidHubID(inc.AckCommandID) || len(inc.AckActorDisplayName) == 0 || len(inc.AckActorDisplayName) > 256 || !utf8.ValidString(inc.AckActorDisplayName) || inc.AckNote != nil && (len(*inc.AckNote) > 4096 || !utf8.ValidString(*inc.AckNote)) {
		return ErrValidation
	}
	if prior == nil || prior.Status == AlertStatusResolved {
		if inc.Status != AlertStatusFiring || inc.TransitionVersion != 1 || prior != nil && inc.SourceAlertID == prior.SourceAlertID {
			return ErrValidation
		}
		return nil
	}
	if inc.SourceAlertID != prior.SourceAlertID || inc.StartedAt.UTC().UnixMicro() != prior.StartedAt.UTC().UnixMicro() || prior.TransitionVersion == math.MaxInt64 || inc.TransitionVersion != prior.TransitionVersion+1 || inc.Status == AlertStatusFiring {
		return ErrValidation
	}
	if prior.Status == AlertStatusAcked && (inc.Status != AlertStatusResolved || inc.AckCommandID != prior.AckCommandID || inc.AckActorDisplayName != prior.AckActorDisplayName || !watchdogSameTime(inc.AckedAt, prior.AckedAt) || !watchdogSameString(inc.AckNote, prior.AckNote)) {
		return ErrValidation
	}
	// A recovery preserves an existing ACK; it cannot invent one. ACK commands
	// must first commit their own transition, fenced to the exact open incident.
	if prior.Status == AlertStatusFiring && inc.Status == AlertStatusResolved && inc.AckedAt != nil {
		return ErrValidation
	}
	return nil
}

func watchdogSameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.UTC().UnixMicro() == b.UTC().UnixMicro()
}
func watchdogSameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
