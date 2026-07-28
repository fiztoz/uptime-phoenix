package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type cloneFakeMonitorRepo struct {
	mu      sync.Mutex
	byID    map[int64]*domain.Monitor
	byToken map[string]int64
	nextID  int64
}

func newCloneFakeMonitorRepo() *cloneFakeMonitorRepo {
	return &cloneFakeMonitorRepo{
		byID:    make(map[int64]*domain.Monitor),
		byToken: make(map[string]int64),
	}
}

func (r *cloneFakeMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	m.ID = r.nextID
	r.byID[m.ID] = m
	if m.PushToken != "" {
		r.byToken[m.PushToken] = m.ID
	}
	return nil
}

func (r *cloneFakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return m, nil
}

func (r *cloneFakeMonitorRepo) GetByPushToken(_ context.Context, token string) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byToken[token]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return r.byID[id], nil
}

func (r *cloneFakeMonitorRepo) List(_ context.Context, _ ports.MonitorFilter) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *cloneFakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *cloneFakeMonitorRepo) Update(_ context.Context, _ *domain.Monitor) error { return nil }
func (r *cloneFakeMonitorRepo) Delete(_ context.Context, _ int64) error           { return nil }
func (r *cloneFakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *cloneFakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (r *cloneFakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

func TestMonitorService_Clone_ClearsPushToken(t *testing.T) {
	repo := newCloneFakeMonitorRepo()
	bus := newFakeBus()
	svc := NewMonitorService(repo, bus)

	src := &domain.Monitor{
		UserID:    1,
		Name:      "Push Monitor",
		Type:      "push",
		Active:    true,
		Interval:  60,
		PushToken: "original-token",
		Config:    map[string]any{"push_token": "original-token"},
	}
	if err := svc.Create(context.Background(), src); err != nil {
		t.Fatalf("create: %v", err)
	}

	cloned, err := svc.Clone(context.Background(), src.ID, 1)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if cloned.PushToken == "" || cloned.PushToken == "original-token" {
		t.Fatalf("cloned PushToken = %q; want new unique token", cloned.PushToken)
	}
	if cloned.Config["push_token"] != cloned.PushToken {
		t.Fatalf("config push_token mismatch: %v vs %s", cloned.Config["push_token"], cloned.PushToken)
	}

	byOld, err := svc.GetByPushToken(context.Background(), "original-token")
	if err != nil {
		t.Fatalf("lookup original token: %v", err)
	}
	if byOld.ID != src.ID {
		t.Fatalf("original token should still map to source monitor id=%d, got %d", src.ID, byOld.ID)
	}

	byNew, err := svc.GetByPushToken(context.Background(), cloned.PushToken)
	if err != nil {
		t.Fatalf("lookup cloned token: %v", err)
	}
	if byNew.ID != cloned.ID {
		t.Fatalf("cloned token should map to clone id=%d, got %d", cloned.ID, byNew.ID)
	}
}
