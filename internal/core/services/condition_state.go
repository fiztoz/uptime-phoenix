package services

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ConditionEvaluation is one check's evaluated auxiliary condition: the durable
// evidence to store and, only when the promoted state changed, its transition.
type ConditionEvaluation struct {
	State      domain.ConditionEvidence
	Transition *domain.ConditionTransition
}

// EvaluateCondition applies the shared two-sample promotion contract to one raw
// checker observation for a trusted owning assignment. It runs at the source
// that owns the assignment and never persists, notifies or consults
// maintenance. The caller's observation is not mutated: the raw checker state is
// preserved as evidence while warning recovery hysteresis shapes the candidate
// exactly as MonitorConditionService does locally.
func EvaluateCondition(prior *domain.MonitorCondition, observation domain.ConditionObservation, monitorID int64, intervalSeconds int, at time.Time) ConditionEvaluation {
	// Every persisted edge/hub timestamp column is microsecond precision;
	// normalize once so payload, row and snapshot carry the identical instant.
	at = at.UTC().Truncate(time.Microsecond)
	if !observation.ObservedAt.IsZero() {
		observation.ObservedAt = observation.ObservedAt.UTC().Truncate(time.Microsecond)
	}
	raw := observation.State
	promoted := PromoteCondition(prior, observation, monitorID, intervalSeconds, at)
	evidence := domain.ConditionEvidence{
		ConditionObservation: domain.ConditionObservation{
			Kind:       promoted.Kind,
			State:      raw,
			Used:       promoted.Used,
			Limit:      promoted.Limit,
			Percent:    promoted.Percent,
			Threshold:  promoted.Threshold,
			Unit:       promoted.Unit,
			Resource:   promoted.Resource,
			Scope:      promoted.Scope,
			Source:     promoted.Source,
			Message:    promoted.Message,
			ObservedAt: promoted.ObservedAt,
			StaleAfter: promoted.StaleAfter,
		},
		ConsecutiveState: promoted.ConsecutiveState,
		ConsecutiveCount: promoted.ConsecutiveCount,
		LastSuccessAt:    promoted.LastSuccessAt,
	}
	if promoted.State != "" {
		state := promoted.State
		evidence.EffectiveState = &state
	}
	evaluation := ConditionEvaluation{State: evidence}
	var previous domain.ConditionState
	if prior != nil {
		previous = prior.State
	}
	if evidence.EffectiveState != nil && *evidence.EffectiveState != previous {
		transition := &domain.ConditionTransition{
			MonitorID: monitorID,
			Kind:      evidence.Kind,
			State:     *evidence.EffectiveState,
			Message:   evidence.Message,
		}
		if previous != "" {
			prior := previous
			transition.PreviousState = &prior
		}
		evaluation.Transition = transition
	}
	return evaluation
}

// ConditionStatePriors adapts durable source condition rows to the promotion
// input. A missing or unconfirmed row behaves like no previous state.
func ConditionStatePriors(states []domain.EdgeConditionState) map[string]*domain.MonitorCondition {
	priors := make(map[string]*domain.MonitorCondition, len(states))
	for i := range states {
		state := states[i]
		prior := &domain.MonitorCondition{
			ConditionObservation: state.ConditionObservation,
			LastSuccessAt:        state.LastSuccessAt,
			ConsecutiveState:     state.ConsecutiveState,
			ConsecutiveCount:     state.ConsecutiveCount,
		}
		// MonitorCondition.State is the PROMOTED state. An unconfirmed row has
		// none, and the embedded raw observation must not leak into it.
		prior.State = ""
		if state.EffectiveState != nil {
			prior.State = *state.EffectiveState
		}
		priors[state.Kind] = prior
	}
	return priors
}

// ConditionStateVersions returns the durable write fence per stored kind.
func ConditionStateVersions(states []domain.EdgeConditionState) map[string]int64 {
	versions := make(map[string]int64, len(states))
	for _, state := range states {
		versions[state.Kind] = state.Version
	}
	return versions
}

// ConditionKindEnabled reports whether the accepted monitor configuration
// still samples one auxiliary capacity kind. A disabled kind retires its row;
// an enabled kind that failed to sample keeps its row and simply ages to stale.
func ConditionKindEnabled(config map[string]any, kind string) bool {
	switch kind {
	case domain.MonitorConditionSessionPool:
		return conditionConfigBool(config, "check_session_pool")
	case domain.MonitorConditionStorage:
		return conditionConfigBool(config, "check_storage")
	default:
		return false
	}
}

func conditionConfigBool(config map[string]any, key string) bool {
	raw, ok := config[key]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		s := strings.TrimSpace(strings.ToLower(value))
		return s == "true" || s == "1" || s == "yes"
	case int:
		return value != 0
	case int64:
		return value != 0
	case float64:
		return value != 0
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return n != 0
		}
		f, err := value.Float64()
		return err == nil && f != 0
	default:
		return false
	}
}
