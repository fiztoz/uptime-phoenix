package domain

import "time"

// Monitor condition kinds emitted by checkers. A condition is deliberately
// separate from heartbeat Status: it describes resource pressure or an
// auxiliary-check failure while the primary availability probe can remain UP.
const (
	MonitorConditionSessionPool = "session_pool"
	MonitorConditionStorage     = "storage"
)

// ConditionState is the latest state of an auxiliary monitor condition.
type ConditionState string

const (
	ConditionStateOK      ConditionState = "ok"
	ConditionStateWarning ConditionState = "warning"
	ConditionStateError   ConditionState = "error"
	ConditionStateStale   ConditionState = "stale"
)

// IsValid reports whether the state can be persisted as an observed state.
// Stale is derived from StaleAfter and is therefore not stored as the latest
// observation state.
func (s ConditionState) IsValid() bool {
	return s == ConditionStateOK || s == ConditionStateWarning || s == ConditionStateError
}

// IsAttention reports whether this condition should appear in operator
// attention surfaces.
func (s ConditionState) IsAttention() bool {
	return s == ConditionStateWarning || s == ConditionStateError || s == ConditionStateStale
}

// ConditionObservation is the typed auxiliary result emitted by a checker.
// Numeric fields are pointers so a real zero is distinguishable from a query
// error that produced no measurement.
type ConditionObservation struct {
	Kind       string
	State      ConditionState
	Used       *float64
	Limit      *float64
	Percent    *float64
	Threshold  *float64
	Unit       string
	Resource   string
	Scope      string
	Source     string
	Message    string
	ObservedAt time.Time
	StaleAfter time.Time
}

// MonitorCondition is the persisted latest state and notification cursor for
// one auxiliary signal on one monitor.
type MonitorCondition struct {
	MonitorID int64
	ConditionObservation
	LastSuccessAt     *time.Time
	ConsecutiveState  ConditionState
	ConsecutiveCount  int
	LastNotifiedState ConditionState
	LastNotifiedAt    *time.Time
}

// ConditionDelete is published when a persisted condition row is removed.
// It has no json tags — HTTP/WS adapters map it to an explicit view.
type ConditionDelete struct {
	MonitorID int64
	Kind      string
}

// DisplayState returns Stale after the observation freshness deadline without
// mutating the persisted state.
func (c *MonitorCondition) DisplayState(now time.Time) ConditionState {
	if c == nil {
		return ConditionStateStale
	}
	if !c.StaleAfter.IsZero() && !now.UTC().Before(c.StaleAfter.UTC()) {
		return ConditionStateStale
	}
	return c.State
}
