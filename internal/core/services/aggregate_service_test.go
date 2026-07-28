package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- Test doubles for AggregateService --------------------------------

// aggFakeHeartbeatRepo extends the basic fake with aggregate storage.
type aggFakeHeartbeatRepo struct {
	*fakeHeartbeatRepo
	aggs1m map[int64][]*ports.Aggregate1m // monitorID -> aggregates
	aggs1h map[int64][]*ports.Aggregate1h
	aggs1d map[int64][]*ports.Aggregate1d
}

func newAggFakeHeartbeatRepo() *aggFakeHeartbeatRepo {
	return &aggFakeHeartbeatRepo{
		fakeHeartbeatRepo: newFakeHeartbeatRepo(),
		aggs1m:            make(map[int64][]*ports.Aggregate1m),
		aggs1h:            make(map[int64][]*ports.Aggregate1h),
		aggs1d:            make(map[int64][]*ports.Aggregate1d),
	}
}

func (r *aggFakeHeartbeatRepo) SaveAggregate1m(_ context.Context, agg *ports.Aggregate1m) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aggs1m[agg.MonitorID] = append(r.aggs1m[agg.MonitorID], agg)
	return nil
}

func (r *aggFakeHeartbeatRepo) SaveAggregate1h(_ context.Context, agg *ports.Aggregate1h) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aggs1h[agg.MonitorID] = append(r.aggs1h[agg.MonitorID], agg)
	return nil
}

func (r *aggFakeHeartbeatRepo) SaveAggregate1d(_ context.Context, agg *ports.Aggregate1d) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aggs1d[agg.MonitorID] = append(r.aggs1d[agg.MonitorID], agg)
	return nil
}

