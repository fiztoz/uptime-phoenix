package services

import (
	"errors"
	"sort"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ErrInvalidDeliveryCutover indicates malformed or incoherent planner input.
var ErrInvalidDeliveryCutover = errors.New("invalid delivery cutover input")

// EvaluateDeliveryCutover checks completeness of an unwired cutover proposal.
// It performs no I/O and cannot authorize aggregate paging or regional suppression.
// Config acknowledgements and today's decoder capability never replace separate
// mode-application evidence. A future barrier must atomically revalidate all
// identity/revision/ownership facts; observational reads and this result are not
// fences. Inputs and document/application evidence are never modified.
func EvaluateDeliveryCutover(input domain.DeliveryCutoverInput) (domain.DeliveryCutoverDecision, error) {
	pending := domain.DeliveryCutoverDecision{State: domain.DeliveryCutoverPending}
	set := input.Assignments
	mode, validMode := domain.CanonicalAlertDelivery(set.AlertDelivery)
	if !validMode || set.MonitorID <= 0 || set.Revision <= 0 || !domain.ValidHubID(input.HubID) ||
		(set.HealthPolicy != domain.HealthPolicyAnyDown && set.HealthPolicy != domain.HealthPolicyAllDown) ||
		input.Now.IsZero() || input.SnapshotAt.IsZero() || input.SnapshotAt.After(input.Now) ||
		(input.Current && !input.SnapshotAt.Equal(input.Now)) {
		return pending, ErrInvalidDeliveryCutover
	}
	members := make(map[string]domain.ProbeAssignment, len(set.Assignments))
	for _, a := range set.Assignments {
		if a.MonitorID != set.MonitorID || a.Generation <= 0 ||
			!domain.ValidProbeConfigTarget(domain.ProbeConfigTarget{HubID: input.HubID, ProbeID: a.ProbeID}) {
			return pending, ErrInvalidDeliveryCutover
		}
		if _, duplicate := members[a.ProbeID]; duplicate {
			return pending, ErrInvalidDeliveryCutover
		}
		members[a.ProbeID] = a
	}
	sources := make(map[string]domain.DeliveryCutoverSource, len(input.Sources))
	for _, source := range input.Sources {
		if _, exists := members[source.ProbeID]; !exists {
			return pending, ErrInvalidDeliveryCutover
		}
		if _, duplicate := sources[source.ProbeID]; duplicate {
			return pending, ErrInvalidDeliveryCutover
		}
		sources[source.ProbeID] = source
	}
	decision := domain.DeliveryCutoverDecision{State: domain.DeliveryCutoverEvidenceComplete, DesiredMode: mode,
		Sources: make([]domain.DeliveryCutoverSourceDecision, 0, len(members))}
	switch {
	case mode == domain.AlertDeliveryRegional:
		decision.State = domain.DeliveryCutoverRegional
	case !input.Current:
		decision.State, decision.Reason = domain.DeliveryCutoverPending, domain.DeliveryCutoverHistoricalSnapshot
	case len(members) == 0:
		decision.State, decision.Reason = domain.DeliveryCutoverPending, domain.DeliveryCutoverNoAssignments
	}
	for probeID, member := range members {
		item := domain.DeliveryCutoverSourceDecision{ProbeID: probeID, State: decision.State, Reason: decision.Reason}
		if mode != domain.AlertDeliveryRegional && input.Current {
			source, exists := sources[probeID]
			item.Reason = deliveryCutoverSourceReason(input, member, source, exists, mode)
			item.State = domain.DeliveryCutoverEvidenceComplete
			if item.Reason != "" {
				item.State, decision.State = domain.DeliveryCutoverPending, domain.DeliveryCutoverPending
			}
		}
		decision.Sources = append(decision.Sources, item)
	}
	sort.Slice(decision.Sources, func(i, j int) bool { return decision.Sources[i].ProbeID < decision.Sources[j].ProbeID })
	return decision, nil
}

func deliveryCutoverSourceReason(input domain.DeliveryCutoverInput, member domain.ProbeAssignment, source domain.DeliveryCutoverSource, exists bool, mode domain.AlertDelivery) domain.DeliveryCutoverReason {
	switch {
	case !exists:
		return domain.DeliveryCutoverSourceUnreported
	case !source.ModeApplicationSupported:
		return domain.DeliveryCutoverApplicationUnsupported
	case source.AcknowledgedConfig == nil:
		return domain.DeliveryCutoverConfigUnreported
	}
	target := domain.ProbeConfigTarget{HubID: input.HubID, ProbeID: member.ProbeID}
	config := source.AcknowledgedConfig
	if !domain.ValidProbeActiveConfig(*config) || config.ProbeConfigTarget != target || config.AssignmentCount < 1 || config.AppliedAt.After(input.Now) {
		return domain.DeliveryCutoverConfigUnverified
	}
	assignment := source.Assignment
	if assignment == nil || assignment.MonitorID != member.MonitorID || assignment.Generation != member.Generation {
		return domain.DeliveryCutoverAssignmentMismatch
	}
	if assignment.AssignmentRevision != input.Assignments.Revision {
		return domain.DeliveryCutoverAssignmentRevisionMismatch
	}
	documentMode, valid := domain.CanonicalAlertDelivery(assignment.AlertDelivery)
	if !valid || documentMode != mode {
		return domain.DeliveryCutoverDocumentModeMismatch
	}
	application := source.ModeApplication
	if application == nil {
		return domain.DeliveryCutoverApplicationUnreported
	}
	if !domain.ValidProbeDeliveryModeApplication(*application) || application.ProbeConfigTarget != target ||
		application.MonitorID != member.MonitorID || application.AssignmentRevision != input.Assignments.Revision ||
		application.Generation != member.Generation || application.ConfigRevision != config.Revision ||
		application.SHA256 != config.SHA256 || application.StreamID != source.StreamID || application.AlertDelivery != mode {
		return domain.DeliveryCutoverApplicationMismatch
	}
	return ""
}
