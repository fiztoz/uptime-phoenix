package services

import (
	"context"
	"math"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// DefaultHistoryHorizon bounds the retained-assignment replay authorization window.
const DefaultHistoryHorizon = 7 * 24 * time.Hour

// MaxFutureClockSkew bounds accepted history; future evidence never updates live state.
const MaxFutureClockSkew = 30 * time.Second

var _ ports.ProbeReplayAuthorizer = (*AccessService)(nil)

// AuthorizeEvent evaluates a replay event using facts captured by its write
// transaction. This path performs no I/O, user-cache lookup or provider work.
func (s *AccessService) AuthorizeEvent(_ context.Context, f domain.ProbeReplayAuthorityFacts, e domain.ProbeReplayEvent, now time.Time) (string, bool) {
	reject := func(code string) (string, bool) { return code, false }
	if e.ObservedAt.IsZero() || e.Seq <= 0 {
		return reject("event_invalid")
	}
	if e.ObservedAt.Before(now.Add(-DefaultHistoryHorizon)) {
		return reject("history_horizon_exceeded")
	}
	if e.ObservedAt.After(now.Add(MaxFutureClockSkew)) {
		return reject("future_dated_evidence")
	}
	if f.HubOwnedIncident && (e.Incident != nil || e.Delivery != nil) {
		return reject("source_owner_conflict")
	}
	switch e.Kind {
	case domain.ReplayKindWatchdogTransition:
		if code := authorizeWatchdogReplay(f, e); code != "" {
			return reject(code)
		}
	case domain.ReplayKindObservation:
		o := e.Observation
		if o == nil || e.Incident != nil || e.Delivery != nil || o.ProbeID != f.ProbeID || o.StreamID != f.StreamID || o.Seq != e.Seq || !o.ObservedAt.Equal(e.ObservedAt) {
			return reject("event_invalid")
		}
		if code := authorizeReplayAssignment(f, o.MonitorID, o.AssignmentGeneration, o.ConfigRevision, e.ObservedAt); code != "" {
			return reject(code)
		}
		if !replayStatus(o.Status) || !replayStatus(o.RawStatus) || o.DownCount < 0 || o.DownCount > math.MaxInt32 || o.Ping < 0 || o.Ping > math.MaxInt32 || o.DurationMS < 0 || o.DurationMS > math.MaxInt32 || len(o.Message) > 4096 {
			return reject("event_invalid")
		}
	case domain.ReplayKindAlertTransition:
		if code := authorizeAvailabilityReplay(f, e); code != "" {
			return reject(code)
		}
	case domain.ReplayKindDeliveryResult:
		d := e.Delivery
		if d == nil || e.Incident != nil || e.Observation != nil || d.ProbeID != f.ProbeID || !domain.ValidHubID(d.DeliveryID) || !domain.ValidHubID(d.SourceAlertID) || !d.ObservedAt.Equal(e.ObservedAt) || d.Attempt < 0 {
			return reject("event_invalid")
		}
		parent := f.ParentTransition
		if parent == nil || parent.ProbeID != f.ProbeID || parent.SourceAlertID != d.SourceAlertID || parent.TransitionVersion != d.SourceTransitionVersion {
			return reject("delivery_parent_not_found")
		}
		watchdog := d.EventKind == domain.DeliveryEventProbeConnection
		if watchdog {
			if parent.Scope != domain.IncidentScopeProbeConnection || parent.SubjectKind != domain.IncidentSubjectWatchdog || parent.MonitorID != 0 || parent.AssignmentGeneration != 0 || parent.Status != domain.AlertStatusFiring && parent.Status != domain.AlertStatusResolved {
				return reject("event_invalid")
			}
			if !f.ConfigFound || !f.WatchdogEnabled || f.ConfigRevision != d.NotificationVersion || d.NotificationVersion < parent.ConfigRevision {
				return reject("config_revision_mismatch")
			}
		} else {
			if !f.MonitorExists {
				return reject("monitor_not_found")
			}
			if f.ConfigAssignment == nil || f.ConfigRevision != parent.ConfigRevision || !f.ConfigAssignment.Active || f.ConfigAssignment.MonitorID != parent.MonitorID || f.ConfigAssignment.Generation != parent.AssignmentGeneration {
				return reject("config_revision_mismatch")
			}
		}
		if d.NotificationVersion <= 0 || f.Channels[d.NotificationID] != d.NotificationVersion {
			return reject("channel_unauthorized")
		}
		if !watchdog && (d.EventKind != domain.DeliveryEventStatusChange || d.ObservedAt.Before(parent.StartedAt) || parent.ResolvedAt != nil && d.ObservedAt.Before(*parent.ResolvedAt)) {
			return reject("event_invalid")
		}
		switch d.Status {
		case domain.DeliveryStatusSent, domain.DeliveryStatusRetrying, domain.DeliveryStatusFailed:
			if d.Attempt < 1 {
				return reject("event_invalid")
			}
		case domain.DeliveryStatusSuperseded:
		default:
			return reject("event_invalid")
		}
		if f.DeliveryIntentExists {
			return reject("delivery_identity_conflict")
		}
		if p := f.PriorDelivery; p != nil {
			if p.SourceAlertID != d.SourceAlertID || p.SourceTransitionVersion != d.SourceTransitionVersion || p.ProbeID != d.ProbeID || p.NotificationID != d.NotificationID || p.NotificationVersion != d.NotificationVersion || p.EventKind != d.EventKind || d.Attempt < p.Attempt || !watchdog && d.ObservedAt.Before(p.ObservedAt) {
				return reject("delivery_identity_conflict")
			}
			// Only retrying can progress to another outcome. Same-sequence retries
			// were handled by receipts before authorization, not by rewriting mirrors.
			if p.Status != domain.DeliveryStatusRetrying || d.Attempt == p.Attempt && d.Status != domain.DeliveryStatusSuperseded {
				return reject("delivery_outcome_conflict")
			}
		}
	default:
		return reject("unsupported_event")
	}
	return "", true
}

func authorizeWatchdogReplay(f domain.ProbeReplayAuthorityFacts, e domain.ProbeReplayEvent) string {
	i := e.Incident
	if i == nil || e.Observation != nil || e.Delivery != nil {
		return "event_invalid"
	}
	if !f.ConfigFound || f.ConfigRevision != i.ConfigRevision {
		return "config_revision_mismatch"
	}
	// Disabling the watchdog resolves the original source incident without an
	// UP notification. That terminal history still belongs in the mirror.
	if !f.WatchdogEnabled && (i.Status != domain.AlertStatusResolved || f.PriorIncident == nil) {
		return "watchdog_disabled"
	}
	// An authenticated edge does not establish that an operator issued an ACK.
	// Command correlation is added with the durable command implementation.
	if i.AckedAt != nil || i.AckCommandID != "" || i.AckActorDisplayName != "" || i.AckNote != nil {
		return "acknowledgement_unauthorized"
	}
	if err := domain.ValidateProbeWatchdogTransition(f.ProbeID, f.ConfigRevision, f.PriorIncident, *i); err != nil {
		return "transition_identity_conflict"
	}
	return ""
}

func authorizeReplayAssignment(f domain.ProbeReplayAuthorityFacts, monitorID, generation, revision int64, at time.Time) string {
	if !f.MonitorExists || f.MonitorID != monitorID {
		return "monitor_not_found"
	}
	a := f.ConfigAssignment
	if a == nil || !a.Active || a.MonitorID != monitorID || a.Generation != generation || revision <= 0 || f.ConfigRevision != revision || at.Before(f.ConfigEffectiveAt) {
		return "config_revision_mismatch"
	}
	for _, interval := range f.AssignmentHistory {
		if interval.ProbeID == f.ProbeID && interval.Generation == generation && !at.Before(interval.From) && (interval.To.IsZero() || at.Before(interval.To)) {
			return ""
		}
	}
	return "assignment_unauthorized"
}

func replayStatus(s domain.Status) bool {
	return s == domain.StatusUp || s == domain.StatusDown || s == domain.StatusPending || s == domain.StatusMaintenance
}
