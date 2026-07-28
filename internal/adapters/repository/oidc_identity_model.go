package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// OIDCIdentityModel maps the oidc_identities table (migration 019).
type OIDCIdentityModel struct {
	bun.BaseModel `bun:"table:oidc_identities"`

	ID          int64      `bun:"id,pk,autoincrement"`
	UserID      int64      `bun:"user_id,notnull"`
	Issuer      string     `bun:"issuer,notnull"`
	Subject     string     `bun:"subject,notnull"`
	Email       string     `bun:"email,notnull,default:''"`
	LastLoginAt *time.Time `bun:"last_login_at"`
	CreatedAt   time.Time  `bun:"created_at,notnull"`
	UpdatedAt   time.Time  `bun:"updated_at,notnull"`
}

// ToDomain converts an OIDCIdentityModel to a domain.OIDCIdentity.
func (m *OIDCIdentityModel) ToDomain() *domain.OIDCIdentity {
	return &domain.OIDCIdentity{
		ID:          m.ID,
		UserID:      m.UserID,
		Issuer:      m.Issuer,
		Subject:     m.Subject,
		Email:       m.Email,
		LastLoginAt: m.LastLoginAt,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

// OIDCIdentityModelFromDomain converts a domain.OIDCIdentity to a model.
func OIDCIdentityModelFromDomain(id *domain.OIDCIdentity) *OIDCIdentityModel {
	return &OIDCIdentityModel{
		ID:          id.ID,
		UserID:      id.UserID,
		Issuer:      id.Issuer,
		Subject:     id.Subject,
		Email:       id.Email,
		LastLoginAt: id.LastLoginAt,
		CreatedAt:   id.CreatedAt,
		UpdatedAt:   id.UpdatedAt,
	}
}
