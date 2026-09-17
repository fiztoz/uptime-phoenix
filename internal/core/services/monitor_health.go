package services

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	projections ports.MonitorHealthProjectionRepository
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

// SetProjections attaches materialized current/history storage. Optional: Current
// and History still evaluate from regional evidence when the store is nil.
func (s *MonitorHealthService) SetProjections(store ports.MonitorHealthProjectionRepository) {
	s.projections = store
}

// Current evaluates overall health at now from the authorized assignment set.
// Unauthorized or missing monitors return ErrNotFound so callers cannot confirm existence.
func (s *MonitorHealthService) Current(ctx context.Context, userID, monitorID int64, now time.Time) (*domain.CurrentMonitorHealth, error) {
	if err := s.denyIfHidden(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	return s.evaluate(ctx, monitorID, now)
}

// History reconstructs overall availability intervals for an authorized monitor.
// Regional charts remain per-probe; this path never pools raw samples.
func (s *MonitorHealthService) History(ctx context.Context, userID, monitorID int64, from, to time.Time) (*domain.MonitorHealthHistory, error) {
	if err := s.denyIfHidden(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	from, to = from.UTC(), to.UTC()
	return s.reconstruct(ctx, monitorID, from, to, to)
}

// ProjectCurrent materializes the current overall snapshot. It is an internal
// write path and does not perform access checks.
func (s *MonitorHealthService) ProjectCurrent(ctx context.Context, monitorID int64, now time.Time) error {
	if s.projections == nil {
		return nil
	}
	current, err := s.evaluate(ctx, monitorID, now)
	if err != nil {
		return err
	}
	next := domain.MonitorHealthState{
		MonitorID: current.MonitorID, Policy: current.Policy, Status: current.Health.Status,
		Reason: current.Health.Reason, Counts: current.Health.Counts, LastTransitionAt: current.AsOf,
		AsOf: current.AsOf, ProjectionVersion: 1,
	}
	prev, err := s.projections.GetHealthState(ctx, monitorID)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return fmt.Errorf("load health state: %w", err)
	}
	if prev != nil {
		next.ProjectionVersion = prev.ProjectionVersion
		next.LastTransitionAt = prev.LastTransitionAt
		if sameHealthSnapshot(*prev, next) {
			next.AsOf = current.AsOf
			return s.projections.PutHealthState(ctx, &next)
		}
		if prev.ProjectionVersion == math.MaxInt64 {
			return fmt.Errorf("projection version exhausted: %w", ports.ErrConflict)
		}
		next.ProjectionVersion = prev.ProjectionVersion + 1
		next.LastTransitionAt = current.AsOf
	}
	return s.projections.PutHealthState(ctx, &next)
}

// ProcessDirty recomputes overall history for closed dirty overall minutes.
func (s *MonitorHealthService) ProcessDirty(ctx context.Context, now time.Time, limit int) (int, error) {
	if s.projections == nil {
		return 0, nil
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.projections.ListDirty(ctx, domain.DirtyResolutionOverall, limit*8)
	if err != nil {
		return 0, err
	}
	type window struct {
		monitorID int64
		bucket    time.Time
	}
	groups := make(map[window][]domain.DirtyBucket)
	order := make([]window, 0)
	for _, row := range rows {
		bucket := row.Bucket.UTC().Truncate(time.Minute)
		if !bucket.Add(time.Minute).After(now) {
			key := window{monitorID: row.MonitorID, bucket: bucket}
			if _, ok := groups[key]; !ok {
				if len(order) >= limit {
					continue
				}
				order = append(order, key)
			}
			groups[key] = append(groups[key], domain.DirtyBucket{
				MonitorID: row.MonitorID, ProbeID: row.ProbeID,
				Resolution: row.Resolution, Bucket: bucket,
			})
		}
	}
	processed := 0
	for _, key := range order {
		from, to := key.bucket, key.bucket.Add(time.Minute)
		history, err := s.reconstruct(ctx, key.monitorID, from, to, now)
		if errors.Is(err, ports.ErrNotFound) {
			if err := s.projections.ClearDirty(ctx, groups[key]); err != nil {
				return processed, err
			}
			processed++
			continue
		}
		if err != nil {
			return processed, err
		}
		if err := s.projections.ReplaceHealthHistory(ctx, key.monitorID, from, to, history.Intervals); err != nil {
			return processed, err
		}
		if err := s.projections.ClearDirty(ctx, groups[key]); err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func (s *MonitorHealthService) denyIfHidden(ctx context.Context, userID, monitorID int64) error {
	if s.access == nil {
		return ports.ErrNotFound
	}
	ok, err := s.access.CanViewMonitor(ctx, userID, monitorID)
	if err != nil {
		return err
	}
	if !ok {
		return ports.ErrNotFound
	}
	return nil
}

func (s *MonitorHealthService) evaluate(ctx context.Context, monitorID int64, now time.Time) (*domain.CurrentMonitorHealth, error) {
	monitor, set, freshFor, err := s.loadMonitorEvidence(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	states, err := s.regional.ListStates(ctx, monitorID)
	if err != nil {
		return nil, fmt.Errorf("load regional state: %w", err)
	}
	byProbe := make(map[string]domain.RegionalState, len(states))
	for _, state := range states {
		byProbe[state.ProbeID] = state
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
		MonitorID: monitorID, Policy: set.HealthPolicy, AsOf: now, Health: health, Regions: evidence,
	}, nil
}

func (s *MonitorHealthService) reconstruct(ctx context.Context, monitorID int64, from, to, now time.Time) (*domain.MonitorHealthHistory, error) {
	monitor, set, freshFor, err := s.loadMonitorEvidence(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	from, to, now = from.UTC(), to.UTC(), now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return &domain.MonitorHealthHistory{MonitorID: monitorID, From: from, To: to}, nil
	}
	var assignments []domain.AssignmentInterval
	if set.Revision == 0 {
		// Only a missing legacy set can use the local fallback. Persisted sets
		// must never apply today's membership or policy to an earlier window.
		assignments = assignmentIntervals(set, from)
	} else {
		assignments, err = s.assignments.ListHistory(ctx, monitorID, from, to)
		if err != nil {
			return nil, fmt.Errorf("load assignment history: %w", err)
		}
	}
	lookback := from.Add(-freshFor)
	observations, err := s.regional.ListObservationsInRange(ctx, monitorID, lookback, to)
	if err != nil {
		return nil, fmt.Errorf("load regional history: %w", err)
	}
	history, err := ReconstructOverallHistory(domain.OverallHistoryInput{
		MonitorID: monitorID, From: from, To: to, FreshFor: freshFor, Paused: !monitor.Active,
		Assignments: assignments, Observations: observations,
	})
	if err != nil {
		return nil, err
	}
	return &history, nil
}

func (s *MonitorHealthService) loadMonitorEvidence(ctx context.Context, monitorID int64) (*domain.Monitor, *domain.MonitorProbeAssignments, time.Duration, error) {
	monitor, err := s.monitors.GetByID(ctx, monitorID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return nil, nil, 0, ports.ErrNotFound
		}
		return nil, nil, 0, fmt.Errorf("load monitor: %w", err)
	}
	set, err := s.assignments.GetByMonitorID(ctx, monitorID)
	if errors.Is(err, ports.ErrNotFound) {
		set = &domain.MonitorProbeAssignments{
			MonitorID:    monitorID,
			HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments:  []domain.ProbeAssignment{{MonitorID: monitorID, ProbeID: domain.LocalProbeID, Generation: 1}},
		}
	} else if err != nil {
		return nil, nil, 0, fmt.Errorf("load assignments: %w", err)
	}
	freshFor, err := RegionalFreshnessWindow(monitor.Interval, monitor.RetryInterval, monitor.Timeout)
	if err != nil {
		return nil, nil, 0, err
	}
	return monitor, set, freshFor, nil
}

func assignmentIntervals(set *domain.MonitorProbeAssignments, from time.Time) []domain.AssignmentInterval {
	out := make([]domain.AssignmentInterval, 0, len(set.Assignments))
	for _, assignment := range set.Assignments {
		start := assignment.CreatedAt.UTC()
		if start.IsZero() {
			start = from
		}
		out = append(out, domain.AssignmentInterval{
			ProbeID: assignment.ProbeID, Generation: assignment.Generation,
			Policy: set.HealthPolicy, Revision: set.Revision, From: start,
		})
	}
	return out
}

func sameHealthSnapshot(prev, next domain.MonitorHealthState) bool {
	return prev.Status == next.Status && prev.Reason == next.Reason && prev.Policy == next.Policy &&
		prev.Counts == next.Counts
}
