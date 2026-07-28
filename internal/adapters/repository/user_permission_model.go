package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// UserPermissionModel maps the user_permissions table (migrations 008, 011).
//
// MonitorID and GroupID are both nullable and exactly one is set per row — the
// DB enforces it with a CHECK, domain.UserPermission.Valid enforces it in
// process. Bun needs no special tag for that: a *int64 marshals to NULL.
type UserPermissionModel struct {
	bun.BaseModel `bun:"table:user_permissions"`

	ID        int64  `bun:"id,pk,autoincrement"`
	UserID    int64  `bun:"user_id,notnull"`
	MonitorID *int64 `bun:"monitor_id"`
	GroupID   *int64 `bun:"group_id"`
	// IncludeDescendants is NOT tagged nullzero. The column is NOT NULL DEFAULT 1
	// (migration 011), so nullzero would make a shallow grant — the false case —
	// unwritable: Bun would omit the column, the DB would fill in its default of
	// 1, and the grant would silently come back deep. Writing the bool always is
	// the whole reason the flag can be turned off at all.
	IncludeDescendants bool      `bun:"include_descendants,notnull"`
	CreatedAt          time.Time `bun:"created_at,notnull"`
}

// ToDomain converts a UserPermissionModel to a domain.UserPermission.
func (m *UserPermissionModel) ToDomain() *domain.UserPermission {
	return &domain.UserPermission{
		ID:                 m.ID,
		UserID:             m.UserID,
		MonitorID:          m.MonitorID,
		GroupID:            m.GroupID,
		IncludeDescendants: m.IncludeDescendants,
		CreatedAt:          m.CreatedAt,
	}
}

// UserPermissionModelFromDomain converts a domain.UserPermission to a model.
func UserPermissionModelFromDomain(p *domain.UserPermission) *UserPermissionModel {
	return &UserPermissionModel{
		ID:                 p.ID,
		UserID:             p.UserID,
		MonitorID:          p.MonitorID,
		GroupID:            p.GroupID,
		IncludeDescendants: p.IncludeDescendants,
		CreatedAt:          p.CreatedAt,
	}
}
