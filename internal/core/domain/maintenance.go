package domain

import "time"

// MaintenanceWindow represents a scheduled maintenance period.
type MaintenanceWindow struct {
	ID          int64
	UserID      int64
	Title       string
	Description string
	Active      bool
	Strategy    string // "single" or "cron"
	StartDate   time.Time
	EndDate     time.Time
	CronExpr    string
	Duration    int // minutes (for cron strategy)
	// Timezone is an IANA name used when evaluating CronExpr (e.g. "Asia/Bangkok").
	// Empty means UTC for backward compatibility with pre-013 windows.
	// Fixed single windows remain absolute UTC instants regardless of Timezone.
	Timezone string
}
