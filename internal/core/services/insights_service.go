package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// InsightsPeriod is a selectable rolling window for the reliability page.
type InsightsPeriod string

const (
	Period24h InsightsPeriod = "24h"
	Period7d  InsightsPeriod = "7d"
	Period30d InsightsPeriod = "30d"
	Period90d InsightsPeriod = "90d"
)

// InsightsMetric is the primary ranking metric. It only controls ordering; every
// row always carries every metric so the frontend can re-sort without a refetch.
type InsightsMetric string

// ErrInsightsLatencyTypeRequired prevents a meaningless cross-type latency ranking.
var ErrInsightsLatencyTypeRequired = errors.New("latency rankings require a monitor type")

const insightsCoverageBasis = "observation_based"

const (
	MetricAvailability InsightsMetric = "availability"
	MetricOutages      InsightsMetric = "outages"
	MetricDowntime     InsightsMetric = "downtime"
	MetricLatency      InsightsMetric = "latency"
	MetricFlapping     InsightsMetric = "flapping"
)

// ParsePeriod normalises a query value into a supported period, defaulting to 7d.
func ParsePeriod(raw string) InsightsPeriod {
	switch InsightsPeriod(strings.TrimSpace(raw)) {
	case Period24h:
		return Period24h
	case Period30d:
		return Period30d
	case Period90d:
		return Period90d
	default:
		return Period7d
	}
}

// ParseMetric normalises a query value into a supported metric, defaulting to
// availability (the "needs attention" default view).
func ParseMetric(raw string) InsightsMetric {
	switch InsightsMetric(strings.TrimSpace(raw)) {
	case MetricOutages:
		return MetricOutages
	case MetricDowntime:
		return MetricDowntime
	case MetricLatency:
		return MetricLatency
	case MetricFlapping:
		return MetricFlapping
	default:
		return MetricAvailability
	}
}

