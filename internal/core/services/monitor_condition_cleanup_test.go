package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestMonitorService_DisablingOneKindDeletesOnlyThatCondition(t *testing.T) {
	monRepo := newCloneFakeMonitorRepo()
	condRepo := newConditionRepoFake()
	bus := newFakeBus()
	svc := NewMonitorService(monRepo, bus)
	svc.SetConditionRepository(condRepo)

	monitor := &domain.Monitor{
		UserID: 1,
		Name:   "db",
		Type:   "database",
		Config: map[string]any{
			"engine":             "mariadb",
			"connection_string":  "x",
			"check_session_pool": true,
			"check_storage":      true,
		},
	}
	if err := svc.Create(context.Background(), monitor); err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = condRepo.Upsert(context.Background(), &domain.MonitorCondition{
		MonitorID: monitor.ID,
		ConditionObservation: domain.ConditionObservation{
			Kind:  domain.MonitorConditionSessionPool,
			State: domain.ConditionStateWarning,
		},
	})
	_ = condRepo.Upsert(context.Background(), &domain.MonitorCondition{
		MonitorID: monitor.ID,
		ConditionObservation: domain.ConditionObservation{
			Kind:  domain.MonitorConditionStorage,
			State: domain.ConditionStateOK,
		},
	})

	monitor.Config["check_storage"] = false
	if err := svc.Update(context.Background(), monitor); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := condRepo.Get(context.Background(), monitor.ID, domain.MonitorConditionStorage); err == nil {
		t.Fatal("storage condition should be deleted")
	}
	if _, err := condRepo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool); err != nil {
		t.Fatalf("session condition should remain: %v", err)
	}
	var deletes int
	for _, event := range bus.events {
		if event.Type != "condition.delete" {
			continue
		}
		deletes++
		payload, ok := event.Payload.(domain.ConditionDelete)
		if !ok || payload.MonitorID != monitor.ID || payload.Kind != domain.MonitorConditionStorage {
			t.Fatalf("delete payload=%#v", event.Payload)
		}
	}
	if deletes != 1 {
		t.Fatalf("delete events=%d, want 1", deletes)
	}
}

func TestMonitorService_ConditionCleanupFailureDoesNotFailUpdate(t *testing.T) {
	monRepo := newCloneFakeMonitorRepo()
	bus := newFakeBus()
	svc := NewMonitorService(monRepo, bus)
	svc.SetConditionRepository(&failingConditionRepo{})

	monitor := &domain.Monitor{
		UserID: 1,
		Name:   "db",
		Type:   "database",
		Config: map[string]any{"engine": "mariadb", "connection_string": "x", "check_storage": true},
	}
	if err := svc.Create(context.Background(), monitor); err != nil {
		t.Fatalf("create: %v", err)
	}
	monitor.Config["check_storage"] = false
	if err := svc.Update(context.Background(), monitor); err != nil {
		t.Fatalf("update should succeed after persist: %v", err)
	}
}

type failingConditionRepo struct{}

func (failingConditionRepo) Upsert(context.Context, *domain.MonitorCondition) error {
	return nil
}
func (failingConditionRepo) Get(context.Context, int64, string) (*domain.MonitorCondition, error) {
	return nil, errors.New("unused")
}
func (failingConditionRepo) ListAll(context.Context) ([]*domain.MonitorCondition, error) {
	return nil, nil
}
func (failingConditionRepo) ListByMonitorIDs(context.Context, []int64) ([]*domain.MonitorCondition, error) {
	return nil, nil
}
func (failingConditionRepo) DeleteKind(context.Context, int64, string) error {
	return errors.New("delete failed")
}
func (failingConditionRepo) DeleteByMonitor(context.Context, int64) error { return nil }
