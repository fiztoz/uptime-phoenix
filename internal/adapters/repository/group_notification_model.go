package repository

import (
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// GroupNotificationModel maps the group_notifications table — the many-to-many
// link between a monitor group (folder) and a notification.
//
// It deliberately mirrors MonitorNotificationModel rather than reusing it: the
// two links mean different things. A monitor link alerts on the monitor's own
// heartbeat transitions; a group link alerts on the group's DERIVED status (the
// rollup its condition produces) and never inherits down to the monitors inside.
type GroupNotificationModel struct {
	bun.BaseModel `bun:"table:group_notifications"`

	ID             int64 `bun:"id,pk,autoincrement"`
	GroupID        int64 `bun:"group_id,notnull"`
	NotificationID int64 `bun:"notification_id,notnull"`
}

// ToDomainGroupNotification converts a GroupNotificationModel to a
// domain.GroupNotification.
func (m *GroupNotificationModel) ToDomainGroupNotification() *domain.GroupNotification {
	return &domain.GroupNotification{
		ID:             m.ID,
		GroupID:        m.GroupID,
		NotificationID: m.NotificationID,
	}
}

// GroupNotificationFromDomain converts a domain.GroupNotification to a
// GroupNotificationModel.
func GroupNotificationFromDomain(gn *domain.GroupNotification) *GroupNotificationModel {
	return &GroupNotificationModel{
		ID:             gn.ID,
		GroupID:        gn.GroupID,
		NotificationID: gn.NotificationID,
	}
}
