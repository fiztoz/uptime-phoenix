package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type statsFakeMonitorRepo struct {
	monitor *domain.Monitor
	err     error
}

func (r *statsFakeMonitorRepo) Create(_ context.Context, _ *domain.Monitor) error { return nil }
func (r *statsFakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.monitor == nil || r.monitor.ID != id {
		return nil, ports.ErrNotFound
	}
	return r.monitor, nil
}
func (r *statsFakeMonitorRepo) GetByPushToken(_ context.Context, _ string) (*domain.Monitor, error) {
	return nil, nil
}
func (r *statsFakeMonitorRepo) List(_ context.Context, _ ports.MonitorFilter) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *statsFakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *statsFakeMonitorRepo) Update(_ context.Context, _ *domain.Monitor) error { return nil }
func (r *statsFakeMonitorRepo) Delete(_ context.Context, _ int64) error           { return nil }
func (r *statsFakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *statsFakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (r *statsFakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) {
	return 0, nil
}

type statsFakeTLSRepo struct {
	mu   sync.Mutex
	info map[int64]*ports.TLSInfo
}

func newStatsFakeTLSRepo() *statsFakeTLSRepo {
	return &statsFakeTLSRepo{info: make(map[int64]*ports.TLSInfo)}
}

func (r *statsFakeTLSRepo) Upsert(_ context.Context, info *ports.TLSInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.info[info.MonitorID] = info
	return nil
}

func (r *statsFakeTLSRepo) GetByMonitorID(_ context.Context, monitorID int64) (*ports.TLSInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.info[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return info, nil
}

func TestMonitorStatsService_GetStats(t *testing.T) {
	now := time.Now().UTC()
	hbRepo := newAggFakeHeartbeatRepo()
	hbRepo.latest[1] = &domain.Heartbeat{ID: 10, MonitorID: 1, Status: domain.StatusUp, Ping: 55, Time: now}
	hbRepo.heartbeats = []*domain.Heartbeat{
		{ID: 1, MonitorID: 1, Status: domain.StatusUp, Ping: 100, Time: now.Add(-2 * time.Hour)},
		{ID: 2, MonitorID: 1, Status: domain.StatusUp, Ping: 200, Time: now.Add(-1 * time.Hour)},
		{ID: 3, MonitorID: 1, Status: domain.StatusDown, Ping: 0, Time: now.Add(-30 * time.Minute)},
		{ID: 4, MonitorID: 1, Status: domain.StatusUp, Ping: 300, Time: now.Add(-10 * time.Minute)},
	}

	monRepo := &statsFakeMonitorRepo{monitor: &domain.Monitor{ID: 1, Active: true}}
	tlsRepo := newStatsFakeTLSRepo()
	expiry := now.Add(45 * 24 * time.Hour)
	tlsRepo.info[1] = &ports.TLSInfo{
		MonitorID:     1,
		DaysRemaining: 45,
		NotAfter:      expiry,
		Issuer:        "Test CA",
		CheckedAt:     now,
	}

	aggSvc := NewAggregateService(hbRepo, monRepo, testLogger())
	svc := NewMonitorStatsService(hbRepo, monRepo, tlsRepo, aggSvc)

	stats, err := svc.GetStats(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetStats returned error: %v", err)
	}

	if stats.CurrentPingMs != 55 {
		t.Errorf("CurrentPingMs = %d, want 55", stats.CurrentPingMs)
	}
	if stats.AvgPing24h != 200 {
		t.Errorf("AvgPing24h = %v, want 200", stats.AvgPing24h)
	}
	if stats.Uptime24h != 75 {
		t.Errorf("Uptime24h = %v, want 75", stats.Uptime24h)
	}
	if stats.Uptime30d != 75 {
		t.Errorf("Uptime30d = %v, want 75", stats.Uptime30d)
	}
	if stats.CertDaysLeft == nil || *stats.CertDaysLeft != 45 {
		t.Errorf("CertDaysLeft = %v, want 45", stats.CertDaysLeft)
	}
	if stats.CertExpiryDate == nil || *stats.CertExpiryDate != expiry.UTC().Format(time.RFC3339) {
		t.Errorf("CertExpiryDate = %v, want %s", stats.CertExpiryDate, expiry.UTC().Format(time.RFC3339))
	}
}

func TestMonitorStatsService_GetStats_MonitorNotFound(t *testing.T) {
	hbRepo := newAggFakeHeartbeatRepo()
	monRepo := &statsFakeMonitorRepo{monitor: nil}
	aggSvc := NewAggregateService(hbRepo, monRepo, testLogger())
	svc := NewMonitorStatsService(hbRepo, monRepo, nil, aggSvc)

	_, err := svc.GetStats(context.Background(), 99)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMonitorStatsService_GetStats_NoTLSInfo(t *testing.T) {
	now := time.Now().UTC()
	hbRepo := newAggFakeHeartbeatRepo()
	hbRepo.latest[1] = &domain.Heartbeat{ID: 1, MonitorID: 1, Status: domain.StatusUp, Ping: 10, Time: now}
	monRepo := &statsFakeMonitorRepo{monitor: &domain.Monitor{ID: 1, Active: true}}
	aggSvc := NewAggregateService(hbRepo, monRepo, testLogger())
	svc := NewMonitorStatsService(hbRepo, monRepo, newStatsFakeTLSRepo(), aggSvc)

	stats, err := svc.GetStats(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetStats returned error: %v", err)
	}
	if stats.CertDaysLeft != nil {
		t.Errorf("CertDaysLeft = %v, want nil", stats.CertDaysLeft)
	}
	if stats.CertExpiryDate != nil {
		t.Errorf("CertExpiryDate = %v, want nil", stats.CertExpiryDate)
	}
}