func (p InsightsPeriod) duration() time.Duration {
	switch p {
	case Period24h:
		return 24 * time.Hour
	case Period30d:
		return 30 * 24 * time.Hour
	case Period90d:
		return 90 * 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}

// InsightsQuery is the request to the reliability read model.
type InsightsQuery struct {
	UserID  int64
	Period  InsightsPeriod
	Metric  InsightsMetric
	Type    string // optional monitor-type filter ("" = all)
	GroupID *int64 // optional group filter (includes descendant groups)
}

// InsightsRow is the computed reliability of one monitor over the window.
type InsightsRow struct {
	MonitorID   int64
	MonitorName string
	MonitorType string
	GroupID     *int64

	AvailabilityPercent *float64
	OutageCount         int
	DowntimeSeconds     int64
	FlapCount           int

	LatencyAvgMs    *float64
	LatencySampleN  int64
	CoveragePercent float64
	Qualification   ReliabilityQualification
}

// InsightsResult is the full response for the page.
type InsightsResult struct {
	From          time.Time
	To            time.Time
	Period        InsightsPeriod
	Metric        InsightsMetric
	CoverageBasis string
	Rows          []InsightsRow
}

// InsightsService computes the reliability ranking read model. It is the single
// place authorization, windowing, and metric contracts live for the page.
type InsightsService struct {
	reliability ports.ReliabilityReader
	aggregates  ports.AggregateBatchReader
	monitors    ports.MonitorRepository
	groups      ports.MonitorGroupRepository
	access      *AccessService
	now         func() time.Time
}

// NewInsightsService wires the reliability read model.
func NewInsightsService(
	reliability ports.ReliabilityReader,
	aggregates ports.AggregateBatchReader,
	monitors ports.MonitorRepository,
	groups ports.MonitorGroupRepository,
	access *AccessService,
) *InsightsService {
	return &InsightsService{
		reliability: reliability,
		aggregates:  aggregates,
		monitors:    monitors,
		groups:      groups,
		access:      access,
		now:         func() time.Time { return time.Now().UTC() },
	}
}

// GetInsights resolves visible monitors, computes per-monitor reliability over
// the window, and returns them ranked by the requested metric.
//
// Authorization is applied BEFORE any aggregation (INSIGHTS-PAGE-REVIEW-2.md §7):
// a non-admin with zero grants receives an empty result, never an install-wide
// ranking. Inaccessible monitors are simply never loaded.
func (s *InsightsService) GetInsights(ctx context.Context, q InsightsQuery) (*InsightsResult, error) {
	period := ParsePeriod(string(q.Period))
	metric := ParseMetric(string(q.Metric))
	to := s.now().UTC()
	from := to.Add(-period.duration())

	result := &InsightsResult{
		From:          from,
		To:            to,
		Period:        period,
		Metric:        metric,
		CoverageBasis: insightsCoverageBasis,
		Rows:          []InsightsRow{},
	}
	if metric == MetricLatency && strings.TrimSpace(q.Type) == "" {
		return nil, ErrInsightsLatencyTypeRequired
	}
	if s.access == nil || s.reliability == nil || s.aggregates == nil || s.monitors == nil {
		return nil, fmt.Errorf("insights: read model is not fully wired")
	}

	all, visibleIDs, err := s.access.VisibleMonitorIDs(ctx, q.UserID)
	if err != nil {
		return nil, fmt.Errorf("insights: resolve visibility: %w", err)
	}
	if !all && len(visibleIDs) == 0 {
		return result, nil // zero grants → empty, not unfiltered
	}

	filter := ports.MonitorFilter{Type: strings.TrimSpace(q.Type)}
	if !all {
		filter.RestrictToIDs = true
		filter.MonitorIDs = visibleIDs
	}
	monitors, err := s.monitors.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("insights: list monitors: %w", err)
	}

	// Optional group filter, including descendant groups (same recursive
	// semantics as the rest of Phoenix). First verify the selected group itself
	// is visible; otherwise a direct group_id probe could confirm a hidden group
	// through the rows it returns.
	if q.GroupID != nil {
		if s.groups == nil {
			return nil, fmt.Errorf("insights: group read model is not wired")
		}
		allGroups, visibleGroupIDs, err := s.access.VisibleGroupIDs(ctx, q.UserID)
		if err != nil {
			return nil, fmt.Errorf("insights: resolve group visibility: %w", err)
		}
		if !allGroups && !containsInt64(visibleGroupIDs, *q.GroupID) {
			monitors = nil
		} else {
			allowed, err := s.groupAndDescendants(ctx, *q.GroupID)
			if err != nil {
				return nil, fmt.Errorf("insights: resolve group tree: %w", err)
			}
			filtered := monitors[:0]
			for _, m := range monitors {
				if m.GroupID != nil && allowed[*m.GroupID] {
					filtered = append(filtered, m)
				}
			}
			monitors = filtered
		}
	}

	monitorIDs := make([]int64, 0, len(monitors))
	for _, m := range monitors {
		monitorIDs = append(monitorIDs, m.ID)
	}
	transitionsByMonitor, err := s.reliability.ListImportantForMonitors(ctx, monitorIDs, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("insights: list transitions: %w", err)
	}
	leadingByMonitor, err := s.reliability.LatestImportantBeforeForMonitors(ctx, monitorIDs, from.UTC())
	if err != nil {
		return nil, fmt.Errorf("insights: list leading states: %w", err)
	}

	var latencyByMonitor map[int64][]*ports.Aggregate1h
	var dailyLatencyByMonitor map[int64][]*ports.Aggregate1d
	if period == Period24h {
		latencyByMonitor, err = s.aggregates.GetAggregate1hForMonitors(ctx, monitorIDs, from.UTC())
	} else {
		dailyLatencyByMonitor, err = s.aggregates.GetAggregate1dForMonitors(ctx, monitorIDs, from.UTC())
	}
	if err != nil {
		return nil, fmt.Errorf("insights: list latency rollups: %w", err)
	}

	rows := make([]InsightsRow, 0, len(monitors))
	for _, m := range monitors {
		rows = append(rows, s.computeRow(
			m,
			from,
			to,
			transitionsByMonitor[m.ID],
			leadingByMonitor[m.ID],
			latencyByMonitor[m.ID],
			dailyLatencyByMonitor[m.ID],
		))
	}

	sortRows(rows, metric)
	result.Rows = rows
	return result, nil
}

