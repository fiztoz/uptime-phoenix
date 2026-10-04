package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type regionalLabelRepo struct {
	ports.ProbeRegistryRepository
	calls []string
	err   error
}

func (r *regionalLabelRepo) GetByID(_ context.Context, id string) (*domain.Probe, error) {
	r.calls = append(r.calls, id)
	if r.err != nil {
		return nil, r.err
	}
	return &domain.Probe{ID: id, Name: "safe region", Location: "Earth"}, nil
}

func TestMonitorRegionalServiceScopeAndFreshness(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	set := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown, Assignments: []domain.ProbeAssignment{{MonitorID: 7, ProbeID: "remote", Generation: 2}, {MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1}}}
	region := &healthRegionalRepo{states: map[int64][]domain.RegionalState{7: {
		{ProbeID: "remote", AssignmentGeneration: 2, Status: domain.StatusUp, ObservedAt: now.Add(-3 * time.Minute), ReceivedAt: now},
		{ProbeID: domain.LocalProbeID, AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: now, ReceivedAt: now},
		{ProbeID: "unassigned", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: now},
	}}}
	access := &healthAccess{allow: map[int64]bool{7: true}}
	health := NewMonitorHealthService(healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: {ID: 7, Active: true, Interval: 60, Timeout: 5}}}, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: set}}, region, access)
	labels := &regionalLabelRepo{}
	svc := NewMonitorRegionalService(health, labels)
	got, err := svc.Health(t.Context(), 1, 7, 1, now.In(time.FixedZone("UTC+7", 7*3600)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Current.Health.Status != domain.StatusUnknown || got.Current.Health.Counts.Unknown != 1 || got.Current.AsOf.Location() != time.UTC {
		t.Fatalf("bad current projection: %+v", got.Current)
	}
	if len(labels.calls) != 2 || labels.calls[0] != "remote" || labels.calls[1] != domain.LocalProbeID {
		t.Fatalf("read unrelated registration: %v", labels.calls)
	}
	if got.Current.Regions[0].ProbeID != domain.LocalProbeID || got.Current.Regions[1].ReceivedAt != now {
		t.Fatalf("unstable region ordering/receipt: %+v", got.Current.Regions)
	}
	status, reason, err := RegionalDisplayHealth(now, got.Current.Regions[1])
	if err != nil || status != domain.StatusUnknown || reason != "stale_evidence" {
		t.Fatalf("stale UP escaped: %v %s %v", status, reason, err)
	}
	assignments, err := svc.Assignments(t.Context(), 1, 7, now)
	if err != nil || assignments.Set.Assignments[0].ProbeID != domain.LocalProbeID || set.Assignments[0].ProbeID != "remote" {
		t.Fatalf("sorting mutated storage: %+v %v", assignments, err)
	}
	// A revoked grant takes effect on the next call, before reading labels/state.
	access.allow[7] = false
	beforeLabels, beforeState := len(labels.calls), region.listCalls
	if _, err := svc.Assignments(t.Context(), 1, 7, now); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("hidden assignments: %v", err)
	}
	if _, err := svc.Health(t.Context(), 1, 7, 1, now); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("hidden health: %v", err)
	}
	if len(labels.calls) != beforeLabels || region.listCalls != beforeState {
		t.Fatal("denial performed scoped reads")
	}
}

func TestMonitorRegionalServiceLegacyAndFailures(t *testing.T) {
	now := time.Now().UTC()
	labels := &regionalLabelRepo{}
	health := NewMonitorHealthService(healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: {ID: 7, Active: true, Interval: 60, Timeout: 5}}}, healthAssignmentRepo{}, &healthRegionalRepo{}, healthAccess{allow: map[int64]bool{7: true, 99: true}})
	svc := NewMonitorRegionalService(health, labels)
	got, err := svc.Assignments(t.Context(), 1, 7, now)
	if err != nil || got.Set.Revision != 0 || len(got.Set.Assignments) != 1 || got.Set.Assignments[0].ProbeID != domain.LocalProbeID {
		t.Fatalf("legacy local: %+v %v", got, err)
	}
	if _, err := svc.Assignments(t.Context(), 1, 99, now); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("missing monitor: %v", err)
	}
	labels.err = errors.New("storage failed")
	if _, err := svc.Health(t.Context(), 1, 7, 1, now); !errors.Is(err, labels.err) {
		t.Fatalf("hidden storage failure: %v", err)
	}
	for _, hours := range []int{0, -1, 721} {
		if _, err := svc.Health(t.Context(), 1, 7, hours, now); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("hours %d: %v", hours, err)
		}
	}
}

func TestRegionalDisplayHealthBoundaries(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		e      domain.RegionalHealthEvidence
		want   domain.Status
		reason string
	}{
		{"fresh", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now, FreshFor: time.Minute}, domain.StatusUp, ""},
		{"expiry boundary", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now.Add(-time.Minute), FreshFor: time.Minute}, domain.StatusUnknown, "stale_evidence"},
		{"future", domain.RegionalHealthEvidence{Status: domain.StatusUp, ObservedAt: now.Add(time.Second), FreshFor: time.Minute}, domain.StatusUnknown, "clock_skew"},
		{"missing", domain.RegionalHealthEvidence{UnknownReason: "missing_evidence"}, domain.StatusUnknown, "missing_evidence"},
		{"reassigned", domain.RegionalHealthEvidence{UnknownReason: "stale_generation"}, domain.StatusUnknown, "stale_generation"},
		{"paused", domain.RegionalHealthEvidence{Paused: true}, domain.StatusUnknown, "paused"},
		{"maintenance", domain.RegionalHealthEvidence{Status: domain.StatusMaintenance, ObservedAt: now, FreshFor: time.Minute}, domain.StatusMaintenance, ""},
		{"redacted reason", domain.RegionalHealthEvidence{UnknownReason: "secret endpoint credential"}, domain.StatusUnknown, "incomplete_evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.e.ProbeID = "region"
			status, reason, err := RegionalDisplayHealth(now, tc.e)
			if err != nil || status != tc.want || reason != tc.reason {
				t.Fatalf("got %v %s %v", status, reason, err)
			}
		})
	}
}
