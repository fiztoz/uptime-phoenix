package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// BenchmarkInsightsReadPath compares identical service calculations with a
// controlled 2ms delay per repository read. It isolates concurrency and cache
// gains; it is not a MariaDB query-plan or production API latency measurement.
func BenchmarkInsightsReadPath(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		for _, period := range []InsightsPeriod{Period24h, Period7d, Period30d, Period90d} {
			b.Run(fmt.Sprintf("monitors=%d/period=%s", n, period), func(b *testing.B) {
				monitors := make([]*domain.Monitor, n)
				ids := make([]int64, n)
				for i := range monitors {
					ids[i] = int64(i + 1)
					monitors[i] = &domain.Monitor{ID: ids[i], Name: fmt.Sprintf("Monitor %d", i), Type: "http", Interval: 60}
				}
				p := &insightsReadProbe{read: func(ctx context.Context, _ int, _ []int64, _ time.Time) error {
					timer := time.NewTimer(2 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-timer.C:
						return nil
					}
				}}
				s := NewInsightsService(p, p, nil, nil, nil)
				now := time.Now().UTC()
				s.now = func() time.Time { return now }
				ctx := context.Background()
				newResult := func() *InsightsResult {
					return &InsightsResult{From: now.Add(-period.duration()), To: now, Period: period, CoverageBasis: insightsCoverageBasis}
				}
				b.Run("sequential", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						result := newResult()
						transitions, err := p.ListImportantForMonitors(ctx, ids, result.From, result.To)
						if err != nil {
							b.Fatal(err)
						}
						leading, err := p.LatestImportantBeforeForMonitors(ctx, ids, result.From)
						if err != nil {
							b.Fatal(err)
						}
						var hourly map[int64][]*ports.Aggregate1h
						var daily map[int64][]*ports.Aggregate1d
						if period == Period24h {
							hourly, err = p.GetAggregate1hForMonitors(ctx, ids, result.From)
						} else {
							daily, err = p.GetAggregate1dForMonitors(ctx, ids, result.From)
						}
						if err != nil {
							b.Fatal(err)
						}
						result.Rows = make([]InsightsRow, 0, n)
						for _, m := range monitors {
							result.Rows = append(result.Rows, s.computeRow(m, result.From, result.To, transitions[m.ID], leading[m.ID], hourly[m.ID], daily[m.ID]))
						}
						sortRows(result.Rows, MetricAvailability)
					}
				})
				b.Run("concurrent-cold", func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						key := newInsightsCacheKey(1, period, "http", nil, monitors)
						// Reset only this benchmark's process-local cache, retaining
						// the same data/window as the sequential reference.
						s.cache = insightsCache{}
						result, err := s.cachedInsights(ctx, key, func(ctx context.Context) (*InsightsResult, error) {
							return s.calculateInsights(ctx, newResult(), monitors)
						})
						if err != nil {
							b.Fatal(err)
						}
						sortRows(cloneInsightsResult(result).Rows, MetricAvailability)
					}
				})
				b.Run("cached", func(b *testing.B) {
					key := newInsightsCacheKey(1, period, "http", nil, monitors)
					calculate := func(ctx context.Context) (*InsightsResult, error) {
						return s.calculateInsights(ctx, newResult(), monitors)
					}
					if _, err := s.cachedInsights(ctx, key, calculate); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					for b.Loop() {
						key = newInsightsCacheKey(1, period, "http", nil, monitors)
						result, err := s.cachedInsights(ctx, key, calculate)
						if err != nil {
							b.Fatal(err)
						}
						sortRows(cloneInsightsResult(result).Rows, MetricAvailability)
					}
				})
			})
		}
	}
}
