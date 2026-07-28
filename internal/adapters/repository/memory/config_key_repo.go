package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ConfigKeyRepo is a thread-safe in-memory config key store for tests.
type ConfigKeyRepo struct {
	mu     sync.RWMutex
	byID   map[int64]*domain.ConfigKey
	byKey  map[string]int64 // type\0name → id
	byRes  map[string]int64 // type\0resourceID → id
	nextID atomic.Int64
}

// NewConfigKeyRepo returns an empty in-memory config key repository.
func NewConfigKeyRepo() *ConfigKeyRepo {
	return &ConfigKeyRepo{
		byID:  make(map[int64]*domain.ConfigKey),
		byKey: make(map[string]int64),
		byRes: make(map[string]int64),
	}
}

func ckKey(t, name string) string { return t + "\x00" + name }
func ckRes(t string, id int64) string {
	return t + "\x00" + itoa64(id)
}

func itoa64(id int64) string {
	if id == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	n := id
	if n < 0 {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if id < 0 {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Upsert inserts or updates a config key mapping.
func (r *ConfigKeyRepo) Upsert(_ context.Context, k *domain.ConfigKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()

	// Drop any existing row for this key name or this resource id (same type).
	if id, ok := r.byKey[ckKey(k.ResourceType, k.KeyName)]; ok {
		old := r.byID[id]
		delete(r.byRes, ckRes(old.ResourceType, old.ResourceID))
		delete(r.byKey, ckKey(old.ResourceType, old.KeyName))
		delete(r.byID, id)
	}
	if id, ok := r.byRes[ckRes(k.ResourceType, k.ResourceID)]; ok {
		old := r.byID[id]
		delete(r.byRes, ckRes(old.ResourceType, old.ResourceID))
		delete(r.byKey, ckKey(old.ResourceType, old.KeyName))
		delete(r.byID, id)
	}

	nid := r.nextID.Add(1)
	clone := *k
	clone.ID = nid
	if clone.CreatedAt.IsZero() {
		clone.CreatedAt = now
	}
	clone.UpdatedAt = now
	r.byID[nid] = &clone
	r.byKey[ckKey(k.ResourceType, k.KeyName)] = nid
	r.byRes[ckRes(k.ResourceType, k.ResourceID)] = nid
	k.ID = nid
	k.CreatedAt = clone.CreatedAt
	k.UpdatedAt = clone.UpdatedAt
	return nil
}

// GetByKey looks up a mapping by type + key name.
func (r *ConfigKeyRepo) GetByKey(_ context.Context, resourceType, keyName string) (*domain.ConfigKey, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byKey[ckKey(resourceType, keyName)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *r.byID[id]
	return &cp, nil
}

// GetByResource looks up a mapping by type + resource id.
func (r *ConfigKeyRepo) GetByResource(_ context.Context, resourceType string, resourceID int64) (*domain.ConfigKey, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byRes[ckRes(resourceType, resourceID)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *r.byID[id]
	return &cp, nil
}

// ListByType returns every key for one resource type.
func (r *ConfigKeyRepo) ListByType(_ context.Context, resourceType string) ([]*domain.ConfigKey, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.ConfigKey, 0)
	for _, k := range r.byID {
		if k.ResourceType == resourceType {
			cp := *k
			out = append(out, &cp)
		}
	}
	return out, nil
}

// DeleteByKey removes one mapping.
func (r *ConfigKeyRepo) DeleteByKey(_ context.Context, resourceType, keyName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byKey[ckKey(resourceType, keyName)]
	if !ok {
		return nil
	}
	old := r.byID[id]
	delete(r.byRes, ckRes(old.ResourceType, old.ResourceID))
	delete(r.byKey, ckKey(old.ResourceType, old.KeyName))
	delete(r.byID, id)
	return nil
}

// DeleteByResource removes the mapping for a resource id.
func (r *ConfigKeyRepo) DeleteByResource(_ context.Context, resourceType string, resourceID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byRes[ckRes(resourceType, resourceID)]
	if !ok {
		return nil
	}
	old := r.byID[id]
	delete(r.byRes, ckRes(old.ResourceType, old.ResourceID))
	delete(r.byKey, ckKey(old.ResourceType, old.KeyName))
	delete(r.byID, id)
	return nil
}

var _ ports.ConfigKeyRepository = (*ConfigKeyRepo)(nil)
