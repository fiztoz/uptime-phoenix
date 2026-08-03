package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type insightsFakeReliabilityReader struct {
	listCalls    int
	leadingCalls int
}

func (r *insightsFakeReliabilityReader) ListImportantForMonitors(context.Context, []int64, time.Time, time.Time) (map[int64][]*domain.Heartbeat, error) {
	r.listCalls++
	return map[int64][]*domain.Heartbeat{}, nil
}

func (r *insightsFakeReliabilityReader) LatestImportantBeforeForMonitors(context.Context, []int64, time.Time) (map[int64]*domain.Heartbeat, error) {
	r.leadingCalls++
	return map[int64]*domain.Heartbeat{}, nil
}

type insightsFakeAggregateReader struct{}

func (insightsFakeAggregateReader) GetAggregate1hForMonitors(context.Context, []int64, time.Time) (map[int64][]*ports.Aggregate1h, error) {
	return map[int64][]*ports.Aggregate1h{}, nil
}

func (insightsFakeAggregateReader) GetAggregate1dForMonitors(context.Context, []int64, time.Time) (map[int64][]*ports.Aggregate1d, error) {
	return map[int64][]*ports.Aggregate1d{}, nil
}

func TestInsightsService_NoGrantsReturnsEmptyBeforeAggregation(t *testing.T) {
	h := newAccessHarness(t)
	admin := h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)
	h.addMonitor(t, "hidden", nil)

	reliability := &insightsFakeReliabilityReader{}
	svc := NewInsightsService(reliability, insightsFakeAggregateReader{}, h.monitors, h.groups, h.svc)
	svc.now = func() time.Time { return time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC) }

	result, err := svc.GetInsights(context.Background(), InsightsQuery{UserID: member, Period: Period7d, Metric: MetricAvailability})
	if err != nil {
		t.Fatalf("GetInsights(member): %v", err)
	}
	if len(result.Rows) != 0 {
		t.Fatalf("member received %d rows without grants; want zero", len(result.Rows))
	}
	if reliability.listCalls != 0 || reliability.leadingCalls != 0 {
		t.Fatalf("aggregation was called for no-grant user: list=%d leading=%d", reliability.listCalls, reliability.leadingCalls)
	}

	adminResult, err := svc.GetInsights(context.Background(), InsightsQuery{UserID: admin, Period: Period7d, Metric: MetricAvailability})
	if err != nil {
		t.Fatalf("GetInsights(admin): %v", err)
	}
	if len(adminResult.Rows) != 1 {
		t.Fatalf("admin received %d rows; want one visible monitor", len(adminResult.Rows))
	}
}

func floatPtr(v float64) *float64 { return &v }

func TestSortRows_InsufficientDataNeverRanksAhead(t *testing.T) {
	rows := []InsightsRow{
		{MonitorID: 2, MonitorName: "new", Qualification: QualificationInsufficient, AvailabilityPercent: floatPtr(100), CoveragePercent: 1},
		{MonitorID: 1, MonitorName: "unstable", Qualification: QualificationQualified, AvailabilityPercent: floatPtr(80), CoveragePercent: 90},
	}
	sortRows(rows, MetricAvailability)
	if rows[0].MonitorID != 1 {
		t.Fatalf("first row = %d; want qualified monitor 1", rows[0].MonitorID)
	}
}

func TestSortRows_UsesMetricSpecificTieBreaks(t *testing.T) {
	rows := []InsightsRow{
		{MonitorID: 1, MonitorName: "a", Qualification: QualificationQualified, OutageCount: 2, DowntimeSeconds: 30, CoveragePercent: 90},
		{MonitorID: 2, MonitorName: "b", Qualification: QualificationQualified, OutageCount: 2, DowntimeSeconds: 60, CoveragePercent: 90},
	}
	sortRows(rows, MetricOutages)
	if rows[0].MonitorID != 2 {
		t.Fatalf("outage tie should prefer downtime, got monitor %d", rows[0].MonitorID)
	}
}

func TestLatencyFromRollups_UsesPingCountNotTotalChecks(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	avg, n := latencyFromRollups([]*ports.Aggregate1h{{
		Bucket:      from,
		AvgPing:     100,
		PingCount:   2,
		TotalChecks: 20,
	}}, nil, from, to)
	if avg == nil || *avg != 100 {
		t.Fatalf("average = %v; want 100", avg)
	}
	if n != 2 {
		t.Fatalf("sample count = %d; want 2", n)
	}
}

func TestObservationSecondsFromRollups_ClipsBoundary(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)
	to := from.Add(90 * time.Minute)
	observed := observationSecondsFromRollups([]*ports.Aggregate1h{
		{Bucket: from.Add(-30 * time.Minute), TotalChecks: 60},
		{Bucket: from.Add(30 * time.Minute), TotalChecks: 60},
	}, nil, from, to, 60)
	// Both buckets are clipped to the selected range; no observation may escape
	// the 90-minute window.
	if observed > to.Sub(from).Seconds() {
		t.Fatalf("observed %.0fs exceeds window %.0fs", observed, to.Sub(from).Seconds())
	}
	if observed != to.Sub(from).Seconds() {
		t.Fatalf("observed %.0fs; want clipped 5400s", observed)
	}
}
