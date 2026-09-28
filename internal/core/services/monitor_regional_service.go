package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// MonitorRegionalService supplies authorized, nonsecret inputs to the M5 HTTP
// views. It never reads fleet credentials, configuration payloads or endpoints.
// The optional diagnostics port is the only source allowed to populate the
// staged connection/config diagnostic fields; without it they stay unreported.
type MonitorRegionalService struct {
	health      *MonitorHealthService
	registry    ports.ProbeRegistryRepository
	diagnostics ports.ProbeDiagnosticsRepository
	obs         ports.RegionalObservationReader
	assignHist  ports.AssignmentHistoryReader
}

// MonitorRegionalAssignments contains desired membership, not proof of execution.
type MonitorRegionalAssignments struct {
	Set    domain.MonitorProbeAssignments
	Probes map[string]domain.Probe
	Diag   map[string]ProbeDiagnosticSummary
}

// MonitorRegionalHealth combines live policy health with bounded historical coverage.
// ProjectionVersion identifies persisted projection work, not a cache validator:
// freshness can change with wall time before a worker increments this version.
type MonitorRegionalHealth struct {
	Current           domain.CurrentMonitorHealth
	Probes            map[string]domain.Probe
	Diag              map[string]ProbeDiagnosticSummary
	ProjectionVersion int64
	Coverage          domain.HealthCoverage
}

// NewMonitorRegionalService binds the existing health and registration readers.
func NewMonitorRegionalService(health *MonitorHealthService, registry ports.ProbeRegistryRepository) *MonitorRegionalService {
	return &MonitorRegionalService{health: health, registry: registry}
}

// SetDiagnostics binds the dedicated safe runtime read port. Diagnostic fields
// remain null until it is wired and only carry values it can prove.
func (s *MonitorRegionalService) SetDiagnostics(diagnostics ports.ProbeDiagnosticsRepository) {
	if s != nil {
		s.diagnostics = diagnostics
	}
}

// SetHistory binds the raw regional evidence and assignment-history readers
// used by the relationship-checked history routes. Without them the routes
// report regional_unavailable rather than serving partial evidence.
func (s *MonitorRegionalService) SetHistory(obs ports.RegionalObservationReader, assignHist ports.AssignmentHistoryReader) {
	if s != nil {
		s.obs = obs
		s.assignHist = assignHist
	}
}

// ErrProbeNotRelated reports a monitor/probe pair with no current or recorded
// assignment relationship. It reads exactly like a missing probe.
var ErrProbeNotRelated = errors.New("probe not related to monitor")

