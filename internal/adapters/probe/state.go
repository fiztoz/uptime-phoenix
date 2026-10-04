package probe

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// MaxStateSnapshotBytes bounds the complete reconstructed snapshot JSON.
	MaxStateSnapshotBytes = 16 << 20
	// MaxSnapshotStates bounds unique assignments in a current-state snapshot.
	MaxSnapshotStates = 10000
)

// StateSnapshot carries source-evaluated current evidence independently of the
// historical cursor. Decoding does not authorize or apply any of these states.
type StateSnapshot struct {
	StreamID       string         `json:"stream_id"`
	ConfigRevision Decimal        `json:"config_revision"`
	CreatedAt      Timestamp      `json:"created_at"`
	LastCreatedSeq Decimal        `json:"last_created_seq"`
	States         []MonitorState `json:"states"`
}

// MonitorState is the last source observation for one assignment. Missing
// assignments require UNKNOWN reconciliation against the authorized config.
type MonitorState struct {
	MonitorID            int64            `json:"monitor_id"`
	AssignmentGeneration Decimal          `json:"assignment_generation"`
	LastObservationSeq   Decimal          `json:"last_observation_seq"`
	ObservedAt           Timestamp        `json:"observed_at"`
	Status               string           `json:"status"`
	DownCount            int64            `json:"down_count"`
	Ping                 int64            `json:"ping"`
	Message              string           `json:"message"`
	Conditions           []ConditionState `json:"conditions"`
	TLS                  *TLSObservation  `json:"tls"`
	ActiveSourceAlertID  *string          `json:"active_source_alert_id"`
}

// ConditionState separates raw observations, hysteresis candidates, and promoted
// state. A null EffectiveState is unconfirmed; freshness is derived by readers.
type ConditionState struct {
	ConditionMeasurements
	ObservedState    string     `json:"observed_state"`
	EffectiveState   *string    `json:"effective_state"`
	ConsecutiveState string     `json:"consecutive_state"`
	ConsecutiveCount int64      `json:"consecutive_count"`
	LastSuccessAt    *Timestamp `json:"last_success_at"`
}

// DecodeStateSnapshot validates complete reconstructed JSON, including duplicate
// keys and bounded entries. It never reevaluates retries, promotion, or freshness,
// and it does not advance a cursor, generate observations, or send notifications.
func DecodeStateSnapshot(data []byte) (StateSnapshot, error) {
	var snapshot StateSnapshot
	if len(data) > MaxStateSnapshotBytes {
		return snapshot, errors.New("state snapshot exceeds maximum bytes")
	}
	if err := validateJSON(data); err != nil {
		return snapshot, err
	}
	fields, err := objectFields(data)
	if err != nil {
		return snapshot, err
	}
	if err := requiredUUID(fields, "stream_id", &snapshot.StreamID); err != nil {
		return snapshot, err
	}
	if err := decodeRequiredFields(fields,
		field{"config_revision", &snapshot.ConfigRevision}, field{"created_at", &snapshot.CreatedAt},
		field{"last_created_seq", &snapshot.LastCreatedSeq},
	); err != nil {
		return snapshot, err
	}
	if snapshot.ConfigRevision <= 0 {
		return snapshot, errors.New("state config_revision must be positive")
	}
	var states []json.RawMessage
	if err := required(fields, "states", &states); err != nil {
		return snapshot, err
	}
	if len(states) > MaxSnapshotStates {
		return snapshot, errors.New("state snapshot exceeds maximum assignments")
	}
	snapshot.States = make([]MonitorState, 0, len(states))
	monitors := make(map[int64]struct{}, len(states))
	sequences := make(map[Decimal]struct{}, len(states))
	for index, raw := range states {
		state, err := decodeMonitorState(raw)
		if err != nil {
			return StateSnapshot{}, fmt.Errorf("states[%d]: %w", index, err)
		}
		if state.LastObservationSeq > snapshot.LastCreatedSeq {
			return StateSnapshot{}, errors.New("state observation exceeds last_created_seq")
		}
		if _, duplicate := monitors[state.MonitorID]; duplicate {
			return StateSnapshot{}, errors.New("duplicate state monitor_id")
		}
		if _, duplicate := sequences[state.LastObservationSeq]; duplicate {
			return StateSnapshot{}, errors.New("duplicate state last_observation_seq")
		}
		monitors[state.MonitorID] = struct{}{}
		sequences[state.LastObservationSeq] = struct{}{}
		snapshot.States = append(snapshot.States, state)
	}
	return snapshot, nil
}

