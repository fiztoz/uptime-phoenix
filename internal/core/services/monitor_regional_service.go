package services

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// MonitorRegionalService supplies authorized, nonsecret inputs to the M5 HTTP
// views. It never reads fleet credentials, configuration payloads or endpoints.
type MonitorRegionalService struct {
	health   *MonitorHealthService
	registry ports.ProbeRegistryRepository
}

// MonitorRegionalAssignments contains desired membership, not proof of execution.
type MonitorRegionalAssignments struct {
	Set    domain.MonitorProbeAssignments
	Probes map[string]domain.Probe
}

// MonitorRegionalHealth combines live policy health with bounded historical coverage.
// ProjectionVersion identifies persisted projection work, not a cache validator:
// freshness can change with wall time before a worker increments this version.
type MonitorRegionalHealth struct {
	Current           domain.CurrentMonitorHealth
	Probes            map[string]domain.Probe
	ProjectionVersion int64
	Coverage          domain.HealthCoverage
}

// NewMonitorRegionalService binds the existing health and registration readers.
func NewMonitorRegionalService(health *MonitorHealthService, registry ports.ProbeRegistryRepository) *MonitorRegionalService {
	return &MonitorRegionalService{health: health, registry: registry}
}

// Assignments reads only a visible monitor's complete desired set. Revision zero
// denotes legacy local execution and must never be submitted to a write endpoint.
func (s *MonitorRegionalService) Assignments(ctx context.Context, userID, monitorID int64) (*MonitorRegionalAssignments, error) {
	if s == nil || s.health == nil || s.registry == nil {
		return nil, domain.ErrInternal
	}
	if err := s.health.denyIfHidden(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	_, set, _, err := s.health.loadMonitorEvidence(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	probes, err := s.labels(ctx, set.Assignments)
	if err != nil {
		return nil, err
	}
	// Do not sort repository-owned slices (including in-memory implementations).
	copySet := *set
	copySet.Assignments = append([]domain.ProbeAssignment{}, set.Assignments...)
	sort.Slice(copySet.Assignments, func(i, j int) bool { return copySet.Assignments[i].ProbeID < copySet.Assignments[j].ProbeID })
	return &MonitorRegionalAssignments{Set: copySet, Probes: probes}, nil
}

// Health reads current policy evidence and coverage over [now-hours, now).
// Access is checked before any regional/registration reads, and all bounds are UTC.
func (s *MonitorRegionalService) Health(ctx context.Context, userID, monitorID int64, hours int, now time.Time) (*MonitorRegionalHealth, error) {
	if s == nil || s.health == nil || s.registry == nil {
		return nil, domain.ErrInternal
	}
	if hours < 1 || hours > 720 || now.IsZero() {
		return nil, domain.ErrValidation
	}
	now = now.UTC()
	current, err := s.health.Current(ctx, userID, monitorID, now)
	if err != nil {
		return nil, err
	}
	probes, err := s.labels(ctx, current.Assignments.Assignments)
	if err != nil {
		return nil, err
	}
	history, err := s.health.History(ctx, userID, monitorID, now.Add(-time.Duration(hours)*time.Hour), now)
	if err != nil {
		return nil, err
	}
	coverage, err := CalculateHealthCoverage(history.Durations)
	if err != nil {
		return nil, err
	}
	version, err := s.health.StoredVersion(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	sort.Slice(current.Regions, func(i, j int) bool { return current.Regions[i].ProbeID < current.Regions[j].ProbeID })
	return &MonitorRegionalHealth{Current: *current, Probes: probes, ProjectionVersion: version, Coverage: coverage}, nil
}

func (s *MonitorRegionalService) labels(ctx context.Context, assignments []domain.ProbeAssignment) (map[string]domain.Probe, error) {
	out := make(map[string]domain.Probe, len(assignments))
	for _, a := range assignments {
		p, err := s.registry.GetByID(ctx, a.ProbeID)
		if err != nil {
			return nil, fmt.Errorf("read assigned probe metadata: %w", err)
		}
		out[a.ProbeID] = *p
	}
	return out, nil
}

// RegionalDisplayHealth uses the same evaluator as overall health, so expired,
// future, missing and invalidated evidence cannot appear UP in a regional view.
func RegionalDisplayHealth(at time.Time, e domain.RegionalHealthEvidence) (domain.Status, string, error) {
	h, err := EvaluateMonitorHealth(at, domain.HealthPolicyAnyDown, []domain.RegionalHealthEvidence{e})
	if err != nil {
		return domain.StatusUnknown, "", err
	}
	if e.Paused {
		return h.Status, "paused", nil
	}
	if h.Status != domain.StatusUnknown {
		return h.Status, "", nil
	}
	// Expose diagnostic codes only, never a source-provided free-form message.
	switch e.UnknownReason {
	case "missing_evidence", "stale_generation", "stream_reset", "missing_snapshot_state", "history_gap":
		return h.Status, e.UnknownReason, nil
	}
	if e.UnknownReason != "" {
		return h.Status, "incomplete_evidence", nil
	}
	if e.ObservedAt.IsZero() {
		return h.Status, "missing_evidence", nil
	}
	if e.ObservedAt.After(at) {
		return h.Status, "clock_skew", nil
	}
	if !at.Before(e.ObservedAt.Add(e.FreshFor)) {
		return h.Status, "stale_evidence", nil
	}
	return h.Status, "incomplete_evidence", nil
}
