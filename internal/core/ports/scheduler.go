package ports

import (
	"context"
	"time"
)

// Scheduler defines the interface for scheduling monitor checks.
type Scheduler interface {
	Run(ctx context.Context) error
}

// CronEvaluator evaluates whether a cron expression is currently within an active window.
// This port keeps cron library usage out of the core/services layer.
type CronEvaluator interface {
	// IsWindowActive returns true if `now` falls within the half-open interval
	// [lastScheduledStart, lastScheduledStart + durationMinutes) evaluated in loc.
	// A nil loc is treated as UTC. Fixed single windows are NOT evaluated here —
	// only cron strategy uses this port.
	IsWindowActive(cronExpr string, durationMinutes int, now time.Time, loc *time.Location) bool
}
