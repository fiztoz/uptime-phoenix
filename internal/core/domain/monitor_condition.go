package domain

import (
	"math"
	"time"
)

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
	MonitorID            int64
	ProbeID              string
	AssignmentGeneration int64
	ConditionObservation
	LastSuccessAt     *time.Time
	ConsecutiveState  ConditionState
	ConsecutiveCount  int
	LastNotifiedState ConditionState
	LastNotifiedAt    *time.Time
}

// ConditionEvidence is one source-evaluated condition state. The embedded
// observation keeps the RAW checker state and measurements; EffectiveState is
// the promoted (two-sample confirmed) state and is nil while unconfirmed.
// ConsecutiveState/Count track the pending transition candidate. The
// notification cursor deliberately stays out: delivery is source-owned and its
// cursor never crosses the wire.
type ConditionEvidence struct {
	ConditionObservation
	EffectiveState   *ConditionState
	ConsecutiveState ConditionState
	ConsecutiveCount int
	LastSuccessAt    *time.Time
}

// ConditionTransition is one promoted auxiliary transition. It never changes
// primary availability and never re-derives promotion at a reader.
type ConditionTransition struct {
	MonitorID            int64
	AssignmentGeneration int64
	ConfigRevision       int64
	Kind                 string
	PreviousState        *ConditionState
	State                ConditionState
	Message              string
	SourceAlertID        *string
}

// ValidConditionEvidence enforces the same promotion invariants the wire
// decoder applies, so callers that bypass DTO decoding cannot persist an
// impossible state: only a first warning/error may be unconfirmed, a confirmed
// condition matches its candidate once it has two samples, and the only legal
// observed/candidate disagreement is the five-point warning recovery latch.
func ValidConditionEvidence(e *ConditionEvidence) bool {
	if e == nil || e.Kind != MonitorConditionSessionPool && e.Kind != MonitorConditionStorage ||
		!e.State.IsValid() || !e.ConsecutiveState.IsValid() || e.ConsecutiveCount <= 0 ||
		e.EffectiveState != nil && !e.EffectiveState.IsValid() ||
		len(e.Message) > 4096 || len(e.Unit) > 256 || len(e.Resource) > 256 || len(e.Scope) > 256 || len(e.Source) > 256 {
		return false
	}
	for _, value := range []*float64{e.Used, e.Limit, e.Percent, e.Threshold} {
		if value != nil && (*value < 0 || *value != *value || *value > math.MaxFloat64) {
			return false
		}
	}
	if e.StaleAfter.IsZero() || e.ObservedAt.IsZero() || e.StaleAfter.Before(e.ObservedAt) {
		return false
	}
	if e.EffectiveState == nil {
		if e.ConsecutiveCount != 1 || e.ConsecutiveState == ConditionStateOK {
			return false
		}
	} else if e.ConsecutiveCount >= 2 && *e.EffectiveState != e.ConsecutiveState {
		return false
	}
	if e.State != e.ConsecutiveState &&
		!(e.State == ConditionStateOK && e.ConsecutiveState == ConditionStateWarning &&
			e.EffectiveState != nil && *e.EffectiveState == ConditionStateWarning) {
		return false
	}
	if e.State != ConditionStateError && e.LastSuccessAt == nil {
		return false
	}
	return true
}

// ValidConditionTransition checks one promoted transition identity. A previous
// state, when present, is a different confirmed state.
func ValidConditionTransition(t *ConditionTransition) bool {
	if t == nil || t.MonitorID <= 0 || t.AssignmentGeneration <= 0 || t.ConfigRevision <= 0 ||
		t.Kind != MonitorConditionSessionPool && t.Kind != MonitorConditionStorage ||
		!t.State.IsValid() || len(t.Message) > 4096 {
		return false
	}
	if t.PreviousState != nil && (!t.PreviousState.IsValid() || *t.PreviousState == t.State) {
		return false
	}
	return t.SourceAlertID == nil || ValidHubID(*t.SourceAlertID)
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
