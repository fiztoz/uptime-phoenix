package services

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AggregateService handles rollup computation from raw heartbeats.
// It reads heartbeats from the repository, groups them into time buckets,
// and writes aggregated statistics to the heartbeat_1m/1h/1d tables.
type AggregateService struct {
	heartbeats ports.HeartbeatRepository
	monitors   ports.MonitorRepository
	logger     ports.Logger
}

// NewAggregateService creates a new AggregateService.
func NewAggregateService(
	heartbeats ports.HeartbeatRepository,
	monitors ports.MonitorRepository,
	logger ports.Logger,
) *AggregateService {
	return &AggregateService{
		heartbeats: heartbeats,
		monitors:   monitors,
		logger:     logger,
	}
}

// Rollup1m computes 1-minute aggregates from raw heartbeats.
// It queries all heartbeats in the [from, to) window, groups them by
// monitor+minute, computes up/down/pending/maint counts and ping stats,
// and saves the results to heartbeat_1m.
func (s *AggregateService) Rollup1m(ctx context.Context, from, to time.Time) error {
	monitors, err := s.monitors.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("listing active monitors: %w", err)
	}

	for _, m := range monitors {
		if err := s.rollup1mForMonitor(ctx, m.ID, from, to); err != nil {
			s.logger.Error("aggregate: rollup1m failed for monitor",
				"monitor_id", m.ID,
				"error", err,
			)
			// Continue with other monitors instead of failing the entire batch.
			continue
		}
	}
	return nil
}

func (s *AggregateService) rollup1mForMonitor(ctx context.Context, monitorID int64, from, to time.Time) error {
	heartbeats, err := s.heartbeats.ListByMonitor(ctx, monitorID, from, to)
	if err != nil {
		return fmt.Errorf("listing heartbeats for monitor %d: %w", monitorID, err)
	}

	// Group heartbeats into 1-minute buckets.
	buckets := groupByBucket(heartbeats, 1*time.Minute)

	for _, bucketTime := range sortedBucketTimes(buckets) {
		agg := computeAggregate(monitorID, bucketTime, buckets[bucketTime])
		if err := s.heartbeats.SaveAggregate1m(ctx, agg); err != nil {
			return fmt.Errorf("saving 1m aggregate for monitor %d at %s: %w",
				monitorID, bucketTime.Format(time.RFC3339), err)
		}
	}
	return nil
}

// sortedBucketTimes returns a bucket map's keys in chronological order.
//
// Ranging a map directly gives Go's randomized iteration order, so the rollups
// used to write their buckets in a different order on every run. Nothing about
// the stored aggregates depended on it — each row is keyed by (monitor, bucket) —
// but any observer that reads them back in write order (a test, or a future
// batch writer) sees a different sequence each time, which is how
// TestRollup1m_GroupsByMinute became a coin-flip. Writing oldest-first is both
// deterministic and the order a time series should be written in.
func sortedBucketTimes[T any](buckets map[time.Time]T) []time.Time {
	times := make([]time.Time, 0, len(buckets))
	for t := range buckets {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	return times
}

// Rollup1h computes 1-hour aggregates from 1-minute aggregates.
// It reads heartbeat_1m records in the [from, to) window, groups them by
// monitor+hour, sums the counts and recomputes ping stats, and saves to heartbeat_1h.
func (s *AggregateService) Rollup1h(ctx context.Context, from, to time.Time) error {
	monitors, err := s.monitors.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("listing active monitors: %w", err)
	}

	for _, m := range monitors {
		if err := s.rollup1hForMonitor(ctx, m.ID, from, to); err != nil {
			s.logger.Error("aggregate: rollup1h failed for monitor",
				"monitor_id", m.ID,
				"error", err,
			)
			continue
		}
	}
	return nil
}

func (s *AggregateService) rollup1hForMonitor(ctx context.Context, monitorID int64, from, _ time.Time) error {
	aggs, err := s.heartbeats.GetAggregate1m(ctx, monitorID, from)
	if err != nil {
		return fmt.Errorf("getting 1m aggregates for monitor %d: %w", monitorID, err)
	}

	// Group 1m aggregates into 1-hour buckets.
	buckets := groupAggsByBucket(aggs, 1*time.Hour)

	for _, bucketTime := range sortedBucketTimes(buckets) {
		agg := mergeAggregates1m(monitorID, bucketTime, buckets[bucketTime])
		if err := s.heartbeats.SaveAggregate1h(ctx, agg); err != nil {
			return fmt.Errorf("saving 1h aggregate for monitor %d at %s: %w",
				monitorID, bucketTime.Format(time.RFC3339), err)
		}
	}
	return nil
}

