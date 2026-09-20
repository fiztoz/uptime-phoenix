package services

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeRecordingService evaluates only the source assignment's evidence. It
// commits observations and provider work together without performing provider I/O.
type EdgeRecordingService struct {
	identity ports.EdgeIdentityRepository
	checks   ports.EdgeCheckRepository
	cron     ports.CronEvaluator
}

// NewEdgeRecordingService wires durable source recording and maintenance rules.
func NewEdgeRecordingService(identity ports.EdgeIdentityRepository, checks ports.EdgeCheckRepository, cron ports.CronEvaluator) *EdgeRecordingService {
	return &EdgeRecordingService{identity: identity, checks: checks, cron: cron}
}

// EdgeMaintenanceActive evaluates only windows named by the accepted assignment.
// Missing cron support fails closed instead of silently omitting suppression.
func EdgeMaintenanceActive(config *domain.EdgeResolvedConfig, assignment domain.EdgeResolvedAssignment, cron ports.CronEvaluator, at time.Time) (bool, error) {
	if config == nil || at.IsZero() {
		return false, domain.ErrValidation
	}
	for _, id := range assignment.MaintenanceIDs {
		w := config.Maintenance[id]
		if w == nil {
			return false, domain.ErrValidation
		}
		if !w.Active {
			continue
		}
		switch w.Strategy {
		case "single":
			if !at.Before(w.StartDate.UTC()) && at.Before(w.EndDate.UTC()) {
				return true, nil
			}
		case "cron":
			if cron == nil {
				return false, domain.ErrValidation
			}
			loc, err := time.LoadLocation(w.Timezone)
			if err != nil {
				return false, domain.ErrValidation
			}
			if cron.IsWindowActive(w.CronExpr, w.Duration, at.UTC(), loc) {
				return true, nil
			}
		default:
			return false, domain.ErrValidation
		}
	}
	return false, nil
}

// Record uses the exact graph captured before the check. Storage fences a config
// replacement and retries only a concurrent source-state update, never authority.
func (s *EdgeRecordingService) Record(ctx context.Context, config *domain.EdgeResolvedConfig, assignment domain.EdgeResolvedAssignment, result ports.CheckResult, at time.Time) (domain.RegionalObservation, error) {
	m := assignment.Monitor
	if config == nil || m == nil || !m.Active || assignment.Generation <= 0 || at.IsZero() || result.Status != domain.StatusUp && result.Status != domain.StatusDown || result.LatencyMs < 0 || result.DurationMs < 0 {
		return domain.RegionalObservation{}, domain.ErrValidation
	}
	i, err := s.identity.ReadIdentity(ctx)
	if err != nil {
		return domain.RegionalObservation{}, err
	}
	if config.Metadata.HubID != i.HubID || config.Metadata.ProbeID != i.ProbeID || config.Metadata.Revision != i.ConfigRevision {
		return domain.RegionalObservation{}, ports.ErrConflict
	}
	maintenance, err := EdgeMaintenanceActive(config, assignment, s.cron, at)
	if err != nil {
		return domain.RegionalObservation{}, err
	}
	at = at.UTC()
	for attempts := 0; attempts < 3; attempts++ {
		before, err := s.checks.ReadEdgeEvidence(ctx, m.ID, assignment.Generation)
		if err != nil {
			return domain.RegionalObservation{}, err
		}
		var previous *domain.RetryState
		var expectedSeq int64
		if before.State != nil {
			previous = &domain.RetryState{Status: before.State.Status, DownCount: before.State.DownCount}
			expectedSeq = before.State.Seq
		}
		eval := EvaluateObservation(previous, result.Status, maintenance, m.MaxRetries)
		o := domain.RegionalObservation{ProbeID: i.ProbeID, StreamID: i.StreamID, MonitorID: m.ID, AssignmentGeneration: assignment.Generation, ConfigRevision: config.Metadata.Revision, Status: eval.State.Status, RawStatus: result.Status, DownCount: eval.State.DownCount, Ping: int(result.LatencyMs), DurationMS: int(result.DurationMs), Message: result.Message, Important: eval.Important, ObservedAt: at, ReceivedAt: at}
		if maintenance {
			o.Message = "Maintenance window active"
		}
		record := domain.EdgeCheckRecord{ExpectedStateSeq: expectedSeq, Observation: o}
		incident := before.Incident
		if incident != nil {
			record.ExpectedIncidentVersion = incident.TransitionVersion
		}
		enqueue := false
		if o.Status == domain.StatusDown {
			if incident == nil || incident.Status == domain.AlertStatusResolved {
				id, err := newUUIDv4()
				if err != nil {
					return domain.RegionalObservation{}, err
				}
				incident = &domain.RegionalIncident{SourceAlertID: id, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectAvailability, ProbeID: i.ProbeID, MonitorID: m.ID, AssignmentGeneration: assignment.Generation, Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: at, Reason: o.Message, ConfigRevision: o.ConfigRevision}
				record.Incident, enqueue = incident, true
			} else if incident.Status == domain.AlertStatusFiring && m.ResendInterval > 0 {
				enqueue = before.LastEnqueuedAt == nil || at.Sub(*before.LastEnqueuedAt) >= time.Duration(m.ResendInterval)*time.Minute
			}
		} else if o.Status == domain.StatusUp && incident != nil && (incident.Status == domain.AlertStatusFiring || incident.Status == domain.AlertStatusAcked) {
			if incident.TransitionVersion == math.MaxInt64 {
				return domain.RegionalObservation{}, ports.ErrConflict
			}
			resolved := *incident
			resolved.Status, resolved.ResolvedAt = domain.AlertStatusResolved, &at
			resolved.TransitionVersion++
			resolved.ConfigRevision, resolved.Reason = o.ConfigRevision, o.Message
			incident, record.Incident, enqueue = &resolved, &resolved, true
		}
		if enqueue {
			for _, link := range assignment.NotificationLinks {
				channel, exists := config.Channels[link.NotificationID]
				if !exists || channel.Notification == nil {
					return domain.RegionalObservation{}, domain.ErrValidation
				}
				if !channel.Notification.Active {
					continue
				}
				id, err := newUUIDv4()
				if err != nil {
					return domain.RegionalObservation{}, err
				}
				record.DeliveryIntents = append(record.DeliveryIntents, domain.DeliveryIntent{DeliveryID: id, SourceAlertID: incident.SourceAlertID, SourceTransitionVersion: incident.TransitionVersion, ProbeID: i.ProbeID, NotificationID: link.NotificationID, NotificationVersion: channel.Version, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at})
			}
		}
		committed, err := s.checks.CommitEdgeCheck(ctx, record)
		if !errors.Is(err, ports.ErrStaleLocalState) {
			return committed, err
		}
	}
	return domain.RegionalObservation{}, ports.ErrStaleLocalState
}