// RegionalHistory returns a visible monitor's ordered regional history for one
// probe over [from, to). The monitor/probe relationship is validated first:
// the probe must be assigned now or have been assigned during the window, and
// a probe that never belonged to this monitor reads exactly like a missing one.
// Bounds are normalized to UTC before any storage read.
func (s *MonitorRegionalService) RegionalHistory(ctx context.Context, userID, monitorID int64, probeID string, from, to time.Time) ([]domain.RegionalObservation, error) {
	if s == nil || s.health == nil || s.obs == nil || s.assignHist == nil {
		return nil, domain.ErrInternal
	}
	if probeID == "" || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, domain.ErrValidation
	}
	if err := s.health.denyIfHidden(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	from, to = from.UTC(), to.UTC()
	related, err := s.probeRelated(ctx, monitorID, probeID, from, to)
	if err != nil {
		return nil, err
	}
	if !related {
		// Same answer as a hidden monitor: never confirm that another monitor's
		// probe exists or has evidence here.
		return nil, ErrProbeNotRelated
	}
	rows, err := s.obs.ListObservations(ctx, monitorID, probeID, from, to)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// OverallHistoryRow is one synthesized overall availability segment for the
// unqualified heartbeat endpoint on a multi-probe monitor. Rows are derived
// from the policy-evaluated intervals, never from pooled regional samples, and
// carry no measured latency.
type OverallHistoryRow struct {
	From      time.Time
	Status    domain.Status
	Reason    string
	Important bool
}

// OverallHistoryResult tells the unqualified endpoint which stream it serves:
// the local recorder's measured rows (LocalOnly) or the synthesized overall
// timeline with its downtime/unknown intervals.
type OverallHistoryResult struct {
	LocalOnly bool
	Rows      []OverallHistoryRow
	Intervals []domain.MonitorHealthInterval
}

// OverallHistory answers the section-7.2 compatibility question for the
// unqualified heartbeat endpoints: a monitor whose only member is the hub's
// local prober keeps today's measured stream, and any monitor with a remote
// member serves the policy-evaluated overall timeline instead. Access is
// checked before any evidence read and bounds are normalized to UTC.
func (s *MonitorRegionalService) OverallHistory(ctx context.Context, userID, monitorID int64, from, to time.Time) (*OverallHistoryResult, error) {
	if s == nil || s.health == nil {
		return nil, domain.ErrInternal
	}
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, domain.ErrValidation
	}
	from, to = from.UTC(), to.UTC()
	history, err := s.health.History(ctx, userID, monitorID, from, to)
	if err != nil {
		return nil, err
	}
	_, set, _, err := s.health.loadMonitorEvidence(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	localOnly := true
	for _, assignment := range set.Assignments {
		if assignment.ProbeID != domain.LocalProbeID {
			localOnly = false
			break
		}
	}
	if localOnly {
		return &OverallHistoryResult{LocalOnly: true}, nil
	}
	rows := make([]OverallHistoryRow, 0, len(history.Intervals))
	for i, interval := range history.Intervals {
		rows = append(rows, OverallHistoryRow{
			From:      interval.From,
			Status:    interval.Status,
			Reason:    interval.Reason,
			Important: i == 0 || interval.Status != history.Intervals[i-1].Status,
		})
	}
	return &OverallHistoryResult{Rows: rows, Intervals: history.Intervals}, nil
}

// probeRelated proves the monitor/probe relationship from the current desired
// set or the recorded assignment intervals overlapping the window.
func (s *MonitorRegionalService) probeRelated(ctx context.Context, monitorID int64, probeID string, from, to time.Time) (bool, error) {
	_, set, _, err := s.health.loadMonitorEvidence(ctx, monitorID)
	if err != nil {
		return false, err
	}
	for _, assignment := range set.Assignments {
		if assignment.ProbeID == probeID {
			return true, nil
		}
	}
	intervals, err := s.assignHist.ListHistory(ctx, monitorID, from, to)
	if err != nil {
		return false, err
	}
	for _, interval := range intervals {
		if interval.ProbeID != probeID {
			continue
		}
		// [From, To) overlap with [from, to); a zero To means open-ended.
		if interval.From.Before(to) && (interval.To.IsZero() || from.Before(interval.To)) {
			return true, nil
		}
	}
	return false, nil
}

// Assignments reads only a visible monitor's complete desired set. Revision zero
// denotes legacy local execution and must never be submitted to a write endpoint.
func (s *MonitorRegionalService) Assignments(ctx context.Context, userID, monitorID int64, now time.Time) (*MonitorRegionalAssignments, error) {
	if s == nil || s.health == nil || s.registry == nil {
		return nil, domain.ErrInternal
	}
	if now.IsZero() {
		return nil, domain.ErrValidation
	}
	if err := s.health.denyIfHidden(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	_, set, _, err := s.health.loadMonitorEvidence(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	probes, diagnostics, err := s.labels(ctx, set.Assignments, now)
	if err != nil {
		return nil, err
	}
	// Do not sort repository-owned slices (including in-memory implementations).
	copySet := *set
	copySet.Assignments = append([]domain.ProbeAssignment{}, set.Assignments...)
	sort.Slice(copySet.Assignments, func(i, j int) bool { return copySet.Assignments[i].ProbeID < copySet.Assignments[j].ProbeID })
	return &MonitorRegionalAssignments{Set: copySet, Probes: probes, Diag: diagnostics}, nil
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
	if err := s.health.denyIfHidden(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	return s.HealthForPublication(ctx, monitorID, hours, now)
}

// HealthForPublication builds a system event after persistence commits. It grants
// no browser access: the event adapter must authorize every recipient at fan-out.
func (s *MonitorRegionalService) HealthForPublication(ctx context.Context, monitorID int64, hours int, now time.Time) (*MonitorRegionalHealth, error) {
	if s == nil || s.health == nil || s.registry == nil || hours < 1 || hours > 720 || now.IsZero() {
		return nil, domain.ErrValidation
	}
	now = now.UTC()
	current, err := s.health.evaluate(ctx, monitorID, now)
	if err != nil {
		return nil, err
	}
	probes, diagnostics, err := s.labels(ctx, current.Assignments.Assignments, now)
	if err != nil {
		return nil, err
	}
	history, err := s.health.reconstruct(ctx, monitorID, now.Add(-time.Duration(hours)*time.Hour), now, now)
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
	return &MonitorRegionalHealth{Current: *current, Probes: probes, Diag: diagnostics, ProjectionVersion: version, Coverage: coverage}, nil
}

func (s *MonitorRegionalService) labels(ctx context.Context, assignments []domain.ProbeAssignment, now time.Time) (map[string]domain.Probe, map[string]ProbeDiagnosticSummary, error) {
	out := make(map[string]domain.Probe, len(assignments))
	diagnostics := make(map[string]ProbeDiagnosticSummary, len(assignments))
	for _, a := range assignments {
		p, err := s.registry.GetByID(ctx, a.ProbeID)
		if err != nil {
			return nil, nil, fmt.Errorf("read assigned probe metadata: %w", err)
		}
		out[a.ProbeID] = *p
		if s.diagnostics == nil {
			continue
		}
		row, err := s.diagnostics.GetProbeDiagnostics(ctx, a.ProbeID)
		if err != nil {
			return nil, nil, fmt.Errorf("read assigned probe diagnostics: %w", err)
		}
		diagnostics[a.ProbeID] = ProbeDiagnosticsSummary(now, *row)
	}
	return out, diagnostics, nil
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