func decodeMonitorState(data []byte) (MonitorState, error) {
	var state MonitorState
	if len(data) > MaxEventBytes {
		return state, errors.New("state exceeds maximum bytes")
	}
	fields, err := objectFields(data)
	if err != nil {
		return state, err
	}
	if err := decodeRequiredFields(fields,
		field{"monitor_id", &state.MonitorID}, field{"assignment_generation", &state.AssignmentGeneration},
		field{"last_observation_seq", &state.LastObservationSeq}, field{"observed_at", &state.ObservedAt},
		field{"status", &state.Status}, field{"down_count", &state.DownCount},
		field{"ping", &state.Ping}, field{"message", &state.Message},
	); err != nil {
		return state, err
	}
	if state.MonitorID <= 0 || state.AssignmentGeneration <= 0 || state.LastObservationSeq <= 0 {
		return state, errors.New("state identity, generation and observation sequence must be positive")
	}
	if state.DownCount < 0 || state.Ping < 0 || len(state.Message) > MaxMessageBytes {
		return state, errors.New("invalid state counts, latency or message length")
	}
	switch state.Status {
	case "UP", "MAINTENANCE":
		if state.DownCount != 0 {
			return state, errors.New("UP/MAINTENANCE state requires zero down_count")
		}
	case "DOWN":
		if state.DownCount == 0 {
			return state, errors.New("DOWN state requires positive down_count")
		}
	case "PENDING": // Raw PENDING has zero failures; retries have positive counts.
	default:
		return state, errors.New("invalid source state status")
	}
	if err := nullable(fields, "active_source_alert_id", &state.ActiveSourceAlertID); err != nil {
		return state, err
	}
	if state.ActiveSourceAlertID != nil {
		if err := requiredUUID(fields, "active_source_alert_id", state.ActiveSourceAlertID); err != nil {
			return state, err
		}
	}
	var tlsRaw *json.RawMessage
	if err := nullable(fields, "tls", &tlsRaw); err != nil {
		return state, err
	}
	if tlsRaw != nil {
		tls, err := decodeTLS(*tlsRaw)
		if err != nil {
			return state, fmt.Errorf("tls: %w", err)
		}
		state.TLS = &tls
	}
	var conditions []json.RawMessage
	if err := required(fields, "conditions", &conditions); err != nil {
		return state, err
	}
	if len(conditions) > 2 {
		return state, errors.New("at most two distinct condition kinds are supported")
	}
	state.Conditions = make([]ConditionState, 0, len(conditions))
	for _, raw := range conditions {
		condition, err := decodeConditionState(raw)
		if err != nil {
			return state, fmt.Errorf("condition: %w", err)
		}
		for _, previous := range state.Conditions {
			if previous.Kind == condition.Kind {
				return state, errors.New("duplicate condition kind")
			}
		}
		state.Conditions = append(state.Conditions, condition)
	}
	return state, nil
}

func decodeConditionState(data []byte) (ConditionState, error) {
	var condition ConditionState
	fields, err := objectFields(data)
	if err != nil {
		return condition, err
	}
	if err := decodeConditionMeasurements(fields, &condition.ConditionMeasurements); err != nil {
		return condition, err
	}
	if err := decodeRequiredFields(fields,
		field{"observed_state", &condition.ObservedState}, field{"consecutive_state", &condition.ConsecutiveState},
		field{"consecutive_count", &condition.ConsecutiveCount},
	); err != nil {
		return condition, err
	}
	if err := nullable(fields, "effective_state", &condition.EffectiveState); err != nil {
		return condition, err
	}
	if err := nullable(fields, "last_success_at", &condition.LastSuccessAt); err != nil {
		return condition, err
	}
	if !validConditionState(condition.ObservedState) || !validConditionState(condition.ConsecutiveState) ||
		condition.EffectiveState != nil && !validConditionState(*condition.EffectiveState) {
		return condition, errors.New("invalid observed/candidate/effective condition state")
	}
	if condition.ConsecutiveCount <= 0 {
		return condition, errors.New("condition consecutive_count must be positive")
	}
	if condition.EffectiveState == nil {
		if condition.ConsecutiveCount != 1 || condition.ConsecutiveState == "ok" {
			return condition, errors.New("only a first warning/error may be unconfirmed")
		}
	} else if condition.ConsecutiveCount >= 2 && *condition.EffectiveState != condition.ConsecutiveState {
		return condition, errors.New("confirmed condition must match its candidate")
	}
	if condition.ObservedState != condition.ConsecutiveState &&
		!(condition.ObservedState == "ok" && condition.ConsecutiveState == "warning" &&
			condition.EffectiveState != nil && *condition.EffectiveState == "warning") {
		return condition, errors.New("condition candidate differs from observation without warning hysteresis")
	}
	if condition.ObservedState != "error" && condition.LastSuccessAt == nil {
		return condition, errors.New("successful condition measurement requires last_success_at")
	}
	return condition, nil
}
