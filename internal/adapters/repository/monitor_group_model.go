package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// MonitorGroupModel maps the monitor_groups table.
//
// The condition column is stored as status_condition, not `condition`:
// CONDITION is a reserved word in MariaDB (used by DECLARE ... CONDITION
// FOR), and using the same column name in both engines keeps this model and
// every hand-written query identical across the sqlite and mariadb adapters.
type MonitorGroupModel struct {
	bun.BaseModel `bun:"table:monitor_groups"`

	ID                 int64  `bun:"id,pk,autoincrement"`
	UserID             int64  `bun:"user_id,notnull"`
	Name               string `bun:"name,notnull"`
	Description        string `bun:"description"`
	ParentID           *int64 `bun:"parent_id"`
	Condition          string `bun:"status_condition,notnull,default:'worst_of_children'"`
	Threshold          int    `bun:"threshold,notnull,default:0"`
	ThresholdIsPercent bool   `bun:"threshold_is_percent,notnull,default:false"`
	Weight             int    `bun:"weight,notnull,default:2000"`
	Collapsed          bool   `bun:"collapsed,notnull,default:false"`
	// LastStatus is the group's last OBSERVED derived status (migration 009).
	// Nullable: NULL means "never evaluated". It is written ONLY by
	// MonitorGroupRepo.ClaimStatusTransition — every normal Update excludes the
	// column, so an admin PUT cannot clobber a transition a worker just claimed.
	LastStatus *int      `bun:"last_status"`
	CreatedAt  time.Time `bun:"created_at,notnull"`
	UpdatedAt  time.Time `bun:"updated_at,notnull"`
}

// ToDomain converts a MonitorGroupModel to a domain.MonitorGroup.
func (m *MonitorGroupModel) ToDomain() *domain.MonitorGroup {
	var lastStatus *domain.Status
	if m.LastStatus != nil {
		s := domain.Status(*m.LastStatus)
		lastStatus = &s
	}
	return &domain.MonitorGroup{
		ID:                 m.ID,
		UserID:             m.UserID,
		Name:               m.Name,
		Description:        m.Description,
		ParentID:           m.ParentID,
		Condition:          domain.GroupCondition(m.Condition),
		Threshold:          m.Threshold,
		ThresholdIsPercent: m.ThresholdIsPercent,
		Weight:             m.Weight,
		Collapsed:          m.Collapsed,
		LastStatus:         lastStatus,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
	}
}

// MonitorGroupModelFromDomain converts a domain.MonitorGroup to a MonitorGroupModel.
func MonitorGroupModelFromDomain(g *domain.MonitorGroup) *MonitorGroupModel {
	var lastStatus *int
	if g.LastStatus != nil {
		v := int(*g.LastStatus)
		lastStatus = &v
	}
	return &MonitorGroupModel{
		ID:                 g.ID,
		UserID:             g.UserID,
		Name:               g.Name,
		Description:        g.Description,
		ParentID:           g.ParentID,
		Condition:          string(g.Condition),
		Threshold:          g.Threshold,
		ThresholdIsPercent: g.ThresholdIsPercent,
		Weight:             g.Weight,
		Collapsed:          g.Collapsed,
		LastStatus:         lastStatus,
		CreatedAt:          g.CreatedAt,
		UpdatedAt:          g.UpdatedAt,
	}
}
