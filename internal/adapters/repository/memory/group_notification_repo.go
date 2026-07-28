package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// GroupNotificationRepo is a thread-safe in-memory implementation of
// ports.GroupNotificationRepository.
//
// Like the rest of this package it is not a production target: it exists so the
// group-alert service and the HTTP handlers can be exercised without a database.
// It reproduces the behaviors the SQL adapters get from their schema, and it
// reproduces them HONESTLY — a test double that quietly accepted everything would
// hide exactly the bugs these tests are here to catch:
//
//   - Attach is idempotent per (group, notification), because both SQL adapters
//     insert-or-ignore against UNIQUE(group_id, notification_id);
//   - Detach of a pair that was never linked is a no-op, not an error;
//   - Attach FAILS when the notification does not exist, because both SQL adapters
//     have a foreign key on notification_id and neither swallows it.
//
// It resolves notifications through the NotificationRepository it is constructed
// with, mirroring the SQL adapters' JOIN.
type GroupNotificationRepo struct {
	mu     sync.RWMutex
	links  map[int64]*domain.GroupNotification
	nextID atomic.Int64

	notifs ports.NotificationRepository
}

// NewGroupNotificationRepo returns an empty in-memory group-notification
// repository. notifs backs ListNotificationsByGroup and the notification-exists
// check on Attach; it is required (a nil one would let Attach succeed against a
// notification that does not exist, which the SQL adapters reject).
func NewGroupNotificationRepo(notifs ports.NotificationRepository) *GroupNotificationRepo {
	return &GroupNotificationRepo{
		links:  make(map[int64]*domain.GroupNotification),
		notifs: notifs,
	}
}

// Attach links a notification to a group, ignoring a repeat of an existing pair.
func (r *GroupNotificationRepo) Attach(ctx context.Context, groupID, notificationID int64) error {
	if r.notifs == nil {
		return fmt.Errorf("memory group notification repo: no notification repository wired")
	}
	// Stand in for the notification_id foreign key: an attach naming a
	// notification that does not exist must fail, not silently succeed.
	if _, err := r.notifs.GetByID(ctx, notificationID); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.links {
		if existing.GroupID == groupID && existing.NotificationID == notificationID {
			return nil // already attached — idempotent, same as the SQL adapters
		}
	}
	id := r.nextID.Add(1)
	r.links[id] = &domain.GroupNotification{
		ID:             id,
		GroupID:        groupID,
		NotificationID: notificationID,
	}
	return nil
}

// Detach removes the link. Detaching an unlinked pair is not an error.
func (r *GroupNotificationRepo) Detach(_ context.Context, groupID, notificationID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, l := range r.links {
		if l.GroupID == groupID && l.NotificationID == notificationID {
			delete(r.links, id)
		}
	}
	return nil
}

// ListByGroup returns the link rows for one group, oldest first.
func (r *GroupNotificationRepo) ListByGroup(_ context.Context, groupID int64) ([]*domain.GroupNotification, error) {
	return r.filter(func(l *domain.GroupNotification) bool { return l.GroupID == groupID }), nil
}

// ListByNotification returns the link rows for one notification, across groups.
func (r *GroupNotificationRepo) ListByNotification(_ context.Context, notificationID int64) ([]*domain.GroupNotification, error) {
	return r.filter(func(l *domain.GroupNotification) bool { return l.NotificationID == notificationID }), nil
}

// ListNotificationsByGroup resolves the links to full notifications, ordered by
// notification id — the same order the SQL adapters' JOIN produces.
func (r *GroupNotificationRepo) ListNotificationsByGroup(ctx context.Context, groupID int64) ([]*domain.Notification, error) {
	if r.notifs == nil {
		return nil, fmt.Errorf("memory group notification repo: no notification repository wired")
	}
	links, err := r.ListByGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Notification, 0, len(links))
	for _, l := range links {
		n, err := r.notifs.GetByID(ctx, l.NotificationID)
		if err != nil {
			// The SQL adapters cannot produce this: the FK cascades, so a deleted
			// notification takes its links with it. Skip rather than fail so a test
			// fake that deletes out from under a link still behaves sanely.
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *GroupNotificationRepo) filter(keep func(*domain.GroupNotification) bool) []*domain.GroupNotification {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.GroupNotification, 0, len(r.links))
	for _, l := range r.links {
		if keep(l) {
			clone := *l
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

var _ ports.GroupNotificationRepository = (*GroupNotificationRepo)(nil)
