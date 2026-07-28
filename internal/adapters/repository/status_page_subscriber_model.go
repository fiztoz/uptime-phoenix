package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// StatusPageEmailSubscriberModel maps the post-014 status_page_subscribers
// email table. The dormant webhook model remains in misc_models.go for the
// integrator to remove after Track A lands.
type StatusPageEmailSubscriberModel struct {
	bun.BaseModel `bun:"table:status_page_subscribers"`

	ID           int64      `bun:"id,pk,autoincrement"`
	StatusPageID int64      `bun:"status_page_id,notnull"`
	Email        string     `bun:"email,notnull"`
	Active       bool       `bun:"active,notnull,default:false"`
	ConfirmedAt  *time.Time `bun:"confirmed_at"`
	CreatedAt    time.Time  `bun:"created_at,notnull"`
	UpdatedAt    time.Time  `bun:"updated_at,notnull"`
}

// StatusPageEmailSubscriberFromDomain converts domain → model.
func StatusPageEmailSubscriberFromDomain(s *domain.StatusPageSubscriber) *StatusPageEmailSubscriberModel {
	return &StatusPageEmailSubscriberModel{
		ID:           s.ID,
		StatusPageID: s.StatusPageID,
		Email:        s.Email,
		Active:       s.Active,
		ConfirmedAt:  s.ConfirmedAt,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}

// ToDomain converts model → domain.StatusPageSubscriber.
func (m *StatusPageEmailSubscriberModel) ToDomain() *domain.StatusPageSubscriber {
	return &domain.StatusPageSubscriber{
		ID:           m.ID,
		StatusPageID: m.StatusPageID,
		Email:        m.Email,
		Active:       m.Active,
		ConfirmedAt:  m.ConfirmedAt,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
}

// StatusPageSubscriptionChannelModel maps status_page_subscription_channels.
type StatusPageSubscriptionChannelModel struct {
	bun.BaseModel `bun:"table:status_page_subscription_channels"`

	StatusPageID   int64 `bun:"status_page_id,pk"`
	NotificationID int64 `bun:"notification_id,notnull"`
}

// StatusPageSubscriptionChannelFromDomain converts domain → model.
func StatusPageSubscriptionChannelFromDomain(c *domain.StatusPageSubscriptionChannel) *StatusPageSubscriptionChannelModel {
	return &StatusPageSubscriptionChannelModel{
		StatusPageID:   c.StatusPageID,
		NotificationID: c.NotificationID,
	}
}

// ToDomain converts model → domain.StatusPageSubscriptionChannel.
func (m *StatusPageSubscriptionChannelModel) ToDomain() *domain.StatusPageSubscriptionChannel {
	return &domain.StatusPageSubscriptionChannel{
		StatusPageID:   m.StatusPageID,
		NotificationID: m.NotificationID,
	}
}