// Rollup1d computes 1-day aggregates from 1-hour aggregates.
// It reads heartbeat_1h records in the [from, to) window, groups them by
// monitor+day, sums the counts and recomputes ping stats, and saves to heartbeat_1d.
func (s *AggregateService) Rollup1d(ctx context.Context, from, to time.Time) error {
	monitors, err := s.monitors.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("listing active monitors: %w", err)
	}

	for _, m := range monitors {
		if err := s.rollup1dForMonitor(ctx, m.ID, from, to); err != nil {
			s.logger.Error("aggregate: rollup1d failed for monitor",
				"monitor_id", m.ID,
				"error", err,
			)
			continue
		}
	}
	return nil
}

func (s *AggregateService) rollup1dForMonitor(ctx context.Context, monitorID int64, from, _ time.Time) error {
	aggs, err := s.heartbeats.GetAggregate1h(ctx, monitorID, from)
	if err != nil {
		return fmt.Errorf("getting 1h aggregates for monitor %d: %w", monitorID, err)
	}

	// Group 1h aggregates into 1-day buckets.
	buckets := groupAggsByBucket1h(aggs, 24*time.Hour)

	for _, bucketTime := range sortedBucketTimes(buckets) {
		agg := mergeAggregates1h(monitorID, bucketTime, buckets[bucketTime])
		if err := s.heartbeats.SaveAggregate1d(ctx, agg); err != nil {
			return fmt.Errorf("saving 1d aggregate for monitor %d at %s: %w",
				monitorID, bucketTime.Format(time.RFC3339), err)
		}
	}
	return nil
}

// GetUptimePercent returns the uptime percentage for a monitor over a time range.
// It returns nil when the range contains no effective observations: no data is
// evidence for neither 0% nor 100%. It queries the heartbeat_1d table (falling back to heartbeat_1h or raw heartbeats)
// and computes: up_checks / (total_checks - maint_checks) * 100.
func (s *AggregateService) GetUptimePercent(ctx context.Context, monitorID int64, from, to time.Time) (*float64, error) {
	// Try daily aggregates first (most efficient for long ranges).
	aggs, err := s.heartbeats.GetAggregate1d(ctx, monitorID, from)
	if err != nil {
		return nil, fmt.Errorf("getting 1d aggregates for uptime: %w", err)
	}

	if len(aggs) > 0 {
		var totalUp, totalChecks, totalMaint int
		for _, a := range aggs {
			if !a.Bucket.Before(from) && a.Bucket.Before(to) {
				totalUp += a.UpCount
				totalChecks += a.TotalChecks
				totalMaint += a.MaintCount
			}
		}
		effective := totalChecks - totalMaint
		if effective <= 0 {
			return nil, nil
		}
		percent := (float64(totalUp) / float64(effective)) * 100.0
		return &percent, nil
	}

	// Fallback: compute from raw heartbeats if no aggregates exist yet.
	heartbeats, err := s.heartbeats.ListByMonitor(ctx, monitorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("listing heartbeats for uptime: %w", err)
	}

	if len(heartbeats) == 0 {
		return nil, nil
	}

	var upCount, maintCount int
	for _, h := range heartbeats {
		switch h.Status {
		case domain.StatusUp:
			upCount++
		case domain.StatusMaintenance:
			maintCount++
		}
	}

	effective := len(heartbeats) - maintCount
	if effective <= 0 {
		return nil, nil
	}
	percent := (float64(upCount) / float64(effective)) * 100.0
	return &percent, nil
}

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// groupByBucket groups raw heartbeats into time buckets of the given duration.
// The bucket key is the start of the interval (truncated to the duration).
func groupByBucket(heartbeats []*domain.Heartbeat, bucketSize time.Duration) map[time.Time][]*domain.Heartbeat {
	buckets := make(map[time.Time][]*domain.Heartbeat)
	for _, h := range heartbeats {
		bucket := h.Time.Truncate(bucketSize)
		buckets[bucket] = append(buckets[bucket], h)
	}
	return buckets
}

// groupAggsByBucket groups Aggregate1m records into 1-hour buckets.
func groupAggsByBucket(aggs []*ports.Aggregate1m, bucketSize time.Duration) map[time.Time][]*ports.Aggregate1m {
	buckets := make(map[time.Time][]*ports.Aggregate1m)
	for _, a := range aggs {
		bucket := a.Bucket.Truncate(bucketSize)
		buckets[bucket] = append(buckets[bucket], a)
	}
	return buckets
}

// groupAggsByBucket1h groups Aggregate1h records into 1-day buckets.
func groupAggsByBucket1h(aggs []*ports.Aggregate1h, bucketSize time.Duration) map[time.Time][]*ports.Aggregate1h {
	buckets := make(map[time.Time][]*ports.Aggregate1h)
	for _, a := range aggs {
		bucket := a.Bucket.Truncate(bucketSize)
		buckets[bucket] = append(buckets[bucket], a)
	}
	return buckets
}

