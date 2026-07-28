// Package domain contains the pure domain types for Phoenix.
// These types have no framework imports, no DB driver imports, and no HTTP imports.
// They are data only — no I/O methods.
package domain

// Status represents the health status of a monitored target.
type Status int

const (
	StatusDown        Status = 0
	StatusUp          Status = 1
	StatusPending     Status = 2
	StatusMaintenance Status = 3
)

// String returns the human-readable representation of the status.
func (s Status) String() string {
	switch s {
	case StatusUp:
		return "UP"
	case StatusDown:
		return "DOWN"
	case StatusPending:
		return "PENDING"
	case StatusMaintenance:
		return "MAINTENANCE"
	default:
		return "UNKNOWN"
	}
}