func (s *InsightsService) computeRow(
	m *domain.Monitor,
	from, to time.Time,
	transitions []*domain.Heartbeat,
	lead *domain.Heartbeat,
	hourly []*ports.Aggregate1h,
	daily []*ports.Aggregate1d,
) InsightsRow {
	observedSeconds := observationSecondsFromRollups(hourly, daily, from, to, m.Interval)
	observationCount := observationCountFromRollups(hourly, daily, from, to)
	in := ReliabilityInput{
		From:        from.UTC(),
		To:          to.UTC(),
		Transitions: transitions,
	}
	// Rollups cap timeline coverage when they actually observed the window.
	// Passing &0 for an empty read model used to wipe KnownSeconds — a
	// days-old UP monitor then ranked as insufficient_data because
	// heartbeat_1h/1d had not been catch-up rolled yet, not because it was
	// unobserved. nil means "no rollup data"; ComputeReliability then trusts
	// the leading+transition timeline.
	if observationCount > 0 {
		in.ObservationSeconds = &observedSeconds
		in.ObservationCount = observationCount
	}
	if lead != nil {
		st := lead.Status
		in.Leading = &st
		in.LeadingConfirmedDown = lead.Status == domain.StatusDown
	}

	metrics := ComputeReliability(in)
	avg, sampleN := latencyFromRollups(hourly, daily, from, to)

	return InsightsRow{
		MonitorID:           m.ID,
		MonitorName:         m.Name,
		MonitorType:         m.Type,
		GroupID:             m.GroupID,
		AvailabilityPercent: metrics.AvailabilityPercent,
		OutageCount:         metrics.OutageCount,
		DowntimeSeconds:     int64(metrics.DownSeconds),
		FlapCount:           metrics.FlapCount,
		LatencyAvgMs:        avg,
		LatencySampleN:      sampleN,
		CoveragePercent:     metrics.CoveragePercent,
		Qualification:       metrics.Qualification,
	}
}

// observationSecondsFromRollups estimates trustworthy observation coverage from
// rollup check counts and the monitor's configured interval. It is explicitly
// observation-based: Phoenix does not yet persist a monitor configuration
// timeline, so historical interval changes cannot be reconstructed exactly.
func observationSecondsFromRollups(hourly []*ports.Aggregate1h, daily []*ports.Aggregate1d, from, to time.Time, intervalSeconds int) float64 {
	if intervalSeconds <= 0 {
		intervalSeconds = 60
	}
	interval := float64(intervalSeconds)
	var observed float64
	for _, a := range hourly {
		observed += observedBucketSeconds(a.Bucket, time.Hour, a.TotalChecks, interval, from, to)
	}
	for _, a := range daily {
		observed += observedBucketSeconds(a.Bucket, 24*time.Hour, a.TotalChecks, interval, from, to)
	}
	return observed
}

func observationCountFromRollups(hourly []*ports.Aggregate1h, daily []*ports.Aggregate1d, from, to time.Time) int {
	count := 0
	for _, a := range hourly {
		if bucketOverlaps(a.Bucket, time.Hour, from, to) {
			count += a.TotalChecks
		}
	}
	for _, a := range daily {
		if bucketOverlaps(a.Bucket, 24*time.Hour, from, to) {
			count += a.TotalChecks
		}
	}
	return count
}

func bucketOverlaps(bucket time.Time, width time.Duration, from, to time.Time) bool {
	start := bucket.UTC()
	end := start.Add(width)
	return start.Before(to) && end.After(from)
}

func observedBucketSeconds(bucket time.Time, bucketWidth time.Duration, checks int, interval float64, from, to time.Time) float64 {
	if checks <= 0 {
		return 0
	}
	start := bucket.UTC()
	end := start.Add(bucketWidth)
	if start.Before(from) {
		start = from
	}
	if end.After(to) {
		end = to
	}
	if !end.After(start) {
		return 0
	}
	observed := float64(checks) * interval
	width := end.Sub(start).Seconds()
	if observed > width {
		return width
	}
	return observed
}

