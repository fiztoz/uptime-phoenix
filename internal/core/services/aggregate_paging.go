package services

import (
	"errors"
	"fmt"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ErrInvalidAggregatePaging indicates malformed or non-current decision input.
var ErrInvalidAggregatePaging = errors.New("invalid aggregate paging input")

// EvaluateAggregatePaging proposes one aggregate availability incident transition.
// It performs no I/O and grants no delivery authority. A future durable reconciler
// must atomically revalidate configuration, current evidence and owner fencing while
// persisting the incident and provider intents. Historical replay never transitions
// incidents. Administrative closure is separate from recovery and never reopens in
// the same evaluation; subsequent fresh evidence is required after a policy boundary.
func EvaluateAggregatePaging(input domain.AggregatePagingInput) (domain.AggregatePagingDecision, error) {
	hold := domain.AggregatePagingDecision{Action: domain.AggregatePagingHold}
	mode := input.DeliveryMode
	if mode == "" {
		mode = domain.AlertDeliveryRegional
	}
	validMode := func(mode domain.AlertDelivery) bool {
		return mode == domain.AlertDeliveryRegional || mode == domain.AlertDeliveryAggregate || mode == domain.AlertDeliveryBoth
	}
	validPolicy := func(policy domain.HealthPolicy) bool {
		return policy == domain.HealthPolicyAnyDown || policy == domain.HealthPolicyAllDown
	}
	if input.Now.IsZero() || input.SnapshotAt.IsZero() || input.SnapshotAt.After(input.Now) ||
		(input.Current && !input.SnapshotAt.Equal(input.Now)) || input.AssignmentRevision <= 0 ||
		input.PolicyEffectiveAt.IsZero() || input.PolicyEffectiveAt.After(input.SnapshotAt) ||
		!validMode(mode) || !validPolicy(input.Policy) {
		return hold, ErrInvalidAggregatePaging
	}
	if incident := input.OpenIncident; incident != nil {
		if incident.AssignmentRevision <= 0 || incident.AssignmentRevision > input.AssignmentRevision ||
			!validPolicy(incident.Policy) || !validMode(incident.DeliveryMode) || incident.DeliveryMode == domain.AlertDeliveryRegional ||
			(incident.AssignmentRevision == input.AssignmentRevision && (incident.Policy != input.Policy || incident.DeliveryMode != mode)) {
			return hold, fmt.Errorf("open incident configuration: %w", ErrInvalidAggregatePaging)
		}
	}
	// Keep the display truth table in one place. Excluding old-policy evidence by
	// marking it unknown preserves ANY/ALL behavior without shrinking the quorum.
	evidence := append([]domain.RegionalHealthEvidence(nil), input.Evidence...)
	for i := range evidence {
		if !evidence[i].ObservedAt.After(input.PolicyEffectiveAt) {
			evidence[i].UnknownReason = "before_paging_configuration"
		}
	}
	health, err := EvaluateMonitorHealth(input.SnapshotAt, input.Policy, evidence)
	if err != nil {
		return hold, fmt.Errorf("aggregate health: %w: %w", ErrInvalidAggregatePaging, err)
	}
	hold.Health = health
	if !input.Current {
		return hold, nil
	}
	if incident := input.OpenIncident; incident != nil {
		// Prefer the most specific administrative reason when several fields change.
		switch {
		case incident.DeliveryMode != mode:
			hold.AdministrativeReason = "delivery_mode_changed"
		case incident.Policy != input.Policy:
			hold.AdministrativeReason = "health_policy_changed"
		case incident.AssignmentRevision != input.AssignmentRevision:
			hold.AdministrativeReason = "assignment_revision_changed"
		}
		if hold.AdministrativeReason != "" {
			hold.Action = domain.AggregatePagingAdminClose
			return hold, nil
		}
	}
	if mode == domain.AlertDeliveryRegional {
		return hold, nil
	}
	switch {
	case health.Status == domain.StatusDown && input.OpenIncident == nil:
		hold.Action = domain.AggregatePagingOpen
	case health.Status == domain.StatusUp && input.OpenIncident != nil:
		hold.Action = domain.AggregatePagingRecover
	}
	return hold, nil
}
