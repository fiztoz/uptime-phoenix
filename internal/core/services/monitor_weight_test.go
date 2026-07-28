package services

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestMonitorService_Create_DefaultsWeight(t *testing.T) {
	monRepo := newCloneFakeMonitorRepo()
	svc := NewMonitorService(monRepo, newFakeBus())
	ctx := context.Background()

	m := &domain.Monitor{UserID: 1, Name: "API", Type: "http", Interval: 60, Active: true}
	if err := svc.Create(ctx, m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if m.Weight != 2000 {
		t.Fatalf("Weight after Create with zero = %d; want default 2000", m.Weight)
	}

	explicit := &domain.Monitor{
		UserID: 1, Name: "Priority", Type: "http", Interval: 60, Active: true, Weight: 100,
	}
	if err := svc.Create(ctx, explicit); err != nil {
		t.Fatalf("Create explicit: %v", err)
	}
	if explicit.Weight != 100 {
		t.Fatalf("Weight after Create with 100 = %d; want 100", explicit.Weight)
	}
}
