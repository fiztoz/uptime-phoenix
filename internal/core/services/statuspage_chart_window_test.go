package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestMonitorPublicChart_CoversFull24hWindow pins issue #195: a 60 s monitor
// with more than 24 h of history must chart the whole labeled 24 h window
// (~96 fifteen-minute buckets), not just the newest 500 samples (~8.3 h, the
// "35 intervals · 24h" users saw).
func TestMonitorPublicChart_CoversFull24hWindow(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	now := time.Now().UTC().Truncate(time.Second)
	for k := 26 * 60; k >= 0; k-- {
		_ = repo.Save(context.Background(), &domain.Heartbeat{
			MonitorID: 1, Status: domain.StatusUp, Ping: 20 + k%7,
			Time: now.Add(-time.Duration(k) * time.Minute),
		})
	}
	svc := &StatusPageService{hbRepo: repo}

	chart := svc.monitorPublicChart(t.Context(), 1)

	if n := len(chart.Buckets); n < 95 || n > 97 {
		t.Fatalf("public chart has %d buckets; want ~96 covering 24 h", n)
	}
	first, err := time.Parse(time.RFC3339, chart.Buckets[0].Time)
	if err != nil {
		t.Fatal(err)
	}
	from := now.Add(-24 * time.Hour)
	if first.After(from.Add(15 * time.Minute)) {
		t.Fatalf("first bucket %s; want the start of the 24 h window %s", first, from)
	}
}
