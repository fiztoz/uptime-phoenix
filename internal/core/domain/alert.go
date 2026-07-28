package domain

import "time"

// Alert lifecycle statuses. An alert opens on confirmed DOWN, may be
// acknowledged while still open, and resolves on recovery (or never — a
// monitor can stay DOWN forever with a firing/acked alert).
const (
	AlertStatusFiring   = "firing"
	AlertStatusAcked    = "acked"
	AlertStatusResolved = "resolved"
)

// Alert is a monitor-level outage record with an acknowledgement lifecycle.
//
// One open alert (firing or acked) exists per monitor at a time. That uniqueness
// is enforced by OpenMonitorID (set to MonitorID while open, NULL when resolved)
// with a UNIQUE constraint — MariaDB/SQLite both allow multiple NULL values in a
// unique column, so resolved rows never block the next outage.
//
// AckToken is a high-entropy secret used by the unauthenticated deep-link ack
// path. It is never returned on list endpoints; only the authenticated Get and
// the deep-link flow consume it.
type Alert struct {
	ID            int64
	MonitorID     int64
	Status        string // firing | acked | resolved
	Message       string
	FiredAt       time.Time
	AckedAt       *time.Time
	AckedByUserID *int64
	ResolvedAt    *time.Time
	AckToken      string
	// OpenMonitorID is MonitorID while the alert is open (firing/acked) and nil
	// once resolved. See the uniqueness note above.
	OpenMonitorID *int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// IsOpen reports whether the alert still represents an ongoing outage.
func (a *Alert) IsOpen() bool {
	if a == nil {
		return false
	}
	return a.Status == AlertStatusFiring || a.Status == AlertStatusAcked
}

// IsAcked reports whether the open alert has been acknowledged.
func (a *Alert) IsAcked() bool {
	return a != nil && a.Status == AlertStatusAcked
}
