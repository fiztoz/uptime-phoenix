package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestMaintenanceWindowShapeIsValidated pins the maintenance part of #118: a
// window whose end is not after its start, an unknown strategy, or a cron
// window without an expression or duration was saved and then silently never
// activated (IsActive only matches "single" with start<=now<end, or "cron"
// with a non-empty expression).
func TestMaintenanceWindowShapeIsValidated(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		mw   domain.MaintenanceWindow
	}{
		{"end before start", domain.MaintenanceWindow{Title: "w", Strategy: "single", StartDate: start, EndDate: start.Add(-time.Hour)}},
		{"end equals start", domain.MaintenanceWindow{Title: "w", Strategy: "single", StartDate: start, EndDate: start}},
		{"single without dates", domain.MaintenanceWindow{Title: "w", Strategy: "single"}},
		{"unknown strategy", domain.MaintenanceWindow{Title: "w", Strategy: "weekly", StartDate: start, EndDate: start.Add(time.Hour)}},
		{"cron without expression", domain.MaintenanceWindow{Title: "w", Strategy: "cron", Duration: 60}},
		{"cron without duration", domain.MaintenanceWindow{Title: "w", Strategy: "cron", CronExpr: "0 2 * * *"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range []string{"create", "update"} {
				repo := &fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{}}
				svc := NewMaintenanceService(repo, &fakeMaintLinkRepo{}, &fakeCronEval{})
				mw := tc.mw
				var err error
				if op == "create" {
					err = svc.Create(context.Background(), &mw)
				} else {
					mw.ID = 1
					err = svc.Update(context.Background(), &mw)
				}
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("%s = %v; want ErrValidation", op, err)
				}
				if len(repo.windows) != 0 {
					t.Fatalf("%s persisted an invalid window", op)
				}
			}
		})
	}

	t.Run("empty strategy means single, like the column default", func(t *testing.T) {
		repo := &fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{}}
		svc := NewMaintenanceService(repo, &fakeMaintLinkRepo{}, &fakeCronEval{})
		if err := svc.Create(context.Background(), &domain.MaintenanceWindow{Title: "w", StartDate: start, EndDate: start.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if repo.windows[1].Strategy != "single" {
			t.Fatalf("strategy = %q; want single", repo.windows[1].Strategy)
		}
	})
}
