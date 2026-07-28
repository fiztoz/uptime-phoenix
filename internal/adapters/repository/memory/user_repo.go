// Package memory provides in-memory implementations of the repository ports.
//
// It exists so the app can boot and serve auth traffic before the SQL-backed
// adapters (sqlite / mariadb) are wired in. It is NOT a production target
// and is excluded from the Helm chart. Data is lost on restart.
package memory

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// UserRepo is a thread-safe in-memory implementation of ports.UserRepository.
//
// The atomic ID generator is the only state that survives a "restart of the
// struct" — the maps themselves are recreated on every New call, so there is
// no risk of test contamination from shared globals.
type UserRepo struct {
	mu     sync.RWMutex
	users  map[int64]*domain.User
	byName map[string]int64
	nextID atomic.Int64
}

// NewUserRepo returns an empty in-memory user repository.
func NewUserRepo() *UserRepo {
	return &UserRepo{
		users:  make(map[int64]*domain.User),
		byName: make(map[string]int64),
	}
}

// Create inserts a new user and assigns it a monotonically increasing ID.
// It returns ports.ErrConflict if the username is already taken.
func (r *UserRepo) Create(_ context.Context, u *domain.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byName[u.Username]; exists {
		return ports.ErrConflict
	}

	id := r.nextID.Add(1)
	clone := *u
	clone.ID = id
	now := time.Now().UTC()
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	r.users[id] = &clone
	r.byName[u.Username] = id
	u.ID = id
	u.CreatedAt = clone.CreatedAt
	u.UpdatedAt = clone.UpdatedAt
	return nil
}

