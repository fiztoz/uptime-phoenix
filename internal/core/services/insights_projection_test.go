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

// The projection stores one interval per minute bucket, so a long outage
// arrives as many back-to-back DOWN rows. It is still one outage (#198).
func TestApplyOverallProjection_MinuteBucketsAreOneOutage(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	start := from.Add(time.Hour)
	intervals := []domain.MonitorHealthInterval{{From: from, To: start, Status: domain.StatusUp}}
	for i := 0; i < 459; i++ {
		bucket := start.Add(time.Duration(i) * time.Minute)
		intervals = append(intervals, domain.MonitorHealthInterval{From: bucket, To: bucket.Add(time.Minute), Status: domain.StatusDown})
	}
	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 1 {
		t.Fatalf("outages = %d, want 1", got.OutageCount)
	}
	if got.DowntimeSeconds != int64(459*time.Minute/time.Second) {
		t.Fatalf("downtime = %d", got.DowntimeSeconds)
	}
}

// Outage runs follow the heartbeat path: PENDING joins the run it leads into,
// while UP, UNKNOWN or a gap in the history ends it.
func TestApplyOverallProjection_CountsOutageRuns(t *testing.T) {
	from := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return from.Add(time.Duration(m) * time.Minute) }
	iv := func(a, b int, s domain.Status) domain.MonitorHealthInterval {
		return domain.MonitorHealthInterval{From: at(a), To: at(b), Status: s}
	}
	cases := []struct {
		name      string
		intervals []domain.MonitorHealthInterval
		want      int
	}{
		{"pending then down", []domain.MonitorHealthInterval{iv(0, 1, domain.StatusUp), iv(1, 2, domain.StatusPending), iv(2, 3, domain.StatusDown), iv(3, 4, domain.StatusDown)}, 1},
		{"down pending down", []domain.MonitorHealthInterval{iv(0, 1, domain.StatusDown), iv(1, 2, domain.StatusPending), iv(2, 3, domain.StatusDown)}, 1},
		{"unknown splits", []domain.MonitorHealthInterval{iv(0, 1, domain.StatusDown), iv(1, 2, domain.StatusUnknown), iv(2, 3, domain.StatusDown), iv(3, 4, domain.StatusUnknown), iv(4, 5, domain.StatusDown)}, 3},
		{"gap splits", []domain.MonitorHealthInterval{iv(0, 1, domain.StatusDown), iv(2, 3, domain.StatusDown)}, 2},
		{"up splits", []domain.MonitorHealthInterval{iv(0, 1, domain.StatusDown), iv(1, 2, domain.StatusUp), iv(2, 3, domain.StatusDown)}, 2},
		{"pending only", []domain.MonitorHealthInterval{iv(0, 1, domain.StatusUp), iv(1, 2, domain.StatusPending), iv(2, 3, domain.StatusUp)}, 0},
		{"run began before window", []domain.MonitorHealthInterval{iv(-5, 0, domain.StatusPending), iv(0, 1, domain.StatusDown), iv(1, 2, domain.StatusDown)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applyOverallProjection(InsightsRow{}, tc.intervals, from, at(60))
			if got.OutageCount != tc.want {
				t.Fatalf("outages = %d, want %d", got.OutageCount, tc.want)
			}
		})
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
