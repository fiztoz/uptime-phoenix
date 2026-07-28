package services

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// maintenanceAnnouncementNotifier is implemented by Track B's subscription
// service. After a successful create/reschedule and committed monitor links,
// MaintenanceService invokes it best-effort. Optional — nil means no fan-out.
type maintenanceAnnouncementNotifier interface {
	NotifyMaintenanceScheduled(
		ctx context.Context,
		window *domain.MaintenanceWindow,
		monitorIDs []int64,
	) error
}

// MaintenanceService handles maintenance window CRUD and activation.
type MaintenanceService struct {
	repo     ports.MaintenanceRepository
	linkRepo ports.MaintenanceWindowMonitorRepository
	cronEval ports.CronEvaluator
	announce maintenanceAnnouncementNotifier
}

// NewMaintenanceService creates a new MaintenanceService.
func NewMaintenanceService(repo ports.MaintenanceRepository, linkRepo ports.MaintenanceWindowMonitorRepository, cronEval ports.CronEvaluator) *MaintenanceService {
	return &MaintenanceService{repo: repo, linkRepo: linkRepo, cronEval: cronEval}
}

// SetAnnouncementNotifier wires best-effort fan-out after create/reschedule.
// Track B's status-page subscription service satisfies this interface.
func (s *MaintenanceService) SetAnnouncementNotifier(n maintenanceAnnouncementNotifier) {
	s.announce = n
}

// Create creates a new maintenance window after validating timezone.
func (s *MaintenanceService) Create(ctx context.Context, mw *domain.MaintenanceWindow) error {
	if err := normalizeAndValidateWindow(mw); err != nil {
		return err
	}
	return s.repo.Create(ctx, mw)
}

// normalizeAndValidateWindow forces UTC bounds, defaults empty timezone to UTC,
// and rejects invalid IANA names with domain.ErrValidation.
func normalizeAndValidateWindow(mw *domain.MaintenanceWindow) error {
	normalizeWindowDates(mw)
	tz := mw.Timezone
	if tz == "" {
		tz = "UTC"
		mw.Timezone = tz
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("%w: invalid timezone %q", domain.ErrValidation, mw.Timezone)
	}
	return nil
}

// normalizeWindowDates forces the window bounds to UTC before they are persisted.
//
// The web form already sends UTC (it does `new Date(local).toISOString()`), but an
// API-key script or an imported backup can hand us an offset-bearing timestamp, and
// SQLite stores whatever wall-clock the value carries. Mixing zones in the column
// would break the moment anything filters on it in SQL. See AGENTS.md rule 6.
func normalizeWindowDates(mw *domain.MaintenanceWindow) {
	if !mw.StartDate.IsZero() {
		mw.StartDate = mw.StartDate.UTC()
	}
	if !mw.EndDate.IsZero() {
		mw.EndDate = mw.EndDate.UTC()
	}
}

// GetByID retrieves a maintenance window by its ID.
func (s *MaintenanceService) GetByID(ctx context.Context, id int64) (*domain.MaintenanceWindow, error) {
	return s.repo.GetByID(ctx, id)
}

// List retrieves all maintenance windows for a user.
func (s *MaintenanceService) List(ctx context.Context, userID int64) ([]*domain.MaintenanceWindow, error) {
	return s.repo.List(ctx, userID)
}

// ListAll retrieves every maintenance window in the install. Callers must have
// checked that the principal is an admin or holds the can_manage_maintenance
// capability — this method performs no authorization of its own.
func (s *MaintenanceService) ListAll(ctx context.Context) ([]*domain.MaintenanceWindow, error) {
	return s.repo.ListAll(ctx)
}