// GetByID returns a copy of the user with the given ID.
func (r *UserRepo) GetByID(_ context.Context, id int64) (*domain.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.users[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	clone := *u
	return &clone, nil
}

// GetByUsername returns a copy of the user with the given username.
func (r *UserRepo) GetByUsername(_ context.Context, username string) (*domain.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byName[username]
	if !ok {
		return nil, ports.ErrNotFound
	}
	clone := *r.users[id]
	return &clone, nil
}

// Update overwrites the user identified by u.ID with the supplied fields.
// The username uniqueness index is updated atomically when the username
// changes so the secondary map cannot drift.
func (r *UserRepo) Update(_ context.Context, u *domain.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.users[u.ID]
	if !ok {
		return ports.ErrNotFound
	}
	if u.Username != existing.Username {
		if _, taken := r.byName[u.Username]; taken {
			return ports.ErrConflict
		}
		delete(r.byName, existing.Username)
		r.byName[u.Username] = u.ID
	}
	clone := *u
	clone.UpdatedAt = time.Now().UTC()
	r.users[u.ID] = &clone
	return nil
}

// Delete removes the user and its secondary index entry.
func (r *UserRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[id]
	if !ok {
		return ports.ErrNotFound
	}
	delete(r.byName, u.Username)
	delete(r.users, id)
	return nil
}

// Count returns the total number of users.
func (r *UserRepo) Count(_ context.Context) (int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return int64(len(r.users)), nil
}

// List returns every user sorted by ID ascending. Intended for tests and
// admin tooling; there is no pagination because this repo is not for
// production.
func (r *UserRepo) List(_ context.Context) ([]*domain.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.User, 0, len(r.users))
	for _, u := range r.users {
		clone := *u
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// APIKeyRepo is a thread-safe in-memory implementation of ports.APIKeyRepository.
type APIKeyRepo struct {
	mu     sync.RWMutex
	keys   map[int64]*domain.APIKey
	byHash map[string]int64
	nextID atomic.Int64
}

// NewAPIKeyRepo returns an empty in-memory API key repository.
func NewAPIKeyRepo() *APIKeyRepo {
	return &APIKeyRepo{
		keys:   make(map[int64]*domain.APIKey),
		byHash: make(map[string]int64),
	}
}

// Create inserts a new API key, returning ports.ErrConflict if the hash
// is already known. The hash is the natural key for lookups and must be
// unique.
func (r *APIKeyRepo) Create(_ context.Context, ak *domain.APIKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byHash[ak.KeyHash]; exists {
		return ports.ErrConflict
	}
	id := r.nextID.Add(1)
	clone := *ak
	clone.ID = id
	r.keys[id] = &clone
	r.byHash[ak.KeyHash] = id
	ak.ID = id
	return nil
}

// GetByHash returns the API key whose stored hash matches.
func (r *APIKeyRepo) GetByHash(_ context.Context, hash string) (*domain.APIKey, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byHash[hash]
	if !ok {
		return nil, ports.ErrNotFound
	}
	clone := *r.keys[id]
	return &clone, nil
}

// List returns all API keys for the given user.
func (r *APIKeyRepo) List(_ context.Context, userID int64) ([]*domain.APIKey, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.APIKey, 0)
	for _, k := range r.keys {
		if k.UserID == userID {
			clone := *k
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update overwrites an existing key by ID.
func (r *APIKeyRepo) Update(_ context.Context, ak *domain.APIKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.keys[ak.ID]; !ok {
		return ports.ErrNotFound
	}
	clone := *ak
	r.keys[ak.ID] = &clone
	return nil
}

// Delete removes a key and its hash index entry.
func (r *APIKeyRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.keys[id]
	if !ok {
		return ports.ErrNotFound
	}
	delete(r.byHash, k.KeyHash)
	delete(r.keys, id)
	return nil
}

// WebAuthnCredentialRepo is a thread-safe in-memory implementation of
// ports.WebAuthnCredentialRepository.
type WebAuthnCredentialRepo struct {
	mu     sync.RWMutex
	creds  map[int64]*domain.WebAuthnCredential
	nextID atomic.Int64
}

// NewWebAuthnCredentialRepo returns an empty in-memory passkey credential repo.
func NewWebAuthnCredentialRepo() *WebAuthnCredentialRepo {
	return &WebAuthnCredentialRepo{creds: make(map[int64]*domain.WebAuthnCredential)}
}

// Create inserts a new credential and assigns it a monotonic ID.
func (r *WebAuthnCredentialRepo) Create(_ context.Context, c *domain.WebAuthnCredential) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.nextID.Add(1)
	clone := *c
	clone.ID = id
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = time.Now().UTC()
	}
	r.creds[id] = &clone
	c.ID = id
	c.CreatedAt = clone.CreatedAt
	return nil
}

// ListByUser returns all credentials owned by a user, oldest first.
func (r *WebAuthnCredentialRepo) ListByUser(_ context.Context, userID int64) ([]*domain.WebAuthnCredential, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.WebAuthnCredential, 0)
	for _, c := range r.creds {
		if c.UserID == userID {
			clone := *c
			out = append(out, &clone)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetByCredentialID looks a credential up by its raw credential ID bytes.
func (r *WebAuthnCredentialRepo) GetByCredentialID(_ context.Context, credentialID []byte) (*domain.WebAuthnCredential, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.creds {
		if bytes.Equal(c.CredentialID, credentialID) {
			clone := *c
			return &clone, nil
		}
	}
	return nil, ports.ErrNotFound
}

// UpdateUsage persists mutable authenticator state and bumps last_used_at.
func (r *WebAuthnCredentialRepo) UpdateUsage(_ context.Context, id int64, signCount uint32, flags byte, cloneWarning bool, attachment string, lastUsedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[id]
	if !ok {
		return ports.ErrNotFound
	}
	c.SignCount = signCount
	c.Flags = flags
	c.FlagsKnown = true
	c.CloneWarning = cloneWarning
	c.Attachment = attachment
	lu := lastUsedAt
	c.LastUsedAt = &lu
	return nil
}

// Delete removes a credential by primary key, scoped to a user.
func (r *WebAuthnCredentialRepo) Delete(_ context.Context, id, userID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.creds[id]
	if !ok || c.UserID != userID {
		return ports.ErrNotFound
	}
	delete(r.creds, id)
	return nil
}

// Ensure the in-memory adapters satisfy the ports at compile time.
var (
	_ ports.UserRepository               = (*UserRepo)(nil)
	_ ports.APIKeyRepository             = (*APIKeyRepo)(nil)
	_ ports.WebAuthnCredentialRepository = (*WebAuthnCredentialRepo)(nil)
)
