package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ConfigKeyModel maps the config_keys table (migration 020).
type ConfigKeyModel struct {
	bun.BaseModel `bun:"table:config_keys"`

	ID           int64     `bun:"id,pk,autoincrement"`
	ResourceType string    `bun:"resource_type,notnull"`
	KeyName      string    `bun:"key_name,notnull"`
	ResourceID   int64     `bun:"resource_id,notnull"`
	CreatedAt    time.Time `bun:"created_at,notnull"`
	UpdatedAt    time.Time `bun:"updated_at,notnull"`
}

// ToDomain converts a ConfigKeyModel to a domain.ConfigKey.
func (m *ConfigKeyModel) ToDomain() *domain.ConfigKey {
	return &domain.ConfigKey{
		ID:           m.ID,
		ResourceType: m.ResourceType,
		KeyName:      m.KeyName,
		ResourceID:   m.ResourceID,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
}

// ConfigKeyModelFromDomain converts a domain.ConfigKey to a model.
func ConfigKeyModelFromDomain(k *domain.ConfigKey) *ConfigKeyModel {
	return &ConfigKeyModel{
		ID:           k.ID,
		ResourceType: k.ResourceType,
		KeyName:      k.KeyName,
		ResourceID:   k.ResourceID,
		CreatedAt:    k.CreatedAt,
		UpdatedAt:    k.UpdatedAt,
	}
}
