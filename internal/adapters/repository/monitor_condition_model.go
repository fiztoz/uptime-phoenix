package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// MonitorConditionModel maps the latest auxiliary condition for a monitor.
type MonitorConditionModel struct {
	bun.BaseModel `bun:"table:monitor_conditions"`

	MonitorID         int64                 `bun:"monitor_id,pk"`
	Kind              string                `bun:"kind,pk"`
	State             domain.ConditionState `bun:"state,notnull"`
	UsedValue         *float64              `bun:"used_value"`
	LimitValue        *float64              `bun:"limit_value"`
	PercentValue      *float64              `bun:"percent_value"`
	ThresholdValue    *float64              `bun:"threshold_value"`
	Unit              string                `bun:"unit,notnull"`
	Resource          string                `bun:"resource,notnull"`
	Scope             string                `bun:"scope,notnull"`
	Source            string                `bun:"source,notnull"`
	Message           string                `bun:"message,notnull"`
	ObservedAt        time.Time             `bun:"observed_at,notnull"`
	StaleAfter        time.Time             `bun:"stale_after,notnull"`
	LastSuccessAt     *time.Time            `bun:"last_success_at"`
	ConsecutiveState  domain.ConditionState `bun:"consecutive_state,notnull"`
	ConsecutiveCount  int                   `bun:"consecutive_count,notnull"`
	LastNotifiedState domain.ConditionState `bun:"last_notified_state,notnull"`
	LastNotifiedAt    *time.Time            `bun:"last_notified_at"`
}

// MonitorConditionModelFromDomain converts a condition for persistence.
func MonitorConditionModelFromDomain(condition *domain.MonitorCondition) *MonitorConditionModel {
	return &MonitorConditionModel{
		MonitorID:         condition.MonitorID,
		Kind:              condition.Kind,
		State:             condition.State,
		UsedValue:         condition.Used,
		LimitValue:        condition.Limit,
		PercentValue:      condition.Percent,
		ThresholdValue:    condition.Threshold,
		Unit:              condition.Unit,
		Resource:          condition.Resource,
		Scope:             condition.Scope,
		Source:            condition.Source,
		Message:           condition.Message,
		ObservedAt:        condition.ObservedAt.UTC(),
		StaleAfter:        condition.StaleAfter.UTC(),
		LastSuccessAt:     utcTimePtr(condition.LastSuccessAt),
		ConsecutiveState:  condition.ConsecutiveState,
		ConsecutiveCount:  condition.ConsecutiveCount,
		LastNotifiedState: condition.LastNotifiedState,
		LastNotifiedAt:    utcTimePtr(condition.LastNotifiedAt),
	}
}

// ToDomain converts a persisted condition into its pure domain representation.
func (m *MonitorConditionModel) ToDomain() *domain.MonitorCondition {
	return &domain.MonitorCondition{
		MonitorID: m.MonitorID,
		ConditionObservation: domain.ConditionObservation{
			Kind:       m.Kind,
			State:      m.State,
			Used:       m.UsedValue,
			Limit:      m.LimitValue,
			Percent:    m.PercentValue,
			Threshold:  m.ThresholdValue,
			Unit:       m.Unit,
			Resource:   m.Resource,
			Scope:      m.Scope,
			Source:     m.Source,
			Message:    m.Message,
			ObservedAt: m.ObservedAt.UTC(),
			StaleAfter: m.StaleAfter.UTC(),
		},
		LastSuccessAt:     utcTimePtr(m.LastSuccessAt),
		ConsecutiveState:  m.ConsecutiveState,
		ConsecutiveCount:  m.ConsecutiveCount,
		LastNotifiedState: m.LastNotifiedState,
		LastNotifiedAt:    utcTimePtr(m.LastNotifiedAt),
	}
}

func utcTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
