package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EscalationPolicyModel maps the escalation_policies table (F2.3).
type EscalationPolicyModel struct {
	bun.BaseModel `bun:"table:escalation_policies"`

	ID          int64     `bun:"id,pk,autoincrement"`
	UserID      int64     `bun:"user_id,notnull"`
	Name        string    `bun:"name,notnull"`
	Description string    `bun:"description,notnull"`
	Enabled     bool      `bun:"enabled,notnull"`
	CreatedAt   time.Time `bun:"created_at,notnull"`
	UpdatedAt   time.Time `bun:"updated_at,notnull"`
}

// EscalationStepModel maps the escalation_steps table.
type EscalationStepModel struct {
	bun.BaseModel `bun:"table:escalation_steps"`

	ID          int64 `bun:"id,pk,autoincrement"`
	PolicyID    int64 `bun:"policy_id,notnull"`
	StepOrder   int   `bun:"step_order,notnull"`
	WaitMinutes int   `bun:"wait_minutes,notnull"`
}

// EscalationStepNotificationModel maps the escalation_step_notifications link
// table. UNIQUE(step_id, notification_id) is what collapses a channel listed
// twice in one step into a single send.
type EscalationStepNotificationModel struct {
	bun.BaseModel `bun:"table:escalation_step_notifications"`

	ID             int64 `bun:"id,pk,autoincrement"`
	StepID         int64 `bun:"step_id,notnull"`
	NotificationID int64 `bun:"notification_id,notnull"`
}

// EscalationPolicyMonitorModel maps escalation_policy_monitors. The UNIQUE is on
// monitor_id alone: a monitor has at most one policy.
type EscalationPolicyMonitorModel struct {
	bun.BaseModel `bun:"table:escalation_policy_monitors"`

	ID        int64 `bun:"id,pk,autoincrement"`
	MonitorID int64 `bun:"monitor_id,notnull"`
	PolicyID  int64 `bun:"policy_id,notnull"`
}

// EscalationPolicyGroupModel maps escalation_policy_groups. The UNIQUE is on
// group_id alone: a group has at most one policy.
type EscalationPolicyGroupModel struct {
	bun.BaseModel `bun:"table:escalation_policy_groups"`

	ID       int64 `bun:"id,pk,autoincrement"`
	GroupID  int64 `bun:"group_id,notnull"`
	PolicyID int64 `bun:"policy_id,notnull"`
}

// AlertEscalationModel maps the alert_escalations table — one alert's persisted
// progress through a policy.
type AlertEscalationModel struct {
	bun.BaseModel `bun:"table:alert_escalations"`

	ID         int64      `bun:"id,pk,autoincrement"`
	AlertID    int64      `bun:"alert_id,notnull"`
	MonitorID  int64      `bun:"monitor_id,notnull"`
	PolicyID   int64      `bun:"policy_id,notnull"`
	NextStep   int        `bun:"next_step,notnull"`
	NextRunAt  time.Time  `bun:"next_run_at,notnull"`
	Status     string     `bun:"status,notnull"`
	LeaseOwner *string    `bun:"lease_owner"`
	LeaseUntil *time.Time `bun:"lease_until"`
	CreatedAt  time.Time  `bun:"created_at,notnull"`
	UpdatedAt  time.Time  `bun:"updated_at,notnull"`
}

// ToDomain converts an EscalationPolicyModel to a domain.EscalationPolicy.
// Steps are populated separately by the repository.
func (m *EscalationPolicyModel) ToDomain() *domain.EscalationPolicy {
	if m == nil {
		return nil
	}
	return &domain.EscalationPolicy{
		ID:          m.ID,
		UserID:      m.UserID,
		Name:        m.Name,
		Description: m.Description,
		Enabled:     m.Enabled,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

// EscalationPolicyModelFromDomain converts a domain.EscalationPolicy to its model.
func EscalationPolicyModelFromDomain(p *domain.EscalationPolicy) *EscalationPolicyModel {
	return &EscalationPolicyModel{
		ID:          p.ID,
		UserID:      p.UserID,
		Name:        p.Name,
		Description: p.Description,
		Enabled:     p.Enabled,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
}

// ToDomain converts an AlertEscalationModel to a domain.AlertEscalation.
func (m *AlertEscalationModel) ToDomain() *domain.AlertEscalation {
	if m == nil {
		return nil
	}
	return &domain.AlertEscalation{
		ID:         m.ID,
		AlertID:    m.AlertID,
		MonitorID:  m.MonitorID,
		PolicyID:   m.PolicyID,
		NextStep:   m.NextStep,
		NextRunAt:  m.NextRunAt,
		Status:     m.Status,
		LeaseOwner: m.LeaseOwner,
		LeaseUntil: m.LeaseUntil,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
}

// AlertEscalationModelFromDomain converts a domain.AlertEscalation to its model.
func AlertEscalationModelFromDomain(e *domain.AlertEscalation) *AlertEscalationModel {
	return &AlertEscalationModel{
		ID:         e.ID,
		AlertID:    e.AlertID,
		MonitorID:  e.MonitorID,
		PolicyID:   e.PolicyID,
		NextStep:   e.NextStep,
		NextRunAt:  e.NextRunAt,
		Status:     e.Status,
		LeaseOwner: e.LeaseOwner,
		LeaseUntil: e.LeaseUntil,
		CreatedAt:  e.CreatedAt,
		UpdatedAt:  e.UpdatedAt,
	}
}
