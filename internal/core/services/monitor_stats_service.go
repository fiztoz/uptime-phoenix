package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// MonitorStats holds Uptime Kuma-style summary statistics for a monitor.
type MonitorStats struct {
	CurrentPingMs  int     // from GetLatest, 0 if none
	AvgPing24h     float64 // average ping from heartbeats in last 24h where ping > 0
	Uptime24h      float64 // percentage
	Uptime30d      float64 // percentage
	CertExpiryDate *string // RFC3339 date or nil
	CertDaysLeft   *int    // nil if not HTTPS/no cert data
}

// MonitorStatsService computes monitor summary statistics from heartbeats and aggregates.
type MonitorStatsService struct {
	heartbeats ports.HeartbeatRepository
	monitors   ports.MonitorRepository
	tlsInfo    ports.TLSInfoRepository
	aggregate  *AggregateService
}

// NewMonitorStatsService creates a MonitorStatsService.
func NewMonitorStatsService(
	heartbeats ports.HeartbeatRepository,
	monitors ports.MonitorRepository,
	tlsInfo ports.TLSInfoRepository,
	aggregate *AggregateService,
) *MonitorStatsService {
	return &MonitorStatsService{
		heartbeats: heartbeats,
		monitors:   monitors,
		tlsInfo:    tlsInfo,
		aggregate:  aggregate,
	}
}

// GetStats returns summary statistics for the given monitor.
func (s *MonitorStatsService) GetStats(ctx context.Context, monitorID int64) (*MonitorStats, error) {
	if _, err := s.monitors.GetByID(ctx, monitorID); err != nil {
		return nil, fmt.Errorf("monitor stats: get monitor: %w", err)
	}

	now := time.Now().UTC()
	stats := &MonitorStats{}

	latest, err := s.heartbeats.GetLatest(ctx, monitorID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return nil, fmt.Errorf("monitor stats: get latest heartbeat: %w", err)
	}
	if latest != nil {
		stats.CurrentPingMs = latest.Ping
	}

	from24h := now.Add(-24 * time.Hour)
	heartbeats, err := s.heartbeats.ListByMonitor(ctx, monitorID, from24h, now)
	if err != nil {
		return nil, fmt.Errorf("monitor stats: list heartbeats: %w", err)
	}
	stats.AvgPing24h = avgPing(heartbeats)

	uptime24h, err := s.aggregate.GetUptimePercent(ctx, monitorID, from24h, now)
	if err != nil {
		return nil, fmt.Errorf("monitor stats: uptime 24h: %w", err)
	}
	stats.Uptime24h = uptime24h

	from30d := now.Add(-30 * 24 * time.Hour)
	uptime30d, err := s.aggregate.GetUptimePercent(ctx, monitorID, from30d, now)
	if err != nil {
		return nil, fmt.Errorf("monitor stats: uptime 30d: %w", err)
	}
	stats.Uptime30d = uptime30d

	if s.tlsInfo != nil {
		tlsData, err := s.tlsInfo.GetByMonitorID(ctx, monitorID)
		if err != nil && !errors.Is(err, ports.ErrNotFound) {
			return nil, fmt.Errorf("monitor stats: get tls info: %w", err)
		}
		if tlsData != nil {
			expiry := tlsData.NotAfter.UTC().Format(time.RFC3339)
			stats.CertExpiryDate = &expiry
			days := tlsData.DaysRemaining
			stats.CertDaysLeft = &days
		}
	}

	return stats, nil
}

func avgPing(heartbeats []*domain.Heartbeat) float64 {
	var sum int
	var count int
	for _, hb := range heartbeats {
		if hb.Ping > 0 {
			sum += hb.Ping
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return float64(sum) / float64(count)
}
