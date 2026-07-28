package sqlite

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestMigration018_StatusPageSLATargetRoundTrip(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	target := 99.95
	page := &domain.StatusPage{
		Slug:      "sla-round-trip",
		Title:     "SLA round trip",
		Theme:     "auto",
		Published: true,
		SLATarget: &target,
	}

	if err := repo.StatusPageRepo.Create(ctx, page); err != nil {
		t.Fatalf("create status page: %v", err)
	}
	got, err := repo.StatusPageRepo.GetByID(ctx, page.ID)
	if err != nil {
		t.Fatalf("get status page: %v", err)
	}
	if got.SLATarget == nil || *got.SLATarget != target {
		t.Fatalf("SLA target = %v; want %v", got.SLATarget, target)
	}

	got.SLATarget = nil
	if err := repo.StatusPageRepo.Update(ctx, got); err != nil {
		t.Fatalf("clear SLA target: %v", err)
	}
	cleared, err := repo.StatusPageRepo.GetByID(ctx, page.ID)
	if err != nil {
		t.Fatalf("get cleared status page: %v", err)
	}
	if cleared.SLATarget != nil {
		t.Fatalf("cleared SLA target = %v; want nil", *cleared.SLATarget)
	}
}