// ListForMonitors returns the maintenance windows covering any of the given
// monitors, deduplicated and ordered by id.
//
// This is the read-only view a non-admin WITHOUT the can_manage_maintenance
// capability gets: the windows that affect the monitors they can see, and nothing
// else. Pass the caller's visible-monitor set — an empty set yields an empty list.
func (s *MaintenanceService) ListForMonitors(ctx context.Context, monitorIDs []int64) ([]*domain.MaintenanceWindow, error) {
	byID := make(map[int64]*domain.MaintenanceWindow)
	for _, monitorID := range monitorIDs {
		windows, err := s.linkRepo.ListByMonitor(ctx, monitorID)
		if err != nil {
			return nil, err
		}
		for _, w := range windows {
			byID[w.ID] = w
		}
	}
	out := make([]*domain.MaintenanceWindow, 0, len(byID))
	for _, w := range byID {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update updates a maintenance window after validating timezone.
func (s *MaintenanceService) Update(ctx context.Context, mw *domain.MaintenanceWindow) error {
	if err := normalizeAndValidateWindow(mw); err != nil {
		return err
	}
	return s.repo.Update(ctx, mw)
}

// Delete deletes a maintenance window by its ID.
func (s *MaintenanceService) Delete(ctx context.Context, id int64) error {
	return s.repo.Delete(ctx, id)
}

// ListMonitorIDs returns the IDs of the monitors covered by a maintenance window.
func (s *MaintenanceService) ListMonitorIDs(ctx context.Context, maintenanceID int64) ([]int64, error) {
	return s.linkRepo.ListByMaintenance(ctx, maintenanceID)
}

// AssignMonitor puts a single monitor under a maintenance window.
func (s *MaintenanceService) AssignMonitor(ctx context.Context, maintenanceID, monitorID int64) error {
	return s.linkRepo.Assign(ctx, maintenanceID, monitorID)
}

// UnassignMonitor removes a single monitor from a maintenance window.
func (s *MaintenanceService) UnassignMonitor(ctx context.Context, maintenanceID, monitorID int64) error {
	return s.linkRepo.Remove(ctx, maintenanceID, monitorID)
}

// SetMonitors makes the window's monitor set exactly monitorIDs, adding and
// removing links as needed. Callers must have already checked that the window
// and every monitor belong to the requesting user.
//
// A window with no monitors suppresses nothing — IsActive is per-monitor and
// only consults windows linked to that monitor.
func (s *MaintenanceService) SetMonitors(ctx context.Context, maintenanceID int64, monitorIDs []int64) error {
	current, err := s.linkRepo.ListByMaintenance(ctx, maintenanceID)
	if err != nil {
		return err
	}

	desired := make(map[int64]struct{}, len(monitorIDs))
	for _, id := range monitorIDs {
		desired[id] = struct{}{}
	}
	existing := make(map[int64]struct{}, len(current))
	for _, id := range current {
		existing[id] = struct{}{}
	}

	for id := range desired {
		if _, ok := existing[id]; !ok {
			if err := s.linkRepo.Assign(ctx, maintenanceID, id); err != nil {
				return err
			}
		}
	}
	for id := range existing {
		if _, ok := desired[id]; !ok {
			if err := s.linkRepo.Remove(ctx, maintenanceID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// NotifyScheduled invokes the optional announcement notifier after create or
// reschedule, once monitor links are committed. Delivery failures are logged
// and never fail the CRUD path (handoff §3.6 / §3.7).
func (s *MaintenanceService) NotifyScheduled(ctx context.Context, window *domain.MaintenanceWindow, monitorIDs []int64) {
	if s.announce == nil || window == nil {
		return
	}
	// Zero-monitor windows suppress and email nothing.
	if len(monitorIDs) == 0 {
		return
	}
	if err := s.announce.NotifyMaintenanceScheduled(ctx, window, monitorIDs); err != nil {
		slog.Error("maintenance: announcement notify failed",
			"maintenance_id", window.ID, "error", err)
	}
}

// IsActive checks if any maintenance window for the monitor is currently active.
// For 'single': half-open [StartDate, EndDate) in absolute UTC.
// For 'cron': uses the CronEvaluator port in the window's IANA timezone
// (empty timezone → UTC).
func (s *MaintenanceService) IsActive(ctx context.Context, monitorID int64) (bool, error) {
	windows, err := s.linkRepo.ListByMonitor(ctx, monitorID)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, mw := range windows {
		if !mw.Active {
			continue
		}
		if mw.Strategy == "single" {
			// Half-open [start, end): inclusive start, exclusive end.
			if !now.Before(mw.StartDate.UTC()) && now.Before(mw.EndDate.UTC()) {
				return true, nil
			}
		} else if mw.Strategy == "cron" && mw.CronExpr != "" {
			loc := locationForTimezone(mw.Timezone)
			if s.cronEval.IsWindowActive(mw.CronExpr, mw.Duration, now, loc) {
				return true, nil
			}
		}
	}
	return false, nil
}

// locationForTimezone loads an IANA location; empty or invalid → UTC.
// Invalid zones are rejected at create/update, so a bad stored value here is a
// data anomaly and must not panic the hot path.
func locationForTimezone(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}
