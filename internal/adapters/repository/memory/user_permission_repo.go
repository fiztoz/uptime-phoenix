package memory

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// UserPermissionRepo is a thread-safe in-memory implementation of
// ports.UserPermissionRepository.
//
// Like the rest of this package it is not a production target: it exists so the
// access service and the HTTP handlers can be exercised without a database. It
// reproduces the two behaviors the SQL adapters get from their UNIQUE indexes:
// Grant is idempotent per (user, resource), and a revoke of something that was
// never granted is a no-op rather than an error.
type UserPermissionRepo struct {
	mu     sync.RWMutex
	perms  map[int64]*domain.UserPermission
	nextID atomic.Int64
}

// NewUserPermissionRepo returns an empty in-memory RBAC grant repository.
func NewUserPermissionRepo() *UserPermissionRepo {
	return &UserPermissionRepo{perms: make(map[int64]*domain.UserPermission)}
}

// Grant inserts a view grant, upserting a repeat of an existing (user, resource).
func (r *UserPermissionRepo) Grant(_ context.Context, p *domain.UserPermission) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.perms {
		if existing.UserID != p.UserID {
			continue
		}
		if sameTarget(existing.MonitorID, p.MonitorID) && sameTarget(existing.GroupID, p.GroupID) {
			// Already granted. Mirror the SQL adapters' ON CONFLICT DO UPDATE
			// exactly: the row stays put and keeps its id and created_at, but the
			// reach is overwritten. Returning early here without the assignment —
			// which is what this did when a grant was pure identity — would make
			// every test using this fake pass while the real adapters were the
			// only ones honoring a changed include_descendants, or vice versa.
			existing.IncludeDescendants = p.IncludeDescendants
			p.ID = existing.ID
			p.CreatedAt = existing.CreatedAt
			return nil
		}
	}
	id := r.nextID.Add(1)
	clone := *p
	clone.ID = id
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now().UTC()
	}
	r.perms[id] = &clone
	p.ID = id
	p.CreatedAt = clone.CreatedAt
	return nil
}

// RevokeMonitor drops a user's direct grant on one monitor.
func (r *UserPermissionRepo) RevokeMonitor(_ context.Context, userID, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.perms {
		if p.UserID == userID && p.MonitorID != nil && *p.MonitorID == monitorID {
			delete(r.perms, id)
		}
	}
	return nil
}

// RevokeGroup drops a user's grant on one group.
func (r *UserPermissionRepo) RevokeGroup(_ context.Context, userID, groupID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.perms {
		if p.UserID == userID && p.GroupID != nil && *p.GroupID == groupID {
			delete(r.perms, id)
		}
	}
	return nil
}

// RevokeAll drops every grant held by one user.
func (r *UserPermissionRepo) RevokeAll(_ context.Context, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.perms {
		if p.UserID == userID {
			delete(r.perms, id)
		}
	}
	return nil
}

// ListByUser returns every grant held by one user, oldest first.
func (r *UserPermissionRepo) ListByUser(_ context.Context, userID int64) ([]*domain.UserPermission, error) {
	return r.filter(func(p *domain.UserPermission) bool { return p.UserID == userID }), nil
}

// ListByMonitor returns every grant pointing at one monitor, across all users.
func (r *UserPermissionRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.UserPermission, error) {
	return r.filter(func(p *domain.UserPermission) bool {
		return p.MonitorID != nil && *p.MonitorID == monitorID
	}), nil
}

// ListByGroup returns every grant pointing at one group, across all users.
func (r *UserPermissionRepo) ListByGroup(_ context.Context, groupID int64) ([]*domain.UserPermission, error) {
	return r.filter(func(p *domain.UserPermission) bool {
		return p.GroupID != nil && *p.GroupID == groupID
	}), nil
}

func (r *UserPermissionRepo) filter(keep func(*domain.UserPermission) bool) []*domain.UserPermission {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.UserPermission, 0, len(r.perms))
	for _, p := range r.perms {
		if keep(p) {
			clone := *p
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sameTarget compares two nullable resource ids: both nil, or both set to the
// same value.
func sameTarget(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

var _ ports.UserPermissionRepository = (*UserPermissionRepo)(nil)