func (r *aggFakeHeartbeatRepo) GetAggregate1m(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1m, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*ports.Aggregate1m
	for _, a := range r.aggs1m[monitorID] {
		if !a.Bucket.Before(from) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *aggFakeHeartbeatRepo) GetAggregate1h(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1h, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*ports.Aggregate1h
	for _, a := range r.aggs1h[monitorID] {
		if !a.Bucket.Before(from) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *aggFakeHeartbeatRepo) GetAggregate1d(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1d, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*ports.Aggregate1d
	for _, a := range r.aggs1d[monitorID] {
		if !a.Bucket.Before(from) {
			out = append(out, a)
		}
	}
	return out, nil
}

// fakeMonitorRepo is a simple in-memory MonitorRepository for tests.
type fakeMonitorRepo struct {
	monitors []*domain.Monitor
}

func (r *fakeMonitorRepo) Create(_ context.Context, _ *domain.Monitor) error { return nil }
func (r *fakeMonitorRepo) GetByID(_ context.Context, _ int64) (*domain.Monitor, error) {
	return nil, nil
}
func (r *fakeMonitorRepo) List(_ context.Context, _ ports.MonitorFilter) ([]*domain.Monitor, error) {
	return r.monitors, nil
}
func (r *fakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	return r.monitors, nil
}
func (r *fakeMonitorRepo) Update(_ context.Context, _ *domain.Monitor) error { return nil }
func (r *fakeMonitorRepo) Delete(_ context.Context, _ int64) error           { return nil }

func (r *fakeMonitorRepo) GetByPushToken(_ context.Context, _ string) (*domain.Monitor, error) {
	return nil, nil
}
func (r *fakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *fakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error)  { return 0, nil }
func (r *fakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) { return 0, nil }

// --- Tests -------------------------------------------------------------

// testLogger returns a no-op ports.Logger for tests.
func testLogger() ports.Logger { return &noopLogger{} }

type noopLogger struct{}

func (l *noopLogger) Debug(_ string, _ ...any)                   {}
func (l *noopLogger) Info(_ string, _ ...any)                    {}
func (l *noopLogger) Warn(_ string, _ ...any)                    {}
func (l *noopLogger) Error(_ string, _ ...any)                   {}
func (l *noopLogger) With(_ ...any) ports.Logger                 { return l }
func (l *noopLogger) WithContext(_ context.Context) ports.Logger { return l }

func TestRollup1m_EmptyHeartbeats(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{
		monitors: []*domain.Monitor{
			{ID: 1, Active: true},
		},
	}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	err := svc.Rollup1m(context.Background(), now.Add(-2*time.Minute), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No heartbeats = no aggregates saved.
	if len(hbRepo.aggs1m[1]) != 0 {
		t.Errorf("expected 0 aggregates, got %d", len(hbRepo.aggs1m[1]))
	}
}

func TestRollup1m_GroupsByMinute(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{
		monitors: []*domain.Monitor{
			{ID: 1, Active: true},
		},
	}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	base := now.Truncate(time.Minute)

	// Add heartbeats in two different minutes.
	hbRepo.heartbeats = []*domain.Heartbeat{
		{ID: 1, MonitorID: 1, Status: domain.StatusUp, Time: base.Add(10 * time.Second), Ping: 50},
		{ID: 2, MonitorID: 1, Status: domain.StatusUp, Time: base.Add(30 * time.Second), Ping: 60},
		{ID: 3, MonitorID: 1, Status: domain.StatusDown, Time: base.Add(1*time.Minute + 5*time.Second), Ping: 0},
	}

	err := svc.Rollup1m(context.Background(), base, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hbRepo.aggs1m[1]) != 2 {
		t.Fatalf("expected 2 aggregate buckets, got %d", len(hbRepo.aggs1m[1]))
	}

	// First bucket: 2 UP, avg ping 55
	agg0 := hbRepo.aggs1m[1][0]
	if agg0.UpCount != 2 {
		t.Errorf("bucket 0: expected 2 up, got %d", agg0.UpCount)
	}
	if agg0.AvgPing != 55.0 {
		t.Errorf("bucket 0: expected avg ping 55, got %f", agg0.AvgPing)
	}
	if agg0.MinPing != 50 {
		t.Errorf("bucket 0: expected min ping 50, got %d", agg0.MinPing)
	}
	if agg0.MaxPing != 60 {
		t.Errorf("bucket 0: expected max ping 60, got %d", agg0.MaxPing)
	}

	// Second bucket: 1 DOWN
	agg1 := hbRepo.aggs1m[1][1]
	if agg1.DownCount != 1 {
		t.Errorf("bucket 1: expected 1 down, got %d", agg1.DownCount)
	}
}

func TestRollup1m_MultipleMonitors(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{
		monitors: []*domain.Monitor{
			{ID: 1, Active: true},
			{ID: 2, Active: true},
		},
	}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	base := now.Truncate(time.Minute)

	hbRepo.heartbeats = []*domain.Heartbeat{
		{ID: 1, MonitorID: 1, Status: domain.StatusUp, Time: base.Add(5 * time.Second), Ping: 100},
		{ID: 2, MonitorID: 2, Status: domain.StatusDown, Time: base.Add(5 * time.Second), Ping: 0},
	}

	err := svc.Rollup1m(context.Background(), base, base.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hbRepo.aggs1m[1]) != 1 {
		t.Errorf("expected 1 agg for monitor 1, got %d", len(hbRepo.aggs1m[1]))
	}
	if len(hbRepo.aggs1m[2]) != 1 {
		t.Errorf("expected 1 agg for monitor 2, got %d", len(hbRepo.aggs1m[2]))
	}
}

func TestRollup1h_MergesFrom1m(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{
		monitors: []*domain.Monitor{
			{ID: 1, Active: true},
		},
	}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	hourStart := now.Truncate(time.Hour)

	// Simulate existing 1m aggregates.
	hbRepo.aggs1m[1] = []*ports.Aggregate1m{
		{MonitorID: 1, Bucket: hourStart, UpCount: 5, DownCount: 1, TotalChecks: 6, AvgPing: 100, MinPing: 50, MaxPing: 200},
		{MonitorID: 1, Bucket: hourStart.Add(1 * time.Minute), UpCount: 6, DownCount: 0, TotalChecks: 6, AvgPing: 80, MinPing: 40, MaxPing: 150},
	}

	err := svc.Rollup1h(context.Background(), hourStart, hourStart.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hbRepo.aggs1h[1]) != 1 {
		t.Fatalf("expected 1 hourly aggregate, got %d", len(hbRepo.aggs1h[1]))
	}

	agg := hbRepo.aggs1h[1][0]
	if agg.UpCount != 11 {
		t.Errorf("expected 11 up, got %d", agg.UpCount)
	}
	if agg.DownCount != 1 {
		t.Errorf("expected 1 down, got %d", agg.DownCount)
	}
	if agg.TotalChecks != 12 {
		t.Errorf("expected 12 total checks, got %d", agg.TotalChecks)
	}
}

func TestRollup1d_MergesFrom1h(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{
		monitors: []*domain.Monitor{
			{ID: 1, Active: true},
		},
	}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	dayStart := now.Truncate(24 * time.Hour)

	hbRepo.aggs1h[1] = []*ports.Aggregate1h{
		{MonitorID: 1, Bucket: dayStart, UpCount: 50, DownCount: 10, TotalChecks: 60, AvgPing: 100, MinPing: 20, MaxPing: 500},
		{MonitorID: 1, Bucket: dayStart.Add(1 * time.Hour), UpCount: 55, DownCount: 5, TotalChecks: 60, AvgPing: 90, MinPing: 15, MaxPing: 400},
	}

	err := svc.Rollup1d(context.Background(), dayStart, dayStart.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hbRepo.aggs1d[1]) != 1 {
		t.Fatalf("expected 1 daily aggregate, got %d", len(hbRepo.aggs1d[1]))
	}

	agg := hbRepo.aggs1d[1][0]
	if agg.UpCount != 105 {
		t.Errorf("expected 105 up, got %d", agg.UpCount)
	}
	if agg.TotalChecks != 120 {
		t.Errorf("expected 120 total checks, got %d", agg.TotalChecks)
	}
}

func TestGetUptimePercent_AllUp(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)

	hbRepo.heartbeats = []*domain.Heartbeat{
		{ID: 1, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(1 * time.Hour)},
		{ID: 2, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(2 * time.Hour)},
		{ID: 3, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(3 * time.Hour)},
	}

	pct, err := svc.GetUptimePercent(context.Background(), 1, from, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pct != 100.0 {
		t.Errorf("expected 100%%, got %f", pct)
	}
}

func TestGetUptimePercent_MixedStatuses(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)

	hbRepo.heartbeats = []*domain.Heartbeat{
		{ID: 1, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(1 * time.Hour)},
		{ID: 2, MonitorID: 1, Status: domain.StatusDown, Time: from.Add(2 * time.Hour)},
		{ID: 3, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(3 * time.Hour)},
		{ID: 4, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(4 * time.Hour)},
	}

	pct, err := svc.GetUptimePercent(context.Background(), 1, from, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pct != 75.0 {
		t.Errorf("expected 75%%, got %f", pct)
	}
}

func TestGetUptimePercent_ExcludesMaintenance(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)

	hbRepo.heartbeats = []*domain.Heartbeat{
		{ID: 1, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(1 * time.Hour)},
		{ID: 2, MonitorID: 1, Status: domain.StatusMaintenance, Time: from.Add(2 * time.Hour)},
		{ID: 3, MonitorID: 1, Status: domain.StatusUp, Time: from.Add(3 * time.Hour)},
	}

	pct, err := svc.GetUptimePercent(context.Background(), 1, from, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 2 up out of 2 effective (excluding 1 maintenance) = 100%
	if pct != 100.0 {
		t.Errorf("expected 100%% (maintenance excluded), got %f", pct)
	}
}

func TestGetUptimePercent_EmptyHeartbeats(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	from := now.Add(-24 * time.Hour)

	pct, err := svc.GetUptimePercent(context.Background(), 1, from, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pct != 100.0 {
		t.Errorf("expected 100%% for empty, got %f", pct)
	}
}

func TestGetUptimePercent_UsesDailyAggregates(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &fakeMonitorRepo{}
	svc := NewAggregateService(hbRepo, monRepo, testLogger())

	now := time.Now().UTC()
	from := now.Add(-48 * time.Hour)

	// No raw heartbeats, but daily aggregates exist.
	hbRepo.aggs1d[1] = []*ports.Aggregate1d{
		{MonitorID: 1, Bucket: from.Add(12 * time.Hour), UpCount: 50, DownCount: 10, MaintCount: 2, TotalChecks: 62},
	}

	pct, err := svc.GetUptimePercent(context.Background(), 1, from, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 50 up / (62 total - 2 maint) = 50/60 = 83.33%
	expected := (50.0 / 60.0) * 100.0
	if pct < expected-0.01 || pct > expected+0.01 {
		t.Errorf("expected ~%.2f%%, got %f", expected, pct)
	}
}

func TestComputeAggregate_PendingAndMaintenance(t *testing.T) {
	hbs := []*domain.Heartbeat{
		{Status: domain.StatusPending, Ping: 0},
		{Status: domain.StatusMaintenance, Ping: 0},
		{Status: domain.StatusUp, Ping: 100},
	}

	agg := computeAggregate(1, time.Now(), hbs)

	if agg.PendingCount != 1 {
		t.Errorf("expected 1 pending, got %d", agg.PendingCount)
	}
	if agg.MaintCount != 1 {
		t.Errorf("expected 1 maint, got %d", agg.MaintCount)
	}
	if agg.UpCount != 1 {
		t.Errorf("expected 1 up, got %d", agg.UpCount)
	}
	if agg.TotalChecks != 3 {
		t.Errorf("expected 3 total, got %d", agg.TotalChecks)
	}
}

func TestComputeAggregate_ZeroPing(t *testing.T) {
	hbs := []*domain.Heartbeat{
		{Status: domain.StatusDown, Ping: 0},
	}

	agg := computeAggregate(1, time.Now(), hbs)

	if agg.MinPing != 0 {
		t.Errorf("expected min ping 0 for down heartbeat, got %d", agg.MinPing)
	}
	if agg.MaxPing != 0 {
		t.Errorf("expected max ping 0, got %d", agg.MaxPing)
	}
	if agg.AvgPing != 0 {
		t.Errorf("expected avg ping 0, got %f", agg.AvgPing)
	}
}
