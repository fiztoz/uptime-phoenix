package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// spUptimeHeartbeatRepo serves either daily aggregates or raw heartbeats, so both
// branches of uptimeDayCounts can be driven.
type spUptimeHeartbeatRepo struct {
	aggs       []*ports.Aggregate1d
	heartbeats []*domain.Heartbeat
}

func (r *spUptimeHeartbeatRepo) GetAggregate1d(_ context.Context, _ int64, from time.Time) ([]*ports.Aggregate1d, error) {
	var out []*ports.Aggregate1d
	for _, a := range r.aggs {
		if !a.Bucket.Before(from) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *spUptimeHeartbeatRepo) ListByMonitor(_ context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	var out []*domain.Heartbeat
	for _, h := range r.heartbeats {
		if h.MonitorID == monitorID && !h.Time.Before(from) && !h.Time.After(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (r *spUptimeHeartbeatRepo) Save(_ context.Context, _ *domain.Heartbeat) error { return nil }
func (r *spUptimeHeartbeatRepo) GetLatest(_ context.Context, _ int64) (*domain.Heartbeat, error) {
	return nil, ports.ErrNotFound
}
func (r *spUptimeHeartbeatRepo) DeleteByMonitor(_ context.Context, _ int64) error { return nil }
func (r *spUptimeHeartbeatRepo) DeleteOlderThan(_ context.Context, _ time.Time) error {
	return nil
}
func (r *spUptimeHeartbeatRepo) SaveAggregate1m(_ context.Context, _ *ports.Aggregate1m) error {
	return nil
}
func (r *spUptimeHeartbeatRepo) SaveAggregate1h(_ context.Context, _ *ports.Aggregate1h) error {
	return nil
}
func (r *spUptimeHeartbeatRepo) SaveAggregate1d(_ context.Context, _ *ports.Aggregate1d) error {
	return nil
}
func (r *spUptimeHeartbeatRepo) GetAggregate1m(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *spUptimeHeartbeatRepo) GetAggregate1h(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}

func spServiceWith(repo ports.HeartbeatRepository) *StatusPageService {
	return &StatusPageService{hbRepo: repo}
}

func dayAt(offsetDays int) time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, offsetDays)
}

func requireUptimePercent(t *testing.T, percentage *float64) float64 {
	t.Helper()
	if percentage == nil {
		t.Fatal("uptime percentage is unknown; want a measured value")
	}
	return *percentage
}

// TestMonitorUptimeBar_ReportsRealDowntime asserts the uptime bar reflects real
// downtime instead of claiming perfect uptime.
func TestMonitorUptimeBar_ReportsRealDowntime(t *testing.T) {
	repo := &spUptimeHeartbeatRepo{
		aggs: []*ports.Aggregate1d{
			// Yesterday: 90 up, 10 down -> 90% and a red segment.
			{Bucket: dayAt(-1), UpCount: 90, DownCount: 10, TotalChecks: 100},
			// Today: all up.
			{Bucket: dayAt(0), UpCount: 100, TotalChecks: 100},
		},
	}
	bar, percentage := spServiceWith(repo).monitorUptimeBar(context.Background(), 1)
	pct := requireUptimePercent(t, percentage)

	if len(bar) != publicUptimeBarDays {
		t.Fatalf("bar length = %d; want exactly %d (the frontend renders a fixed strip)", len(bar), publicUptimeBarDays)
	}

	// 190 up out of 200 effective checks.
	if want := 95.0; pct != want {
		t.Errorf("uptime = %v%%; want %v%% — a hardcoded 100%% would hide all downtime", pct, want)
	}

	yesterday := bar[len(bar)-2]
	today := bar[len(bar)-1]
	if yesterday.Status != "down" {
		t.Errorf("yesterday status = %q; want \"down\" (one failed check must redden the day)", yesterday.Status)
	}
	if today.Status != "up" {
		t.Errorf("today status = %q; want \"up\"", today.Status)
	}
	if yesterday.Date != dayAt(-1).Format(time.DateOnly) {
		t.Errorf("yesterday date = %q; want %q", yesterday.Date, dayAt(-1).Format(time.DateOnly))
	}
	// Days with no data at all are "none", not a silent "up".
	if bar[0].Status != "none" {
		t.Errorf("day with no checks = %q; want \"none\"", bar[0].Status)
	}
}

// A maintenance window must not count against uptime, and must be visually
// distinct from a real outage.
func TestMonitorUptimeBar_ExcludesMaintenanceFromUptime(t *testing.T) {
	repo := &spUptimeHeartbeatRepo{
		aggs: []*ports.Aggregate1d{
			{Bucket: dayAt(-1), MaintCount: 100, TotalChecks: 100},
			{Bucket: dayAt(0), UpCount: 50, TotalChecks: 50},
		},
	}
	bar, percentage := spServiceWith(repo).monitorUptimeBar(context.Background(), 1)
	pct := requireUptimePercent(t, percentage)

	if pct != 100.0 {
		t.Errorf("uptime = %v%%; want 100%% — maintenance checks are excluded, not counted as downtime", pct)
	}
	if got := bar[len(bar)-2].Status; got != "maintenance" {
		t.Errorf("maintenance day status = %q; want \"maintenance\" (must not read as an outage)", got)
	}
}

// A monitor younger than the daily-rollup interval has no aggregates yet. Without
// the raw-heartbeat fallback its bar would read as 90 empty days on day one.
func TestMonitorUptimeBar_FallsBackToRawHeartbeats(t *testing.T) {
	now := time.Now().UTC()
	repo := &spUptimeHeartbeatRepo{
		heartbeats: []*domain.Heartbeat{
			{MonitorID: 1, Status: domain.StatusUp, Time: now.Add(-3 * time.Minute)},
			{MonitorID: 1, Status: domain.StatusUp, Time: now.Add(-2 * time.Minute)},
			{MonitorID: 1, Status: domain.StatusDown, Time: now.Add(-1 * time.Minute)},
		},
	}
	bar, percentage := spServiceWith(repo).monitorUptimeBar(context.Background(), 1)
	pct := requireUptimePercent(t, percentage)

	if want := 200.0 / 3.0; pct < want-0.01 || pct > want+0.01 {
		t.Errorf("uptime = %v%%; want ~%.2f%% computed from raw heartbeats", pct, want)
	}
	if got := bar[len(bar)-1].Status; got != "down" {
		t.Errorf("today status = %q; want \"down\"", got)
	}
}

// No checks at all is unknown uptime — there is no evidence supporting either
// 0% or 100%, so the public trust surface must not invent a percentage.
func TestMonitorUptimeBar_NoDataIsUnknown(t *testing.T) {
	bar, pct := spServiceWith(&spUptimeHeartbeatRepo{}).monitorUptimeBar(context.Background(), 1)

	if pct != nil {
		t.Errorf("uptime with no checks = %v%%; want an unknown value", *pct)
	}
	if len(bar) != publicUptimeBarDays {
		t.Fatalf("bar length = %d; want %d", len(bar), publicUptimeBarDays)
	}
	for _, d := range bar {
		if d.Status != "none" {
			t.Fatalf("day %s = %q; want every day \"none\"", d.Date, d.Status)
		}
	}
}

func TestUptimePeriod_ExcludesMaintenanceAndMarksCompletedCalendarPeriod(t *testing.T) {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	byDay := map[string]*dayCounts{
		"2026-01-02": {up: 90, down: 10, total: 100},
		"2026-01-03": {maint: 50, total: 50},
	}

	period := uptimePeriod(
		"January 2026",
		start,
		time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC),
		byDay,
	)

	if got := requireUptimePercent(t, period.UptimePercent); got != 90 {
		t.Errorf("period uptime = %v%%; want 90%%", got)
	}
	if !period.Complete {
		t.Error("January period is not complete after February begins")
	}
	if period.StartDate != "2026-01-01" || period.EndDate != "2026-01-31" {
		t.Errorf("period dates = %s..%s; want 2026-01-01..2026-01-31", period.StartDate, period.EndDate)
	}
}

func TestMonitorUptimeHistory_ReturnsMonthlyAndQuarterlyRowsWithUnknownNoData(t *testing.T) {
	history := spServiceWith(&spUptimeHeartbeatRepo{}).monitorUptimeHistory(context.Background(), 1)

	if len(history.Monthly) != publicUptimeHistoryMonths {
		t.Fatalf("monthly periods = %d; want %d", len(history.Monthly), publicUptimeHistoryMonths)
	}
	if len(history.Quarterly) != publicUptimeHistoryQuarters {
		t.Fatalf("quarterly periods = %d; want %d", len(history.Quarterly), publicUptimeHistoryQuarters)
	}
	now := time.Now().UTC()
	if history.Monthly[0].Label != now.Format("January 2006") {
		t.Errorf("newest monthly label = %q; want %q", history.Monthly[0].Label, now.Format("January 2006"))
	}
	if history.Monthly[0].UptimePercent != nil || history.Quarterly[0].UptimePercent != nil {
		t.Fatal("periods without checks must keep uptime unknown")
	}
	if history.Monthly[0].Complete || history.Quarterly[0].Complete {
		t.Fatal("current calendar periods must be marked incomplete")
	}
}

func TestNormalizeStatusPageSLATarget(t *testing.T) {
	valid := 99.9
	sp := &domain.StatusPage{SLATarget: &valid}
	if err := normalizeStatusPageSLATarget(sp); err != nil {
		t.Fatalf("valid SLA target rejected: %v", err)
	}

	clear := 0.0
	sp.SLATarget = &clear
	if err := normalizeStatusPageSLATarget(sp); err != nil || sp.SLATarget != nil {
		t.Fatalf("zero SLA target should clear display; target=%v err=%v", sp.SLATarget, err)
	}

	invalid := 100.001
	sp.SLATarget = &invalid
	if err := normalizeStatusPageSLATarget(sp); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid SLA error = %v; want domain.ErrValidation", err)
	}
}
