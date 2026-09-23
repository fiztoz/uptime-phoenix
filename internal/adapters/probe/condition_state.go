package probe

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// wireConditionMeasurements maps the explicit measurement whitelist shared by
// raw observations and evaluated snapshots.
func wireConditionMeasurements(source domain.ConditionObservation) ConditionMeasurements {
	return ConditionMeasurements{
		Kind: source.Kind, Message: source.Message,
		Used: source.Used, Limit: source.Limit, Percent: source.Percent, Threshold: source.Threshold,
		Unit: source.Unit, Resource: source.Resource, Scope: source.Scope, Source: source.Source,
		ObservedAt: Timestamp(source.ObservedAt.UTC()), StaleAfter: Timestamp(source.StaleAfter.UTC()),
	}
}

func sourceConditionMeasurements(wire ConditionMeasurements) domain.ConditionObservation {
	return domain.ConditionObservation{
		Kind: wire.Kind, Message: wire.Message,
		Used: wire.Used, Limit: wire.Limit, Percent: wire.Percent, Threshold: wire.Threshold,
		Unit: wire.Unit, Resource: wire.Resource, Scope: wire.Scope, Source: wire.Source,
		ObservedAt: time.Time(wire.ObservedAt).UTC(), StaleAfter: time.Time(wire.StaleAfter).UTC(),
	}
}

// wireConditionObservation preserves the raw checker state of one measurement.
func wireConditionObservation(source domain.ConditionObservation) ConditionObservation {
	return ConditionObservation{ConditionMeasurements: wireConditionMeasurements(source), State: string(source.State)}
}

func sourceConditionObservation(wire ConditionObservation) domain.ConditionObservation {
	observation := sourceConditionMeasurements(wire.ConditionMeasurements)
	observation.State = domain.ConditionState(wire.State)
	return observation
}

// wireConditionObservations maps raw checker evidence for observation events.
// A nil source serializes as an explicit empty list, never a missing field.
func wireConditionObservations(source []domain.ConditionObservation) []ConditionObservation {
	out := make([]ConditionObservation, 0, len(source))
	for _, condition := range source {
		out = append(out, wireConditionObservation(condition))
	}
	return out
}

func sourceConditionObservations(wire []ConditionObservation) []domain.ConditionObservation {
	out := make([]domain.ConditionObservation, 0, len(wire))
	for _, condition := range wire {
		out = append(out, sourceConditionObservation(condition))
	}
	return out
}

// wireConditionEvidence maps one evaluated source condition: raw measurement,
// unconfirmed or promoted state and the pending transition candidate.
func wireConditionEvidence(source domain.ConditionEvidence) ConditionState {
	effective := (*string)(nil)
	if source.EffectiveState != nil {
		state := string(*source.EffectiveState)
		effective = &state
	}
	var lastSuccess *Timestamp
	if source.LastSuccessAt != nil {
		at := Timestamp(source.LastSuccessAt.UTC())
		lastSuccess = &at
	}
	return ConditionState{
		ConditionMeasurements: wireConditionMeasurements(source.ConditionObservation),
		ObservedState:         string(source.State),
		EffectiveState:        effective,
		ConsecutiveState:      string(source.ConsecutiveState),
		ConsecutiveCount:      int64(source.ConsecutiveCount),
		LastSuccessAt:         lastSuccess,
	}
}

func sourceConditionEvidence(wire ConditionState) domain.ConditionEvidence {
	evidence := domain.ConditionEvidence{
		ConditionObservation: sourceConditionMeasurements(wire.ConditionMeasurements),
		ConsecutiveState:     domain.ConditionState(wire.ConsecutiveState),
		ConsecutiveCount:     int(wire.ConsecutiveCount),
	}
	evidence.State = domain.ConditionState(wire.ObservedState)
	if wire.EffectiveState != nil {
		state := domain.ConditionState(*wire.EffectiveState)
		evidence.EffectiveState = &state
	}
	if wire.LastSuccessAt != nil {
		at := time.Time(*wire.LastSuccessAt).UTC()
		evidence.LastSuccessAt = &at
	}
	return evidence
}

func sourceConditionEvidenceList(wire []ConditionState) []domain.ConditionEvidence {
	out := make([]domain.ConditionEvidence, 0, len(wire))
	for _, condition := range wire {
		out = append(out, sourceConditionEvidence(condition))
	}
	return out
}

func wireConditionEvidenceList(source []domain.ConditionEvidence) []ConditionState {
	out := make([]ConditionState, 0, len(source))
	for i := range source {
		out = append(out, wireConditionEvidence(source[i]))
	}
	return out
}

// sameRawCondition proves the evaluated snapshot evidence and the immutable
// observation payload describe the same raw checker measurement.
func sameRawCondition(raw domain.ConditionObservation, wire ConditionObservation) bool {
	if raw.Kind != wire.Kind || string(raw.State) != wire.State || raw.Message != wire.Message ||
		raw.Unit != wire.Unit || raw.Resource != wire.Resource || raw.Scope != wire.Scope || raw.Source != wire.Source ||
		!raw.ObservedAt.UTC().Equal(time.Time(wire.ObservedAt).UTC()) || !raw.StaleAfter.UTC().Equal(time.Time(wire.StaleAfter).UTC()) {
		return false
	}
	for i, pair := range [][2]*float64{{raw.Used, wire.Used}, {raw.Limit, wire.Limit}, {raw.Percent, wire.Percent}, {raw.Threshold, wire.Threshold}} {
		_ = i
		left, right := pair[0], pair[1]
		if (left == nil) != (right == nil) {
			return false
		}
		if left != nil && *left != *right {
			return false
		}
	}
	return true
}

// conditionEvidenceMatchesPayload requires the exact same raw kind set with
// identical measurements, in any order.
func conditionEvidenceMatchesPayload(evaluated []domain.ConditionEvidence, raw []ConditionObservation) bool {
	if len(evaluated) != len(raw) {
		return false
	}
	matched := make([]bool, len(raw))
	for i := range evaluated {
		found := false
		for j := range raw {
			if matched[j] || !sameRawCondition(evaluated[i].ConditionObservation, raw[j]) {
				continue
			}
			matched[j], found = true, true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

// wireConditionTransition maps one promoted auxiliary transition. Assignment
// identity is restated from the trusted source record, never guessed.
func wireConditionTransition(t domain.ConditionTransition) ConditionTransition {
	previous := (*string)(nil)
	if t.PreviousState != nil {
		state := string(*t.PreviousState)
		previous = &state
	}
	return ConditionTransition{
		MonitorID: t.MonitorID, AssignmentGeneration: Decimal(t.AssignmentGeneration),
		ConfigRevision: Decimal(t.ConfigRevision), Kind: t.Kind,
		PreviousState: previous, State: string(t.State), Message: t.Message,
		SourceAlertID: t.SourceAlertID,
	}
}

func sourceConditionTransition(wire ConditionTransition) domain.ConditionTransition {
	transition := domain.ConditionTransition{
		MonitorID: wire.MonitorID, AssignmentGeneration: int64(wire.AssignmentGeneration),
		ConfigRevision: int64(wire.ConfigRevision), Kind: wire.Kind,
		State: domain.ConditionState(wire.State), Message: wire.Message, SourceAlertID: wire.SourceAlertID,
	}
	if wire.PreviousState != nil {
		state := domain.ConditionState(*wire.PreviousState)
		transition.PreviousState = &state
	}
	return transition
}
