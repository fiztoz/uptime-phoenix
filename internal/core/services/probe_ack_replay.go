package services

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func authorizeAvailabilityReplay(f domain.ProbeReplayAuthorityFacts, e domain.ProbeReplayEvent) string {
	i := e.Incident
	if i == nil || e.Observation != nil || e.Delivery != nil || i.ProbeID != f.ProbeID || !domain.ValidHubID(i.SourceAlertID) || i.TransitionVersion <= 0 || i.Scope != domain.IncidentScopeRegional || i.SubjectKind != domain.IncidentSubjectAvailability || i.EscalationPolicyID != 0 || i.ConditionKind != "" || i.CertificateThreshold != 0 || i.StartedAt.IsZero() || len(i.Reason) > 4096 {
		return "event_invalid"
	}
	switch i.Status {
	case domain.AlertStatusFiring:
		if i.ResolvedAt != nil || hasReplayAcknowledgement(i) {
			return "event_invalid"
		}
	case domain.AlertStatusAcked:
		if i.ResolvedAt != nil || i.AckedAt == nil {
			return "event_invalid"
		}
	case domain.AlertStatusResolved:
		if i.ResolvedAt == nil || i.ResolvedAt.IsZero() {
			return "event_invalid"
		}
	default:
		return "event_invalid"
	}
	prior := f.PriorIncident
	if prior == nil {
		if i.TransitionVersion != 1 || i.Status != domain.AlertStatusFiring {
			return "transition_version_invalid"
		}
		if i.StartedAt.After(e.ObservedAt) {
			return "event_invalid"
		}
		return authorizeReplayAssignment(f, i.MonitorID, i.AssignmentGeneration, i.ConfigRevision, e.ObservedAt)
	}
	if prior.SourceAlertID != i.SourceAlertID || prior.ProbeID != f.ProbeID || prior.MonitorID != i.MonitorID || prior.AssignmentGeneration != i.AssignmentGeneration || prior.SubjectKind != i.SubjectKind || prior.Scope != i.Scope || !prior.StartedAt.Equal(i.StartedAt) || prior.Status == domain.AlertStatusResolved || i.TransitionVersion <= prior.TransitionVersion || i.TransitionVersion-prior.TransitionVersion != 1 {
		return "transition_identity_conflict"
	}
	if i.Status == domain.AlertStatusFiring || prior.Status != domain.AlertStatusFiring && prior.Status != domain.AlertStatusAcked {
		return "transition_identity_conflict"
	}
	if prior.AckedAt != nil {
		// Recovery may follow a backward wall-clock step. Sequence/version order
		// governs lifecycle, while ACK identity is immutable through recovery.
		if i.Status != domain.AlertStatusResolved || !sameReplayAcknowledgement(prior, i) {
			return "acknowledgement_unauthorized"
		}
	} else if i.Status == domain.AlertStatusAcked {
		if !authorizedReplayAcknowledgement(f.IssuedAcknowledgement, i) || !i.AckedAt.Equal(e.ObservedAt.UTC().Truncate(time.Microsecond)) {
			return "acknowledgement_unauthorized"
		}
	} else if hasReplayAcknowledgement(i) {
		return "acknowledgement_unauthorized"
	}
	if i.Status == domain.AlertStatusAcked {
		// An operator may ACK a retained incident after its assignment was moved
		// or deleted. The accepted opening and exact issued command are authority;
		// the source ACK copies the incident's original configuration revision.
		if i.ConfigRevision != prior.ConfigRevision {
			return "config_revision_mismatch"
		}
		return ""
	}
	if !f.MonitorExists || f.MonitorID != i.MonitorID {
		return "monitor_not_found"
	}
	a := f.ConfigAssignment
	if a == nil || !a.Active || a.MonitorID != i.MonitorID || a.Generation != i.AssignmentGeneration || i.ConfigRevision <= 0 || f.ConfigRevision != i.ConfigRevision {
		return "config_revision_mismatch"
	}
	// The accepted opening proves historical membership. Requiring the later
	// source wall clock to remain in that interval would discard valid recovery
	// after a clock correction. Future/horizon checks still apply to each event.
	return ""
}

func authorizedReplayAcknowledgement(c *domain.ProbeAlertAcknowledgement, i *domain.RegionalIncident) bool {
	return c != nil && i.AckedAt != nil && c.CommandID == i.AckCommandID && c.ProbeID == i.ProbeID && c.SourceAlertID == i.SourceAlertID && c.AssignmentGeneration == i.AssignmentGeneration && c.ActorDisplayName == i.AckActorDisplayName && sameAckNote(c.Note, i.AckNote) && !i.AckedAt.Before(c.CreatedAt.Add(-MaxFutureClockSkew)) && i.AckedAt.Before(c.ExpiresAt)
}

func hasReplayAcknowledgement(i *domain.RegionalIncident) bool {
	return i.AckedAt != nil || i.AckCommandID != "" || i.AckActorDisplayName != "" || i.AckNote != nil
}

func sameReplayAcknowledgement(a, b *domain.RegionalIncident) bool {
	return a.AckedAt != nil && b.AckedAt != nil && a.AckedAt.Equal(*b.AckedAt) && a.AckCommandID == b.AckCommandID && a.AckActorDisplayName == b.AckActorDisplayName && sameAckNote(a.AckNote, b.AckNote)
}

func sameAckNote(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
