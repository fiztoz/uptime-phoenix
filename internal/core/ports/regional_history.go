package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// RegionalObservationReader supplies ordered raw regional history for one
// assigned monitor/probe pair. Implementations force from/to to UTC at the
// database boundary and return rows ordered by observation time. It reads
// persisted evidence only; it never opens configuration or credentials.
type RegionalObservationReader interface {
	ListObservations(ctx context.Context, monitorID int64, probeID string, from, to time.Time) ([]domain.RegionalObservation, error)
}

// AssignmentHistoryReader reports which probes were assigned to a monitor over
// a window. It exists so history reads can prove a monitor/probe relationship
// for probes that were unassigned after their observations were recorded.
type AssignmentHistoryReader interface {
	ListHistory(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.AssignmentInterval, error)
}
