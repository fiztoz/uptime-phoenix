package services

import (
	"context"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// insightsProjectionReader supplies the batched overall read model. Both methods
// are one query per id chunk. A nil reader leaves Insights on the heartbeat path.
type insightsProjectionReader interface {
	ListProjectionVersions(ctx context.Context, monitorIDs []int64) (map[int64]int64, error)
	ListHealthHistoryForMonitors(ctx context.Context, monitorIDs []int64, from, to time.Time) (map[int64][]domain.MonitorHealthInterval, error)
}

// SetProjectionReader invalidates Insights when overall versions change and
// ranks monitors that have overall history by that history. Optional.
func (s *InsightsService) SetProjectionReader(r insightsProjectionReader) {
	s.projections = r
}

// applyOverallProjection replaces pooled heartbeat reliability with policy
// durations. Latency is cleared: an overall ranking must not average pings
// from different vantage points. An empty interval list leaves the row alone.
func applyOverallProjection(row InsightsRow, intervals []domain.MonitorHealthInterval, from, to time.Time) InsightsRow {
	if len(intervals) == 0 {
		return row
	}
	from, to = from.UTC(), to.UTC()
	if !from.Before(to) {
		return row
	}
	ordered := append([]domain.MonitorHealthInterval(nil), intervals...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].From.Equal(ordered[j].From) {
			return ordered[i].To.Before(ordered[j].To)
		}
		return ordered[i].From.Before(ordered[j].From)
	})

	var durations domain.HealthDurations
	var covered time.Duration
	var previous domain.Status
	var havePrevious bool
	outages, flaps := 0, 0
	flapFrom := to.Add(-reliabilityFlapWindow)
	if flapFrom.Before(from) {
		flapFrom = from
	}
	for _, interval := range ordered {
		start, end := interval.From.UTC(), interval.To.UTC()
		if end.IsZero() || end.After(to) {
			end = to
		}
		clipped := start
		if clipped.Before(from) {
			clipped = from
		}
		if clipped.Before(end) {
			dur := end.Sub(clipped)
			covered += dur
			switch interval.Status {
			case domain.StatusUp:
				durations.Up += dur
			case domain.StatusDown:
				durations.Down += dur
			case domain.StatusPending:
				durations.Pending += dur
			case domain.StatusMaintenance:
				durations.Maintenance += dur
			default:
				durations.Unknown += dur
			}
		}
		if interval.Status == domain.StatusDown && !start.Before(from) && start.Before(to) {
			outages++
		}
		if havePrevious && previous != interval.Status {
			change := start
			if !change.Before(flapFrom) && change.Before(to) && confirmedFlip(previous, interval.Status) {
				flaps++
			}
		}
		previous = interval.Status
		havePrevious = true
	}
	if covered == 0 {
		return row
	}
	if gap := to.Sub(from) - covered; gap > 0 {
		durations.Unknown += gap
	}
	coverage, err := CalculateHealthCoverage(durations)
	if err != nil {
		return row
	}
	row.AvailabilityPercent = coverage.UptimePercent
	row.DowntimeSeconds = int64(durations.Down / time.Second)
	if coverage.CoveragePercent != nil {
		row.CoveragePercent = *coverage.CoveragePercent
	} else {
		row.CoveragePercent = 0
	}
	row.OutageCount = outages
	row.FlapCount = flaps
	row.LatencyAvgMs = nil
	row.LatencySampleN = 0
	if coverage.Known > 0 && coverage.CoveragePercent != nil && *coverage.CoveragePercent >= MinCoverageToQualify*100 {
		row.Qualification = QualificationQualified
	} else {
		row.Qualification = QualificationInsufficient
	}
	return row
}

func confirmedFlip(previous, current domain.Status) bool {
	return (previous == domain.StatusUp && current == domain.StatusDown) ||
		(previous == domain.StatusDown && current == domain.StatusUp)
}
