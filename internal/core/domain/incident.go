package domain

import (
	"errors"
	"strings"
	"time"
)

// ErrIncidentActive is returned when an active incident is deleted before it
// has been resolved.
var ErrIncidentActive = errors.New("active incident must be resolved before deletion")

// IncidentStatus is the ordered public timeline state for an incident update.
type IncidentStatus string

const (
	// IncidentStatusInvestigating means operators are aware and investigating.
	IncidentStatusInvestigating IncidentStatus = "investigating"
	// IncidentStatusIdentified means the root cause has been identified.
	IncidentStatusIdentified IncidentStatus = "identified"
	// IncidentStatusMonitoring means a mitigation is deployed and being watched.
	IncidentStatusMonitoring IncidentStatus = "monitoring"
	// IncidentStatusResolved means the incident is resolved.
	IncidentStatusResolved IncidentStatus = "resolved"
)

var incidentStatusOrder = map[IncidentStatus]int{
	IncidentStatusInvestigating: 0,
	IncidentStatusIdentified:    1,
	IncidentStatusMonitoring:    2,
	IncidentStatusResolved:      3,
}

// NormalizeIncidentStatus converts user input into a canonical incident status.
func NormalizeIncidentStatus(raw string) IncidentStatus {
	return IncidentStatus(strings.ToLower(strings.TrimSpace(raw)))
}

// ValidIncidentStatus reports whether status is one of the supported timeline states.
func ValidIncidentStatus(status IncidentStatus) bool {
	_, ok := incidentStatusOrder[status]
	return ok
}

// IncidentStatusRank returns the ordering rank for a valid incident status.
func IncidentStatusRank(status IncidentStatus) int {
	return incidentStatusOrder[status]
}

// IncidentStatusProgresses reports whether next is at or after previous in the
// incident timeline order.
func IncidentStatusProgresses(previous, next IncidentStatus) bool {
	return IncidentStatusRank(next) >= IncidentStatusRank(previous)
}

// IncidentUpdate represents one markdown timeline entry on an incident.
type IncidentUpdate struct {
	ID           int64
	IncidentID   int64
	StatusPageID int64
	Status       IncidentStatus
	Content      string
	CreatedAt    time.Time
}

// Incident represents an incident on a status page.
type Incident struct {
	ID           int64
	StatusPageID int64
	Title        string
	Content      string
	Style        string // "warning", "danger", "info", "success"
	Pinned       bool
	Active       bool
	CreatedAt    time.Time
}
