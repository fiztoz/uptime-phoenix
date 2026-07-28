package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// UserModel maps the users table.
type UserModel struct {
	bun.BaseModel `bun:"table:users"`

	ID                     int64     `bun:"id,pk,autoincrement"`
	Username               string    `bun:"username,notnull"`
	AuthHash               string    `bun:"password_hash,notnull"` // bcrypt hash from auth adapter
	Active                 bool      `bun:"active,notnull,default:true"`
	IsAdmin                bool      `bun:"is_admin,notnull,default:false"`
	CanManageNotifications bool      `bun:"can_manage_notifications,notnull,default:false"`
	CanManageMaintenance   bool      `bun:"can_manage_maintenance,notnull,default:false"`
	CanCreateMonitors      bool      `bun:"can_create_monitors,notnull,default:false"`
	CanCreateGroups        bool      `bun:"can_create_groups,notnull,default:false"`
	Timezone               string    `bun:"timezone,default:'UTC'"`
	TOTPSeed               string    `bun:"totp_secret"` // encrypted TOTP seed
	TOTPOn                 bool      `bun:"totp_enabled,notnull,default:false"`
	CreatedAt              time.Time `bun:"created_at,notnull"`
	UpdatedAt              time.Time `bun:"updated_at,notnull"`
}

// ToDomain converts a UserModel to a domain.User.
func (m *UserModel) ToDomain() *domain.User {
	return &domain.User{
		ID:                     m.ID,
		Username:               m.Username,
		PasswordHash:           m.AuthHash,
		Active:                 m.Active,
		IsAdmin:                m.IsAdmin,
		CanManageNotifications: m.CanManageNotifications,
		CanManageMaintenance:   m.CanManageMaintenance,
		CanCreateMonitors:      m.CanCreateMonitors,
		CanCreateGroups:        m.CanCreateGroups,
		Timezone:               m.Timezone,
		TOTPSecret:             m.TOTPSeed,
		TOTPEnabled:            m.TOTPOn,
		CreatedAt:              m.CreatedAt,
		UpdatedAt:              m.UpdatedAt,
	}
}

// UserModelFromDomain converts a domain.User to a UserModel.
func UserModelFromDomain(u *domain.User) *UserModel {
	return &UserModel{
		ID:                     u.ID,
		Username:               u.Username,
		AuthHash:               u.PasswordHash,
		Active:                 u.Active,
		IsAdmin:                u.IsAdmin,
		CanManageNotifications: u.CanManageNotifications,
		CanManageMaintenance:   u.CanManageMaintenance,
		CanCreateMonitors:      u.CanCreateMonitors,
		CanCreateGroups:        u.CanCreateGroups,
		Timezone:               u.Timezone,
		TOTPSeed:               u.TOTPSecret,
		TOTPOn:                 u.TOTPEnabled,
		CreatedAt:              u.CreatedAt,
		UpdatedAt:              u.UpdatedAt,
	}
}
