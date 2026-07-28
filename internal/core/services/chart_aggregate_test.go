package services

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestBucketHeartbeats_MinAvgMax(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	hbs := []*domain.Heartbeat{
		{Time: base, Ping: 100, Status: domain.StatusUp},
		{Time: base.Add(2 * time.Minute), Ping: 200, Status: domain.StatusUp},
		{Time: base.Add(4 * time.Minute), Ping: 50, Status: domain.StatusUp},
		{Time: base.Add(1 * time.Hour), Ping: 300, Status: domain.StatusUp},
	}

	buckets := BucketHeartbeats(hbs, 15*time.Minute)
	if len(buckets) != 2 {
		t.Fatalf("len(buckets) = %d, want 2", len(buckets))
	}
	if buckets[0].Min != 50 || buckets[0].Max != 200 {
		t.Errorf("first bucket min/max = %d/%d, want 50/200", buckets[0].Min, buckets[0].Max)
	}
	wantAvg := (100.0 + 200.0 + 50.0) / 3.0
	if buckets[0].Avg != wantAvg {
		t.Errorf("first bucket avg = %v, want %v", buckets[0].Avg, wantAvg)
	}
}

func TestDetectDowntimeIntervals_MergesContiguous(t *testing.T) {
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	hbs := []*domain.Heartbeat{
		{Time: base, Status: domain.StatusUp},
		{Time: base.Add(5 * time.Minute), Status: domain.StatusDown},
		{Time: base.Add(10 * time.Minute), Status: domain.StatusPending},
		{Time: base.Add(15 * time.Minute), Status: domain.StatusUp},
		{Time: base.Add(20 * time.Minute), Status: domain.StatusDown},
	}

	intervals := DetectDowntimeIntervals(hbs)
	if len(intervals) != 2 {
		t.Fatalf("len(intervals) = %d, want 2", len(intervals))
	}
	if !intervals[0].Start.Equal(base.Add(5*time.Minute)) || !intervals[0].End.Equal(base.Add(10*time.Minute)) {
		t.Errorf("first interval = %+v", intervals[0])
	}
	if !intervals[1].Start.Equal(base.Add(20 * time.Minute)) {
		t.Errorf("second interval start = %v", intervals[1].Start)
	}
}
