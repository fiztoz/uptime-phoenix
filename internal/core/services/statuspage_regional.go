package services

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// populatePublicRegionalHealth never publishes regional identity, endpoints or
// measured latency. A policy timeline is availability evidence, not a ping series.
func (s *StatusPageService) populatePublicRegionalHealth(ctx context.Context, id int64, view *PublicMonitorStatus, now time.Time) bool {
	if s.regionalHealth == nil {
		return false
	}
	_, set, _, err := s.regionalHealth.loadMonitorEvidence(ctx, id)
	if err != nil {
		view.Status = "unknown"
		return true
	}
	remote := false
	for _, a := range set.Assignments {
		if a.ProbeID != domain.LocalProbeID {
			remote = true
		}
	}
	if !remote {
		return false
	}
	view.UptimeData = []PublicUptimeDay{}
	view.UptimeHistory = PublicUptimeHistory{Monthly: []PublicUptimePeriod{}, Quarterly: []PublicUptimePeriod{}}
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	quarterStart := time.Date(now.Year(), time.Month(((int(now.Month())-1)/3)*3+1), 1, 0, 0, 0, 0, time.UTC)
	from := monthStart.AddDate(0, -(publicUptimeHistoryMonths - 1), 0)
	if q := quarterStart.AddDate(0, -3*(publicUptimeHistoryQuarters-1), 0); q.Before(from) {
		from = q
	}
	history, err := s.regionalHealth.reconstruct(ctx, id, from, now, now)
	if err != nil {
		slog.Error("public overall history unavailable", "monitor_id", id, "error", err)
		return true
	}
	byDay := overallPublicDays(history.Intervals)
	today := now.Truncate(24 * time.Hour)
	barFrom := today.AddDate(0, 0, -(publicUptimeBarDays - 1))
	for i := 0; i < publicUptimeBarDays; i++ {
		day := barFrom.AddDate(0, 0, i).Format(time.DateOnly)
		view.UptimeData = append(view.UptimeData, PublicUptimeDay{Date: day, Status: dayStatus(byDay[day])})
	}
	period := uptimePeriod("", barFrom, today, today, byDay)
	view.UptimePercent = period.UptimePercent
	for i := 0; i < publicUptimeHistoryMonths; i++ {
		start := monthStart.AddDate(0, -i, 0)
		view.UptimeHistory.Monthly = append(view.UptimeHistory.Monthly, uptimePeriod(start.Format("January 2006"), start, start.AddDate(0, 1, -1), today, byDay))
	}
	for i := 0; i < publicUptimeHistoryQuarters; i++ {
		start := quarterStart.AddDate(0, -3*i, 0)
		view.UptimeHistory.Quarterly = append(view.UptimeHistory.Quarterly, uptimePeriod(fmt.Sprintf("Q%d %d", (int(start.Month())-1)/3+1, start.Year()), start, start.AddDate(0, 3, -1), today, byDay))
	}
	var known, unknown time.Duration
	window := now.Add(-24 * time.Hour)
	for _, interval := range history.Intervals {
		start, end := interval.From, interval.To
		if start.Before(window) {
			start = window
		}
		if end.After(now) {
			end = now
		}
		if !start.Before(end) {
			continue
		}
		switch interval.Status {
		case domain.StatusUp, domain.StatusDown, domain.StatusPending:
			known += end.Sub(start)
		case domain.StatusUnknown:
			unknown += end.Sub(start)
		}
	}
	if eligible := known + unknown; eligible > 0 {
		value := 100 * float64(known) / float64(eligible)
		view.CoveragePercent = &value
	}
	return true
}

func overallPublicDays(intervals []domain.MonitorHealthInterval) map[string]*dayCounts {
	byDay := make(map[string]*dayCounts)
	for _, interval := range intervals {
		for start := interval.From.UTC(); start.Before(interval.To); {
			end := start.Truncate(24 * time.Hour).Add(24 * time.Hour)
			if end.After(interval.To) {
				end = interval.To
			}
			key := start.Format(time.DateOnly)
			counts := byDay[key]
			if counts == nil {
				counts = &dayCounts{}
				byDay[key] = counts
			}
			weight := int(end.Sub(start) / time.Microsecond)
			counts.total += weight
			switch interval.Status {
			case domain.StatusUp:
				counts.up += weight
			case domain.StatusDown:
				counts.down += weight
			case domain.StatusPending:
				counts.pending += weight
			case domain.StatusMaintenance:
				counts.maint += weight
			default:
				counts.unknown += weight
			}
			start = end
		}
	}
	return byDay
}
