package services

import (
	"context"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AggregateStatusReader returns overall policy status for monitors whose
// assignment set includes a remote probe. Monitors absent from the map keep
// their local heartbeat: a local-only install does not change readers.
type AggregateStatusReader interface {
	StatusForMonitors(ctx context.Context, monitorIDs []int64, now time.Time) (map[int64]domain.Status, error)
}

// StatusForMonitors evaluates overall health for remotely assigned monitors.
// A monitor with no stored assignment set, or only the local probe, is omitted.
func (s *MonitorHealthService) StatusForMonitors(ctx context.Context, monitorIDs []int64, now time.Time) (map[int64]domain.Status, error) {
	if s == nil {
		return map[int64]domain.Status{}, nil
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out := make(map[int64]domain.Status)
	seen := make(map[int64]struct{}, len(monitorIDs))
	for _, id := range monitorIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		set, err := s.assignments.GetByMonitorID(ctx, id)
		if errors.Is(err, ports.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !assignmentUsesOverall(set) {
			continue
		}
		current, err := s.evaluate(ctx, id, now)
		if err != nil {
			return nil, err
		}
		out[id] = current.Health.Status
	}
	return out, nil
}

// assignmentUsesOverall reports whether group, status-page, and badge readers
// must use policy evidence instead of the latest local heartbeat.
func assignmentUsesOverall(set *domain.MonitorProbeAssignments) bool {
	if set == nil {
		return false
	}
	for _, assignment := range set.Assignments {
		if domain.NormalizeProbeID(assignment.ProbeID) != domain.LocalProbeID {
			return true
		}
	}
	return false
}
