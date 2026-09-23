package services

import (
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// CapacityPagingInput is one trusted assignment's capacity paging context. The
// evaluation runs only at the source that owns the assignment.
type CapacityPagingInput struct {
	Config      *domain.EdgeResolvedConfig
	Monitor     *domain.Monitor
	ProbeID     string
	Assignment  domain.EdgeResolvedAssignment
	Evidence    domain.ConditionEvidence
	Previous    *domain.MonitorCondition
	Prior       *domain.EdgeConditionState
	Incident    *domain.RegionalIncident
	Maintenance bool
	Now         time.Time
	NewID       func() (string, error)
}

// EvaluateCapacityPaging decides the durable capacity paging work for one
// evaluated condition. Paging is level-triggered against the delivered cursor,
// exactly like the local MonitorConditionService: a confirmed warning or error
// the operator has not been told about pages (a maintenance window defers the
// page instead of consuming it), a changed warning/error re-pages under the same
// incident identity, and a confirmed recovery pages once and resolves the open
// incident. Maintenance suppresses the entire lifecycle without advancing the
// cursor or quietly closing an incident that is still factually open. No
// provider I/O happens here.
func EvaluateCapacityPaging(in CapacityPagingInput) (*domain.EdgeConditionAlertWork, error) {
	e := in.Evidence
	if in.Config == nil || in.Monitor == nil || in.NewID == nil {
		return nil, domain.ErrValidation
	}
	// Unconfirmed candidates never page; the local contract pages only after
	// two-sample confirmation.
	if e.EffectiveState == nil {
		return nil, nil
	}
	want := *e.EffectiveState
	delivered := domain.ConditionState("")
	if in.Prior != nil {
		delivered = in.Prior.DeliveredState
	}
	var open *domain.RegionalIncident
	if in.Incident != nil {
		open = in.Incident
	}
	pending := (want == domain.ConditionStateWarning || want == domain.ConditionStateError) && delivered != want
	recovery := want == domain.ConditionStateOK && (delivered == domain.ConditionStateWarning || delivered == domain.ConditionStateError)
	if (!pending && !recovery) || in.Maintenance {
		return nil, nil
	}

	// The rendered snapshot reports the state plus the prior state the operator
	// knows, matching the local condition alert copy. A delayed page repeats its
	// own state on both sides, exactly as the local service renders it.
	previousState := domain.ConditionStateOK
	if in.Previous != nil && in.Previous.State != "" {
		previousState = in.Previous.State
	}
	content := &domain.EdgeConditionAlertContent{
		Kind: e.Kind, State: want, PreviousState: previousState,
		Used: e.Used, Limit: e.Limit, Percent: e.Percent, Threshold: e.Threshold,
		Unit: e.Unit, Resource: e.Resource, Scope: e.Scope, Source: e.Source,
		Message: e.Message, ObservedAt: e.ObservedAt.UTC(),
	}
	if want == domain.ConditionStateOK {
		content.Message = fmt.Sprintf("%s recovered to normal", conditionLabel(e.Resource, e.Kind))
		if e.Percent != nil {
			content.Message = fmt.Sprintf("%s recovered to %.1f%%", conditionLabel(e.Resource, e.Kind), *e.Percent)
		}
	}
	if !domain.ValidEdgeConditionAlertContent(content) {
		return nil, domain.ErrValidation
	}

	now := in.Now.UTC()
	work := &domain.EdgeConditionAlertWork{Content: content, DeliveredState: want}
	switch {
	case pending && open == nil:
		id, err := in.NewID()
		if err != nil {
			return nil, err
		}
		work.Incident = &domain.RegionalIncident{
			SourceAlertID: id, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectCapacity,
			ConditionKind: e.Kind, ProbeID: in.ProbeID, MonitorID: in.Monitor.ID,
			AssignmentGeneration: in.Assignment.Generation, Status: domain.AlertStatusFiring,
			TransitionVersion: 1, StartedAt: now, Reason: content.Message, ConfigRevision: in.Config.Metadata.Revision,
		}
		work.OpenAlertID = id
	case pending && open != nil:
		// A warning/error change keeps the incident identity and restates it.
		if open.Status != domain.AlertStatusFiring || open.TransitionVersion == 0 ||
			open.ConditionKind != e.Kind || open.MonitorID != in.Monitor.ID || open.AssignmentGeneration != in.Assignment.Generation {
			return nil, domain.ErrConflict
		}
		if open.TransitionVersion == int64(^uint64(0)>>1) {
			return nil, domain.ErrConflict
		}
		restated := *open
		restated.TransitionVersion++
		restated.Reason = content.Message
		work.Incident = &restated
		work.OpenAlertID = open.SourceAlertID
	case recovery:
		if open == nil || open.Status != domain.AlertStatusFiring || open.TransitionVersion == 0 ||
			open.ConditionKind != e.Kind || open.MonitorID != in.Monitor.ID || open.AssignmentGeneration != in.Assignment.Generation {
			// A recovery page without its factually open incident is an inconsistent
			// cursor. Fail loudly instead of manufacturing a recovery identity.
			return nil, domain.ErrConflict
		}
		if open.TransitionVersion == int64(^uint64(0)>>1) {
			return nil, domain.ErrConflict
		}
		resolved := *open
		resolved.Status = domain.AlertStatusResolved
		resolved.ResolvedAt = &now
		resolved.TransitionVersion++
		resolved.Reason = content.Message
		resolved.ConfigRevision = in.Config.Metadata.Revision
		work.Incident = &resolved
		work.OpenAlertID = ""
	}

	for _, link := range in.Assignment.NotificationLinks {
		channel, exists := in.Config.Channels[link.NotificationID]
		if !exists || channel.Notification == nil {
			return nil, domain.ErrValidation
		}
		if !channel.Notification.Active {
			continue
		}
		id, err := in.NewID()
		if err != nil {
			return nil, err
		}
		work.Intents = append(work.Intents, domain.DeliveryIntent{
			DeliveryID: id, SourceAlertID: work.Incident.SourceAlertID,
			SourceTransitionVersion: work.Incident.TransitionVersion, ProbeID: in.ProbeID,
			NotificationID: link.NotificationID, NotificationVersion: channel.Version,
			EventKind: domain.DeliveryEventCapacityCondition, AvailableAt: now,
		})
	}
	if domain.ValidCapacityIncident(work.Incident) == false {
		return nil, domain.ErrValidation
	}
	return work, nil
}

// CloseCapacityAlertForRemoval retires an open capacity incident administratively
// when its check is disabled: the incident resolves without any provider work,
// exactly like a certificate cursor retirement, and the current projection's
// omission clears the hub mirror.
func CloseCapacityAlertForRemoval(prior *domain.EdgeConditionState, probeID string, generation, revision int64, at time.Time) *domain.EdgeConditionAlertWork {
	if prior == nil || prior.Alert == nil {
		return nil
	}
	open := prior.Alert
	if open.Status != domain.AlertStatusFiring || open.TransitionVersion <= 0 || open.TransitionVersion == int64(^uint64(0)>>1) {
		// An inconsistent stored cursor must fail the commit loudly instead of
		// leaving an unreachable incident behind.
		return &domain.EdgeConditionAlertWork{}
	}
	now := at.UTC()
	resolved := *open
	resolved.Status = domain.AlertStatusResolved
	resolved.ResolvedAt = &now
	resolved.TransitionVersion++
	resolved.Reason = "capacity check disabled"
	resolved.ConfigRevision = revision
	return &domain.EdgeConditionAlertWork{Incident: &resolved, OpenAlertID: "", DeliveredState: prior.DeliveredState}
}
