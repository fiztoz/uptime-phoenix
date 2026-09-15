package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// monitorViewer is the AccessService view check used by overall health reads.
type monitorViewer interface {
	CanViewMonitor(ctx context.Context, userID, monitorID int64) (bool, error)
}

// MonitorHealthService projects overall health from complete assignment evidence.
// It does not emit UNKNOWN on existing HTTP or browser heartbeat routes.
type MonitorHealthService struct {
	monitors    ports.MonitorRepository
	assignments ports.MonitorProbeAssignmentRepository
	regional    ports.RegionalCommitRepository
	access      monitorViewer
}

// NewMonitorHealthService creates an overall health reader.
func NewMonitorHealthService(
	monitors ports.MonitorRepository,
	assignments ports.MonitorProbeAssignmentRepository,
	regional ports.RegionalCommitRepository,
	access monitorViewer,
) *MonitorHealthService {
	return &MonitorHealthService{monitors: monitors, assignments: assignments, regional: regional, access: access}
}

// Current evaluates overall health at now from the authorized assignment set.
// Unauthorized or missing monitors return ErrNotFound so callers cannot confirm existence.
func (s *MonitorHealthService) Current(ctx context.Context, userID, monitorID int64, now time.Time) (*domain.CurrentMonitorHealth, error) {
	if s.access == nil {
		return nil, ports.ErrNotFound
	}
	ok, err := s.access.CanViewMonitor(ctx, userID, monitorID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ports.ErrNotFound
	}
	monitor, err := s.monitors.GetByID(ctx, monitorID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return nil, ports.ErrNotFound
		}
		return nil, fmt.Errorf("load monitor: %w", err)
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	set, err := s.assignments.GetByMonitorID(ctx, monitorID)
	if errors.Is(err, ports.ErrNotFound) {
		set = &domain.MonitorProbeAssignments{
			MonitorID:    monitorID,
			HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments:  []domain.ProbeAssignment{{MonitorID: monitorID, ProbeID: domain.LocalProbeID, Generation: 1}},
		}
	} else if err != nil {
		return nil, fmt.Errorf("load assignments: %w", err)
	}
	states, err := s.regional.ListStates(ctx, monitorID)
	if err != nil {
		return nil, fmt.Errorf("load regional state: %w", err)
	}
	byProbe := make(map[string]domain.RegionalState, len(states))
	for _, state := range states {
		byProbe[state.ProbeID] = state
	}
	freshFor, err := RegionalFreshnessWindow(monitor.Interval, monitor.RetryInterval, monitor.Timeout)
	if err != nil {
		return nil, err
	}
	evidence := make([]domain.RegionalHealthEvidence, 0, len(set.Assignments))
	for _, assignment := range set.Assignments {
		region := domain.RegionalHealthEvidence{ProbeID: assignment.ProbeID, FreshFor: freshFor, Paused: !monitor.Active}
		state, ok := byProbe[assignment.ProbeID]
		switch {
		case !ok:
			region.UnknownReason = "missing_evidence"
		case state.AssignmentGeneration != assignment.Generation:
			region.UnknownReason = "stale_generation"
		default:
			region.Status = state.Status
			region.ObservedAt = state.ObservedAt.UTC()
		}
		evidence = append(evidence, region)
	}
	health, err := EvaluateMonitorHealth(now, set.HealthPolicy, evidence)
	if err != nil {
		return nil, err
	}
	return &domain.CurrentMonitorHealth{
		MonitorID: monitorID,
		Policy:    set.HealthPolicy,
		AsOf:      now,
		Health:    health,
		Regions:   evidence,
	}, nil
}
