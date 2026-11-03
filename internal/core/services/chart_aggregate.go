package services

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ChartBucket holds min/avg/max latency for a time bucket.
type ChartBucket struct {
	Time time.Time
	Min  int
	Avg  float64
	Max  int
}

// DowntimeInterval marks a contiguous down/pending period on the chart.
type DowntimeInterval struct {
	Start time.Time
	End   time.Time
}

// BucketDurationForRange picks bucket width from selected chart range (hours).
func BucketDurationForRange(hours int) time.Duration {
	switch {
	case hours <= 1:
		return time.Minute
	case hours <= 6:
		return 5 * time.Minute
	case hours <= 24:
		return 15 * time.Minute
	default:
		return time.Hour
	}
}

// ChartData is the aggregated response-time chart for one monitor window.
type ChartData struct {
	Buckets           []ChartBucket
	DowntimeIntervals []DowntimeInterval
	UnknownIntervals  []DowntimeInterval
}

// LoadChart aggregates the WHOLE [from, to] window into bucketDur buckets plus
// downtime / unknown intervals (issue #195: the charts used to keep only the
// newest N raw beats, so a "24 h" chart covered ~8 h at a 60 s interval).
//
// Work and response size stay bounded without truncating the window: with a
// ports.HeartbeatChartReader the database returns at most one row per bucket
// plus at most intervalLimit runs, never the raw beats. intervalLimit keeps the
// newest runs (both kinds together); <= 0 means unbounded. Repositories without
// the capability (test fakes) are aggregated in Go over the full window, with
// the identical output contract. Bounds are forced to UTC (rule 6).
func LoadChart(
	ctx context.Context, repo ports.HeartbeatRepository, monitorID int64,
	from, to time.Time, bucketDur time.Duration, intervalLimit int,
) (ChartData, error) {
	from, to = from.UTC(), to.UTC()
	var runs []ports.StatusRun
	var buckets []ChartBucket
	if reader, ok := repo.(ports.HeartbeatChartReader); ok {
		rows, err := reader.ListLatencyBuckets(ctx, monitorID, from, to, bucketDur)
		if err != nil {
			return ChartData{}, fmt.Errorf("chart latency buckets: %w", err)
		}
		buckets = make([]ChartBucket, 0, len(rows))
		for _, r := range rows {
			if r.PingCount <= 0 {
				continue
			}
			buckets = append(buckets, ChartBucket{
				Time: r.Start.UTC(),
				Min:  r.MinPing,
				Avg:  float64(r.PingSum) / float64(r.PingCount),
				Max:  r.MaxPing,
			})
		}
		if runs, err = reader.ListStatusRuns(ctx, monitorID, from, to, intervalLimit); err != nil {
			return ChartData{}, fmt.Errorf("chart status runs: %w", err)
		}
	} else {
		heartbeats, err := repo.ListByMonitor(ctx, monitorID, from, to)
		if err != nil {
			return ChartData{}, fmt.Errorf("chart heartbeats: %w", err)
		}
		buckets = BucketHeartbeats(heartbeats, bucketDur)
		for _, iv := range DetectDowntimeIntervals(heartbeats) {
			runs = append(runs, ports.StatusRun{Kind: ports.StatusRunDowntime, Start: iv.Start, End: iv.End})
		}
		for _, iv := range DetectUnknownIntervals(heartbeats) {
			runs = append(runs, ports.StatusRun{Kind: ports.StatusRunUnknown, Start: iv.Start, End: iv.End})
		}
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].Start.Before(runs[j].Start) })
		if intervalLimit > 0 && len(runs) > intervalLimit {
			runs = runs[len(runs)-intervalLimit:]
		}
	}

	out := ChartData{Buckets: buckets}
	for _, run := range runs {
		iv := DowntimeInterval{Start: run.Start, End: run.End}
		switch run.Kind {
		case ports.StatusRunDowntime:
			out.DowntimeIntervals = append(out.DowntimeIntervals, iv)
		case ports.StatusRunUnknown:
			out.UnknownIntervals = append(out.UnknownIntervals, iv)
		}
	}
	return out, nil
}

// BucketHeartbeats groups heartbeats into fixed-width time buckets with min/avg/max ping.
// Heartbeats with ping <= 0 are excluded from latency stats but still advance bucket windows.
func BucketHeartbeats(heartbeats []*domain.Heartbeat, bucketDuration time.Duration) []ChartBucket {
	if len(heartbeats) == 0 || bucketDuration <= 0 {
		return nil
	}

	sorted := make([]*domain.Heartbeat, len(heartbeats))
	copy(sorted, heartbeats)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Time.Before(sorted[j].Time)
	})

	type acc struct {
		sum   int
		count int
		min   int
		max   int
	}
	buckets := make(map[int64]*acc)
	var order []int64

	for _, hb := range sorted {
		key := hb.Time.UTC().Truncate(bucketDuration).Unix()
		a, ok := buckets[key]
		if !ok {
			a = &acc{min: hb.Ping, max: hb.Ping}
			buckets[key] = a
			order = append(order, key)
		}
		if hb.Ping > 0 {
			a.sum += hb.Ping
			a.count++
			if a.count == 1 || hb.Ping < a.min {
				a.min = hb.Ping
			}
			if hb.Ping > a.max {
				a.max = hb.Ping
			}
		}
	}

	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	out := make([]ChartBucket, 0, len(order))
	for _, key := range order {
		a := buckets[key]
		if a.count == 0 {
			continue
		}
		out = append(out, ChartBucket{
			Time: time.Unix(key, 0).UTC(),
			Min:  a.min,
			Avg:  float64(a.sum) / float64(a.count),
			Max:  a.max,
		})
	}
	return out
}

// DetectUnknownIntervals returns contiguous periods whose status is UNKNOWN.
// Unknown evidence is not downtime and is not healthy uptime.
func DetectUnknownIntervals(heartbeats []*domain.Heartbeat) []DowntimeInterval {
	return detectStatusIntervals(heartbeats, func(s domain.Status) bool {
		return s == domain.StatusUnknown
	})
}

// DetectDowntimeIntervals returns contiguous periods where status is down or pending.
func DetectDowntimeIntervals(heartbeats []*domain.Heartbeat) []DowntimeInterval {
	return detectStatusIntervals(heartbeats, func(s domain.Status) bool {
		return s == domain.StatusDown || s == domain.StatusPending
	})
}

func detectStatusIntervals(heartbeats []*domain.Heartbeat, match func(domain.Status) bool) []DowntimeInterval {
	if len(heartbeats) == 0 {
		return nil
	}

	sorted := make([]*domain.Heartbeat, len(heartbeats))
	copy(sorted, heartbeats)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Time.Before(sorted[j].Time)
	})

	var out []DowntimeInterval
	var cur *DowntimeInterval

	for _, hb := range sorted {
		if !match(hb.Status) {
			cur = nil
			continue
		}
		if cur == nil {
			cur = &DowntimeInterval{Start: hb.Time, End: hb.Time}
			out = append(out, *cur)
			cur = &out[len(out)-1]
			continue
		}
		cur.End = hb.Time
	}

	return out
}
