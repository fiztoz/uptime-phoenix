// Package scheduler provides adapters for the ports.Scheduler and ports.CronEvaluator interfaces.
package scheduler

import (
	"time"

	"github.com/robfig/cron/v3"
)

// CronEvaluator implements ports.CronEvaluator using robfig/cron/v3.
type CronEvaluator struct{}

// NewCronEvaluator creates a new cron evaluator.
func NewCronEvaluator() *CronEvaluator {
	return &CronEvaluator{}
}

// IsWindowActive parses the cron expression and checks if now is within an
// active half-open window [start, start+duration) after a scheduled start,
// evaluated in loc. A nil loc is treated as UTC.
//
// Cron schedules are location-aware: "0 2 * * *" in Asia/Bangkok means 02:00
// Bangkok time, not 02:00 UTC. Fixed single windows are handled by the service
// against absolute UTC bounds and never reach this method.
func (CronEvaluator) IsWindowActive(cronExpr string, durationMinutes int, now time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.UTC
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parser.Parse(cronExpr)
	if err != nil {
		return false
	}

	// Evaluate in the window's location so wall-clock hours match the operator's intent.
	nowLocal := now.In(loc)

	// Walk backwards from now to find the most recent scheduled start.
	// Use a bounded search (last 30 days in the same location).
	searchStart := nowLocal.Add(-30 * 24 * time.Hour)
	lastStart := searchStart
	for {
		next := sched.Next(lastStart)
		if next.After(nowLocal) {
			break
		}
		lastStart = next
	}
	if lastStart.IsZero() || lastStart.Equal(searchStart) {
		return false
	}
	end := lastStart.Add(time.Duration(durationMinutes) * time.Minute)
	// Half-open [start, end): inclusive start, exclusive end.
	return !nowLocal.Before(lastStart) && nowLocal.Before(end)
}
