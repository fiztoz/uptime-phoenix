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
		o.TLS = edgeTLSObservation(result.Metadata, at)
		// Auxiliary capacity conditions are evaluated at the source: raw evidence
		// rides the observation, promotion is two-sample confirmed and fenced like
		// every other source-owned state, and a disabled check retires its row.
		priors := ConditionStatePriors(before.Conditions)
		versions := ConditionStateVersions(before.Conditions)
		storedStates := make(map[string]*domain.EdgeConditionState, len(before.Conditions))
		for index := range before.Conditions {
			storedStates[before.Conditions[index].Kind] = &before.Conditions[index]
		}
		conditionWorks := make([]domain.EdgeConditionWork, 0, len(result.Conditions)+len(before.Conditions))
		for _, raw := range result.Conditions {
			evaluation := EvaluateCondition(priors[raw.Kind], raw, m.ID, m.Interval, at)
			if !domain.ValidConditionEvidence(&evaluation.State) {
				return domain.RegionalObservation{}, domain.ErrValidation
			}
			prior := storedStates[raw.Kind]
			var open *domain.RegionalIncident
			if prior != nil {
				open = prior.Alert
			}
			alert, err := EvaluateCapacityPaging(CapacityPagingInput{
				Config: config, Monitor: m, ProbeID: i.ProbeID, Assignment: assignment,
				Evidence: evaluation.State, Previous: priors[raw.Kind], Prior: prior,
				Incident: open, Maintenance: maintenance, Now: at, NewID: newUUIDv4,
			})
			if err != nil {
				return domain.RegionalObservation{}, err
			}
			work := domain.EdgeConditionWork{State: evaluation.State, ExpectedVersion: versions[raw.Kind], Alert: alert}
			if evaluation.Transition != nil {
				transition := *evaluation.Transition
				transition.AssignmentGeneration = assignment.Generation
				transition.ConfigRevision = config.Metadata.Revision
				openID := ""
				if alert != nil {
					openID = alert.OpenAlertID
				} else if prior != nil {
					openID = prior.AlertSourceID
				}
				if openID != "" {
					incident := openID
					transition.SourceAlertID = &incident
				}
				work.Transition = &transition
			}
			conditionWorks = append(conditionWorks, work)
			o.Conditions = append(o.Conditions, evaluation.State.ConditionObservation)
		}
		for _, stored := range before.Conditions {
			if !ConditionKindEnabled(m.Config, stored.Kind) {
				retired := domain.EdgeConditionState{ConditionEvidence: stored.ConditionEvidence, AlertSourceID: stored.AlertSourceID, DeliveredState: stored.DeliveredState, Alert: stored.Alert, Version: stored.Version}
				conditionWorks = append(conditionWorks, domain.EdgeConditionWork{
					State:           domain.ConditionEvidence{ConditionObservation: domain.ConditionObservation{Kind: stored.Kind}},
					ExpectedVersion: stored.Version, Remove: true,
					Alert: CloseCapacityAlertForRemoval(&retired, i.ProbeID, assignment.Generation, config.Metadata.Revision, at),
				})
			}
		}
		if maintenance {
			o.Message = "Maintenance window active"
		}
		record := domain.EdgeCheckRecord{ExpectedStateSeq: expectedSeq, Observation: o, Conditions: conditionWorks}
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
				// Step zero is the direct links queued below. A runnable policy
				// only schedules its first real step; a disabled policy stays
				// on the assignment and does not fall through.
				var policy *domain.EscalationPolicy
				if assignment.EscalationPolicyID != nil {
					policy = config.Policies[*assignment.EscalationPolicyID]
				}
				domain.ArmAvailabilityEscalation(incident, policy, o.ConfigRevision, at)
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
			domain.SettleAvailabilityEscalation(&resolved)
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
		if before.Certificate != nil {
			record.ExpectedCertificateVersion = before.Certificate.Version
		}
		// Certificate paging is independent of availability lifecycle: a healthy
		// monitor still has to page its expiring certificate, and a down monitor
		// still has to retire a renewed one. Maintenance suppression is decided
		// inside the evaluator from the same window set the check used.
		certificate, err := EvaluateCertAlertPaging(CertAlertPagingInput{Config: config, Monitor: m, Assignment: assignment, Observation: o, Prior: before.Certificate, PriorCursor: before.CertificateIncident, Maintenance: maintenance, Now: at, NewID: newUUIDv4})
		if err != nil {
			return domain.RegionalObservation{}, err
		}
		record.Certificate = certificate
		committed, err := s.checks.CommitEdgeCheck(ctx, record)
		if !errors.Is(err, ports.ErrStaleLocalState) {
			return committed, err
		}
	}
	return domain.RegionalObservation{}, ports.ErrStaleLocalState
}
