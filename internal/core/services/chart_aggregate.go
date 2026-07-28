package services

import (
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
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

// DetectDowntimeIntervals returns contiguous periods where status is down or pending.
func DetectDowntimeIntervals(heartbeats []*domain.Heartbeat) []DowntimeInterval {
	if len(heartbeats) == 0 {
		return nil
	}

	sorted := make([]*domain.Heartbeat, len(heartbeats))
	copy(sorted, heartbeats)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Time.Before(sorted[j].Time)
	})

	isDown := func(s domain.Status) bool {
		return s == domain.StatusDown || s == domain.StatusPending
	}

	var out []DowntimeInterval
	var cur *DowntimeInterval

	for _, hb := range sorted {
		if !isDown(hb.Status) {
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