// latencyFromRollups merges only buckets with an exact latency sample count.
// The old aggregate rows have ping_count=0 after migration and are therefore
// reported as unavailable rather than pretending TotalChecks was a latency
// sample count. p95 remains intentionally deferred for v1.
func latencyFromRollups(hourly []*ports.Aggregate1h, daily []*ports.Aggregate1d, from, to time.Time) (*float64, int64) {
	var weighted float64
	var samples int64
	for _, a := range hourly {
		if a.Bucket.Before(from) || !a.Bucket.Before(to) || a.PingCount <= 0 || a.AvgPing <= 0 {
			continue
		}
		weighted += a.AvgPing * float64(a.PingCount)
		samples += int64(a.PingCount)
	}
	for _, a := range daily {
		if a.Bucket.Before(from) || !a.Bucket.Before(to) || a.PingCount <= 0 || a.AvgPing <= 0 {
			continue
		}
		weighted += a.AvgPing * float64(a.PingCount)
		samples += int64(a.PingCount)
	}
	if samples == 0 {
		return nil, 0
	}
	avg := weighted / float64(samples)
	return &avg, samples
}

// groupAndDescendants returns the set of group IDs consisting of the selected
// group and every group nested beneath it.
func (s *InsightsService) groupAndDescendants(ctx context.Context, groupID int64) (map[int64]bool, error) {
	groups, err := s.groups.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	children := make(map[int64][]int64)
	for _, g := range groups {
		if g.ParentID != nil {
			children[*g.ParentID] = append(children[*g.ParentID], g.ID)
		}
	}
	allowed := make(map[int64]bool)
	var walk func(id int64)
	walk = func(id int64) {
		if allowed[id] {
			return
		}
		allowed[id] = true
		for _, c := range children[id] {
			walk(c)
		}
	}
	walk(groupID)
	return allowed, nil
}

// sortRows orders rows for the requested metric. Insufficient-data rows always
// sink below qualified rows so a monitor with no trustworthy data can never
// occupy a strongest/weakest position (INSIGHTS-PAGE-REVIEW-2.md). Tie-breaks
// are deterministic and per-metric.
func sortRows(rows []InsightsRow, metric InsightsMetric) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]

		aQual := a.Qualification == QualificationQualified
		bQual := b.Qualification == QualificationQualified
		if aQual != bQual {
			return aQual // qualified rows first
		}

		switch metric {
		case MetricOutages:
			if a.OutageCount != b.OutageCount {
				return a.OutageCount > b.OutageCount
			}
			if a.DowntimeSeconds != b.DowntimeSeconds {
				return a.DowntimeSeconds > b.DowntimeSeconds
			}
		case MetricDowntime:
			if a.DowntimeSeconds != b.DowntimeSeconds {
				return a.DowntimeSeconds > b.DowntimeSeconds
			}
			if a.OutageCount != b.OutageCount {
				return a.OutageCount > b.OutageCount
			}
		case MetricLatency:
			al, bl := latencyOrElse(a.LatencyAvgMs, -1), latencyOrElse(b.LatencyAvgMs, -1)
			if al != bl {
				return al > bl // slowest first
			}
		case MetricFlapping:
			if a.FlapCount != b.FlapCount {
				return a.FlapCount > b.FlapCount
			}
			if a.DowntimeSeconds != b.DowntimeSeconds {
				return a.DowntimeSeconds > b.DowntimeSeconds
			}
		default: // availability — worst first (needs attention)
			av, bv := availabilityOrElse(a.AvailabilityPercent, 101), availabilityOrElse(b.AvailabilityPercent, 101)
			if av != bv {
				return av < bv
			}
			if a.DowntimeSeconds != b.DowntimeSeconds {
				return a.DowntimeSeconds > b.DowntimeSeconds
			}
		}

		if a.CoveragePercent != b.CoveragePercent {
			return a.CoveragePercent > b.CoveragePercent // higher coverage wins ties
		}
		if an, bn := strings.ToLower(a.MonitorName), strings.ToLower(b.MonitorName); an != bn {
			return an < bn
		}
		return a.MonitorID < b.MonitorID
	})
}

func containsInt64(values []int64, wanted int64) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func availabilityOrElse(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}

func latencyOrElse(v *float64, fallback float64) float64 {
	if v == nil {
		return fallback
	}
	return *v
}
