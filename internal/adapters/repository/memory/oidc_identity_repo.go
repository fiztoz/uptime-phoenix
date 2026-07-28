package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// OIDCIdentityRepo is a thread-safe in-memory OIDC identity store for tests.
type OIDCIdentityRepo struct {
	mu     sync.RWMutex
	byID   map[int64]*domain.OIDCIdentity
	byPair map[string]int64 // issuer\0subject → id
	nextID atomic.Int64
}

// NewOIDCIdentityRepo returns an empty in-memory OIDC identity repository.
func NewOIDCIdentityRepo() *OIDCIdentityRepo {
	return &OIDCIdentityRepo{
		byID:   make(map[int64]*domain.OIDCIdentity),
		byPair: make(map[string]int64),
	}
}

func pairKey(issuer, subject string) string {
	return issuer + "\x00" + subject
}

// Create inserts a new identity link.
func (r *OIDCIdentityRepo) Create(_ context.Context, id *domain.OIDCIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := pairKey(id.Issuer, id.Subject)
	if _, exists := r.byPair[key]; exists {
		return ports.ErrConflict
	}
	for _, existing := range r.byID {
		if existing.UserID == id.UserID && existing.Issuer == id.Issuer {
			return ports.ErrConflict
		}
	}
	nid := r.nextID.Add(1)
	clone := *id
	clone.ID = nid
	now := time.Now().UTC()
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	r.byID[nid] = &clone
	r.byPair[key] = nid
	id.ID = nid
	id.CreatedAt = clone.CreatedAt
	id.UpdatedAt = clone.UpdatedAt
	return nil
}

// GetByIssuerSubject looks up a link by the immutable OIDC pair.
func (r *OIDCIdentityRepo) GetByIssuerSubject(_ context.Context, issuer, subject string) (*domain.OIDCIdentity, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byPair[pairKey(issuer, subject)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	clone := *r.byID[id]
	return &clone, nil
}

// ListByUser returns every OIDC identity linked to a Phoenix user.
func (r *OIDCIdentityRepo) ListByUser(_ context.Context, userID int64) ([]*domain.OIDCIdentity, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.OIDCIdentity, 0)
	for _, id := range r.byID {
		if id.UserID == userID {
			clone := *id
			out = append(out, &clone)
		}
	}
	return out, nil
}

// TouchLogin updates email and last_login_at after a successful login.
func (r *OIDCIdentityRepo) TouchLogin(_ context.Context, id int64, email string, lastLoginAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.byID[id]
	if !ok {
		return ports.ErrNotFound
	}
	row.Email = email
	ts := lastLoginAt.UTC()
	row.LastLoginAt = &ts
	row.UpdatedAt = time.Now().UTC()
	return nil
}

// Delete removes one identity row by primary key.
func (r *OIDCIdentityRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.byID[id]
	if !ok {
		return ports.ErrNotFound
	}
	delete(r.byPair, pairKey(row.Issuer, row.Subject))
	delete(r.byID, id)
	return nil
}

var _ ports.OIDCIdentityRepository = (*OIDCIdentityRepo)(nil)
