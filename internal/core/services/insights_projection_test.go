package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type insightsProjectionFake struct {
	versions map[int64]int64
	history  map[int64][]domain.MonitorHealthInterval
	versionN int
	historyN int
}

func (f *insightsProjectionFake) ListProjectionVersions(_ context.Context, ids []int64) (map[int64]int64, error) {
	f.versionN++
	if len(ids) == 0 {
		return map[int64]int64{}, nil
	}
	return f.versions, nil
}

func (f *insightsProjectionFake) ListHealthHistoryForMonitors(_ context.Context, _ []int64, _, _ time.Time) (map[int64][]domain.MonitorHealthInterval, error) {
	f.historyN++
	return f.history, nil
}

func TestApplyOverallProjection_ReplacesPooledUptime(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(2 * time.Hour)
	pooled := 100.0
	row := InsightsRow{
		MonitorID: 7, AvailabilityPercent: &pooled, DowntimeSeconds: 0,
		LatencyAvgMs: &pooled, LatencySampleN: 40, CoveragePercent: 100,
		Qualification: QualificationQualified,
	}
	intervals := []domain.MonitorHealthInterval{
		{From: from, To: from.Add(time.Hour), Status: domain.StatusUp},
		{From: from.Add(time.Hour), To: to, Status: domain.StatusDown},
	}
	got := applyOverallProjection(row, intervals, from, to)
	if got.AvailabilityPercent == nil || *got.AvailabilityPercent != 50 {
		t.Fatalf("availability = %v, want 50", got.AvailabilityPercent)
	}
	if got.DowntimeSeconds != int64(time.Hour/time.Second) || got.OutageCount != 1 || got.FlapCount != 1 {
		t.Fatalf("downtime/outage/flap = %d/%d/%d", got.DowntimeSeconds, got.OutageCount, got.FlapCount)
	}
	if got.LatencyAvgMs != nil || got.LatencySampleN != 0 {
		t.Fatalf("pooled latency survived: %+v", got)
	}
	if got.Qualification != QualificationQualified {
		t.Fatalf("qualification = %s", got.Qualification)
	}
}

func TestApplyOverallProjection_OpenOutageIsNotNew(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	got := applyOverallProjection(InsightsRow{}, []domain.MonitorHealthInterval{
		{From: from.Add(-time.Hour), To: to, Status: domain.StatusDown},
	}, from, to)
	if got.OutageCount != 0 || got.DowntimeSeconds != int64(time.Hour/time.Second) {
		t.Fatalf("open outage counted as new: %+v", got)
	}
}

func TestInsightsCacheMissesWhenProjectionVersionChanges(t *testing.T) {
	s, _, _, q := newInsightsProbeService(t)
	reader := &insightsProjectionFake{versions: map[int64]int64{1: 1}}
	s.SetProjectionReader(reader)
	ctx := context.Background()
	first, err := s.GetInsights(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProjectionVersion != 1 || reader.versionN != 1 || reader.historyN != 1 {
		t.Fatalf("first read version=%d versionCalls=%d historyCalls=%d", first.ProjectionVersion, reader.versionN, reader.historyN)
	}
	second, err := s.GetInsights(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if second.ProjectionVersion != 1 || reader.historyN != 1 || reader.versionN != 2 {
		t.Fatalf("cache hit recalculated: versionCalls=%d historyCalls=%d", reader.versionN, reader.historyN)
	}
	reader.versions[1] = 2
	third, err := s.GetInsights(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if third.ProjectionVersion != 2 || reader.historyN != 2 {
		t.Fatalf("version bump did not miss the cache: version=%d historyCalls=%d", third.ProjectionVersion, reader.historyN)
	}
}

func TestInsightsOverallHistoryReplacesHeartbeatRanking(t *testing.T) {
	s, _, _, q := newInsightsProbeService(t)
	to := s.now().UTC()
	from := to.Add(-24 * time.Hour)
	s.SetProjectionReader(&insightsProjectionFake{
		versions: map[int64]int64{1: 3},
		history: map[int64][]domain.MonitorHealthInterval{
			1: {
				{From: from, To: from.Add(12 * time.Hour), Status: domain.StatusUp},
				{From: from.Add(12 * time.Hour), To: from.Add(24 * time.Hour), Status: domain.StatusDown},
			},
		},
	})
	got, err := s.GetInsights(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 || got.Rows[0].AvailabilityPercent == nil || *got.Rows[0].AvailabilityPercent != 50 {
		t.Fatalf("row = %+v", got.Rows)
	}
	if got.Rows[0].LatencySampleN != 0 || got.ProjectionVersion != 3 {
		t.Fatalf("latency or version = %+v", got.Rows[0])
	}
}

var _ insightsProjectionReader = (*insightsProjectionFake)(nil)
var _ ports.MonitorHealthProjectionRepository = (*healthProjectionRepo)(nil)
