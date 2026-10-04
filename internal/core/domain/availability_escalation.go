package domain

import (
	"math"
	"time"
)

// ValidIncidentEscalation reports whether availability escalation progress is
// internally consistent. Every other subject must carry none. An empty ladder
// is valid: the assignment has no runnable policy. Pending progress exists only
// on a firing incident and always names the next unsent step.
func ValidIncidentEscalation(incident *RegionalIncident) bool {
	if incident == nil {
		return false
	}
	empty := incident.EscalationPolicyID == 0 && incident.EscalationPolicyVersion == 0 && incident.EscalationStatus == "" &&
		incident.EscalationNextStep == nil && incident.EscalationNextRunAt == nil
	if incident.SubjectKind != IncidentSubjectAvailability {
		return empty
	}
	if empty {
		return true
	}
	if incident.EscalationPolicyID <= 0 || incident.EscalationPolicyVersion <= 0 || incident.EscalationPolicyVersion > incident.ConfigRevision {
		return false
	}
	switch incident.EscalationStatus {
	case EscalationStatePending:
		return incident.Status == AlertStatusFiring && incident.EscalationNextStep != nil && *incident.EscalationNextStep > 0 &&
			incident.EscalationNextRunAt != nil && !incident.EscalationNextRunAt.IsZero()
	case EscalationStateDone, EscalationStateCanceled:
		return incident.EscalationNextStep == nil && incident.EscalationNextRunAt == nil
	default:
		return false
	}
}

// RunnableEscalationPolicy returns the enabled policy with at least one step.
// A disabled or empty policy is assigned on purpose and must not fall through
// to another policy; callers pass only the already-resolved assignment policy.
func RunnableEscalationPolicy(policy *EscalationPolicy) *EscalationPolicy {
	if policy == nil || !policy.Enabled || len(policy.Steps) == 0 || policy.ID <= 0 || policy.Steps[0].StepOrder <= 0 {
		return nil
	}
	return policy
}

// ArmAvailabilityEscalation starts step 1 when a firing incident opens.
// Step zero stays the direct-notification dispatcher. A non-runnable policy
// leaves the incident without escalation progress.
func ArmAvailabilityEscalation(incident *RegionalIncident, policy *EscalationPolicy, revision int64, at time.Time) {
	policy = RunnableEscalationPolicy(policy)
	if incident == nil || policy == nil || incident.Status != AlertStatusFiring || incident.TransitionVersion != 1 || revision <= 0 || at.IsZero() {
		return
	}
	first := policy.Steps[0]
	step := int64(first.StepOrder)
	due := at.UTC().Add(time.Duration(first.WaitMinutes) * time.Minute)
	incident.EscalationPolicyID = policy.ID
	incident.EscalationPolicyVersion = revision
	incident.EscalationStatus = EscalationStatePending
	incident.EscalationNextStep = &step
	incident.EscalationNextRunAt = &due
}

// SettleAvailabilityEscalation stops a pending ladder. Acknowledgement and
// recovery both cancel; a finished ladder stays done. The caller owns the
// incident status and transition version.
func SettleAvailabilityEscalation(incident *RegionalIncident) {
	if incident == nil || incident.EscalationStatus != EscalationStatePending {
		return
	}
	incident.EscalationStatus = EscalationStateCanceled
	incident.EscalationNextStep = nil
	incident.EscalationNextRunAt = nil
}

// AdvanceAvailabilityEscalation applies one due step, or cancels the ladder when
// the accepted policy is no longer the one that armed it. now must be UTC.
// The returned step is the rung whose channels should be queued; a cancel or a
// shortened ladder returns nil and still reports changed.
func AdvanceAvailabilityEscalation(incident *RegionalIncident, policy *EscalationPolicy, revision int64, now time.Time) (*EscalationStep, bool) {
	if incident == nil || revision <= 0 || now.IsZero() || incident.Status != AlertStatusFiring || incident.EscalationStatus != EscalationStatePending ||
		incident.TransitionVersion <= 0 || incident.TransitionVersion == math.MaxInt64 || incident.EscalationNextStep == nil || incident.EscalationNextRunAt == nil {
		return nil, false
	}
	now = now.UTC()
	if now.Before(incident.EscalationNextRunAt.UTC()) {
		return nil, false
	}
	incident.TransitionVersion++
	incident.ConfigRevision = revision
	incident.EscalationPolicyVersion = revision
	if policy == nil || !policy.Enabled || policy.ID != incident.EscalationPolicyID {
		SettleAvailabilityEscalation(incident)
		return nil, true
	}
	current, next := splitAvailabilityEscalation(policy.Steps, int(*incident.EscalationNextStep))
	if current == nil {
		incident.EscalationStatus = EscalationStateDone
		incident.EscalationNextStep = nil
		incident.EscalationNextRunAt = nil
		return nil, true
	}
	if next == nil {
		incident.EscalationStatus = EscalationStateDone
		incident.EscalationNextStep = nil
		incident.EscalationNextRunAt = nil
		return current, true
	}
	step := int64(next.StepOrder)
	due := now.Add(time.Duration(next.WaitMinutes) * time.Minute)
	incident.EscalationStatus = EscalationStatePending
	incident.EscalationNextStep = &step
	incident.EscalationNextRunAt = &due
	return current, true
}

func splitAvailabilityEscalation(steps []EscalationStep, order int) (current, next *EscalationStep) {
	for i := range steps {
		if steps[i].StepOrder != order {
			continue
		}
		current = &steps[i]
		if i+1 < len(steps) {
			next = &steps[i+1]
		}
		return current, next
	}
	return nil, nil
}
