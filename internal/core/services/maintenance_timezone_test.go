package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// fakeCronEval records the location it was given and returns a fixed answer.
type fakeCronEval struct {
	active bool
	loc    *time.Location
	expr   string
	dur    int
}

func (f *fakeCronEval) IsWindowActive(cronExpr string, durationMinutes int, _ time.Time, loc *time.Location) bool {
	f.expr = cronExpr
	f.dur = durationMinutes
	f.loc = loc
	return f.active
}

type fakeMaintRepo struct {
	windows map[int64]*domain.MaintenanceWindow
}

func (r *fakeMaintRepo) Create(_ context.Context, mw *domain.MaintenanceWindow) error {
	if mw.ID == 0 {
		mw.ID = int64(len(r.windows) + 1)
	}
	cp := *mw
	r.windows[mw.ID] = &cp
	return nil
}
func (r *fakeMaintRepo) GetByID(_ context.Context, id int64) (*domain.MaintenanceWindow, error) {
	w, ok := r.windows[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *w
	return &cp, nil
}
func (r *fakeMaintRepo) List(context.Context, int64) ([]*domain.MaintenanceWindow, error) {
	return nil, nil
}
func (r *fakeMaintRepo) ListAll(context.Context) ([]*domain.MaintenanceWindow, error) {
	return nil, nil
}
func (r *fakeMaintRepo) Update(_ context.Context, mw *domain.MaintenanceWindow) error {
	cp := *mw
	r.windows[mw.ID] = &cp
	return nil
}
func (r *fakeMaintRepo) Delete(context.Context, int64) error { return nil }

type fakeMaintLinkRepo struct {
	byMonitor map[int64][]*domain.MaintenanceWindow
	byMaint   map[int64][]int64
}

func (r *fakeMaintLinkRepo) Assign(context.Context, int64, int64) error { return nil }
func (r *fakeMaintLinkRepo) Remove(context.Context, int64, int64) error { return nil }
func (r *fakeMaintLinkRepo) ListByMaintenance(_ context.Context, maintenanceID int64) ([]int64, error) {
	return r.byMaint[maintenanceID], nil
}
func (r *fakeMaintLinkRepo) ListByMonitor(_ context.Context, monitorID int64) ([]*domain.MaintenanceWindow, error) {
	return r.byMonitor[monitorID], nil
}

func TestMaintenanceCreate_ValidatesTimezone(t *testing.T) {
	repo := &fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{}}
	svc := NewMaintenanceService(repo, &fakeMaintLinkRepo{}, &fakeCronEval{})

	err := svc.Create(context.Background(), &domain.MaintenanceWindow{
		Title:    "win",
		Strategy: "cron",
		Timezone: "Not/AZone",
		CronExpr: "0 2 * * *",
		Duration: 60,
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Create invalid tz = %v, want ErrValidation", err)
	}

	err = svc.Create(context.Background(), &domain.MaintenanceWindow{
		Title:    "win",
		Strategy: "cron",
		Timezone: "Asia/Bangkok",
		CronExpr: "0 2 * * *",
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("Create Asia/Bangkok: %v", err)
	}
	got := repo.windows[1]
	if got.Timezone != "Asia/Bangkok" {
		t.Errorf("Timezone = %q, want Asia/Bangkok", got.Timezone)
	}
}

func TestMaintenanceCreate_EmptyTimezoneDefaultsUTC(t *testing.T) {
	repo := &fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{}}
	svc := NewMaintenanceService(repo, &fakeMaintLinkRepo{}, &fakeCronEval{})

	err := svc.Create(context.Background(), &domain.MaintenanceWindow{
		Title:    "legacy",
		Strategy: "single",
		// Timezone empty
		StartDate: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		EndDate:   time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if repo.windows[1].Timezone != "UTC" {
		t.Errorf("empty timezone = %q, want UTC", repo.windows[1].Timezone)
	}
}

func TestIsActive_SingleHalfOpen(t *testing.T) {
	now := time.Now().UTC()
	start := now.Add(-time.Minute)
	end := now.Add(time.Minute)
	mw := &domain.MaintenanceWindow{
		ID:        1,
		Active:    true,
		Strategy:  "single",
		StartDate: start,
		EndDate:   end,
		Timezone:  "UTC",
	}
	link := &fakeMaintLinkRepo{byMonitor: map[int64][]*domain.MaintenanceWindow{10: {mw}}}
	svc := NewMaintenanceService(&fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{1: mw}}, link, &fakeCronEval{})

	active, err := svc.IsActive(context.Background(), 10)
	if err != nil || !active {
		t.Fatalf("IsActive mid-window = (%v, %v), want true", active, err)
	}

	// Exclusive end: set end to just before now so window has ended.
	mw.EndDate = now.Add(-time.Second)
	active, err = svc.IsActive(context.Background(), 10)
	if err != nil || active {
		t.Fatalf("IsActive after end = (%v, %v), want false", active, err)
	}

	// Inclusive start: start == now should be active.
	mw.StartDate = time.Now().UTC().Add(-time.Millisecond)
	mw.EndDate = time.Now().UTC().Add(time.Hour)
	active, err = svc.IsActive(context.Background(), 10)
	if err != nil || !active {
		t.Fatalf("IsActive at start = (%v, %v), want true", active, err)
	}
}

func TestIsActive_CronPassesLocation(t *testing.T) {
	mw := &domain.MaintenanceWindow{
		ID:       1,
		Active:   true,
		Strategy: "cron",
		CronExpr: "0 2 * * *",
		Duration: 60,
		Timezone: "Asia/Bangkok",
	}
	link := &fakeMaintLinkRepo{byMonitor: map[int64][]*domain.MaintenanceWindow{10: {mw}}}
	eval := &fakeCronEval{active: true}
	svc := NewMaintenanceService(&fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{1: mw}}, link, eval)

	active, err := svc.IsActive(context.Background(), 10)
	if err != nil || !active {
		t.Fatalf("IsActive cron = (%v, %v), want true", active, err)
	}
	if eval.loc == nil || eval.loc.String() != "Asia/Bangkok" {
		t.Errorf("cron loc = %v, want Asia/Bangkok", eval.loc)
	}
}

func TestIsActive_LegacyEmptyTimezoneUsesUTC(t *testing.T) {
	mw := &domain.MaintenanceWindow{
		ID:       1,
		Active:   true,
		Strategy: "cron",
		CronExpr: "0 2 * * *",
		Duration: 60,
		Timezone: "", // pre-013 legacy
	}
	link := &fakeMaintLinkRepo{byMonitor: map[int64][]*domain.MaintenanceWindow{10: {mw}}}
	eval := &fakeCronEval{active: false}
	svc := NewMaintenanceService(&fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{1: mw}}, link, eval)

	_, _ = svc.IsActive(context.Background(), 10)
	if eval.loc == nil || eval.loc.String() != "UTC" {
		t.Errorf("legacy empty tz loc = %v, want UTC", eval.loc)
	}
}

func TestNotifyScheduled_SkipsZeroMonitors(t *testing.T) {
	called := 0
	svc := NewMaintenanceService(&fakeMaintRepo{windows: map[int64]*domain.MaintenanceWindow{}}, &fakeMaintLinkRepo{}, &fakeCronEval{})
	svc.SetAnnouncementNotifier(announceFunc(func(context.Context, *domain.MaintenanceWindow, []int64) error {
		called++
		return nil
	}))
	svc.NotifyScheduled(context.Background(), &domain.MaintenanceWindow{ID: 1}, nil)
	if called != 0 {
		t.Fatalf("zero monitors still announced: %d", called)
	}
	svc.NotifyScheduled(context.Background(), &domain.MaintenanceWindow{ID: 1}, []int64{1, 2})
	if called != 1 {
		t.Fatalf("announce count = %d, want 1", called)
	}
}

type announceFunc func(context.Context, *domain.MaintenanceWindow, []int64) error

func (f announceFunc) NotifyMaintenanceScheduled(ctx context.Context, w *domain.MaintenanceWindow, ids []int64) error {
	return f(ctx, w, ids)
}