// computeAggregate computes an Aggregate1m from a slice of raw heartbeats.
func computeAggregate(monitorID int64, bucket time.Time, hbs []*domain.Heartbeat) *ports.Aggregate1m {
	agg := &ports.Aggregate1m{
		MonitorID: monitorID,
		Bucket:    bucket,
	}

	if len(hbs) == 0 {
		return agg
	}

	agg.TotalChecks = len(hbs)
	agg.MinPing = math.MaxInt32

	var pingSum int
	var pingCount int

	for _, h := range hbs {
		switch h.Status {
		case domain.StatusUp:
			agg.UpCount++
		case domain.StatusDown:
			agg.DownCount++
		case domain.StatusPending:
			agg.PendingCount++
		case domain.StatusMaintenance:
			agg.MaintCount++
		}

		if h.Ping > 0 {
			pingSum += h.Ping
			pingCount++
			if h.Ping < agg.MinPing {
				agg.MinPing = h.Ping
			}
			if h.Ping > agg.MaxPing {
				agg.MaxPing = h.Ping
			}
		}
	}

	if pingCount > 0 {
		agg.PingCount = pingCount
		agg.AvgPing = float64(pingSum) / float64(pingCount)
	}
	if agg.MinPing == math.MaxInt32 {
		agg.MinPing = 0
	}

	return agg
}

// mergeAggregates1m merges multiple Aggregate1m records into a single Aggregate1h.
func mergeAggregates1m(monitorID int64, bucket time.Time, items []*ports.Aggregate1m) *ports.Aggregate1h {
	agg := &ports.Aggregate1h{
		MonitorID: monitorID,
		Bucket:    bucket,
	}

	if len(items) == 0 {
		return agg
	}

	agg.MinPing = math.MaxInt32
	var pingSum float64
	var pingCount int

	for _, item := range items {
		agg.UpCount += item.UpCount
		agg.DownCount += item.DownCount
		agg.PendingCount += item.PendingCount
		agg.MaintCount += item.MaintCount
		agg.TotalChecks += item.TotalChecks
		sampleCount := item.PingCount
		// Rows created before migration 026 have no ping_count. Preserve their
		// existing average during the compatibility window; newly written rows
		// always carry the exact sample count.
		if sampleCount == 0 && item.AvgPing > 0 {
			sampleCount = item.TotalChecks
		}
		if item.AvgPing > 0 && sampleCount > 0 {
			pingSum += item.AvgPing * float64(sampleCount)
			pingCount += sampleCount
		}
		if item.MinPing > 0 && item.MinPing < agg.MinPing {
			agg.MinPing = item.MinPing
		}
		if item.MaxPing > agg.MaxPing {
			agg.MaxPing = item.MaxPing
		}
	}

	if pingCount > 0 {
		agg.PingCount = pingCount
		agg.AvgPing = pingSum / float64(pingCount)
	}
	if agg.MinPing == math.MaxInt32 {
		agg.MinPing = 0
	}

	return agg
}

// mergeAggregates1h merges multiple Aggregate1h records into a single Aggregate1d.
func mergeAggregates1h(monitorID int64, bucket time.Time, items []*ports.Aggregate1h) *ports.Aggregate1d {
	agg := &ports.Aggregate1d{
		MonitorID: monitorID,
		Bucket:    bucket,
	}

	if len(items) == 0 {
		return agg
	}

	agg.MinPing = math.MaxInt32
	var pingSum float64
	var pingCount int

	for _, item := range items {
		agg.UpCount += item.UpCount
		agg.DownCount += item.DownCount
		agg.PendingCount += item.PendingCount
		agg.MaintCount += item.MaintCount
		agg.TotalChecks += item.TotalChecks
		sampleCount := item.PingCount
		if sampleCount == 0 && item.AvgPing > 0 {
			sampleCount = item.TotalChecks
		}
		if item.AvgPing > 0 && sampleCount > 0 {
			pingSum += item.AvgPing * float64(sampleCount)
			pingCount += sampleCount
		}
		if item.MinPing > 0 && item.MinPing < agg.MinPing {
			agg.MinPing = item.MinPing
		}
		if item.MaxPing > agg.MaxPing {
			agg.MaxPing = item.MaxPing
		}
	}

	if pingCount > 0 {
		agg.PingCount = pingCount
		agg.AvgPing = pingSum / float64(pingCount)
	}
	if agg.MinPing == math.MaxInt32 {
		agg.MinPing = 0
	}

	return agg
}
