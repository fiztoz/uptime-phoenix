package services

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestPublicRegionalCoverageUsesOverallTimeline(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	start := now.Add(-2 * time.Minute)
	monitor := &domain.Monitor{ID: 7, Active: true, Interval: 60, Timeout: 5}
	assignments := healthAssignmentRepo{
		sets:    map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Revision: 1, HealthPolicy: domain.HealthPolicyAnyDown, Assignments: []domain.ProbeAssignment{{ProbeID: "local", Generation: 1}, {ProbeID: "private-region", Generation: 1}}}},
		history: map[int64][]domain.AssignmentInterval{7: {{ProbeID: "local", Generation: 1, Revision: 1, Policy: domain.HealthPolicyAnyDown, From: start}, {ProbeID: "private-region", Generation: 1, Revision: 1, Policy: domain.HealthPolicyAnyDown, From: start}}},
	}
	regional := &healthRegionalRepo{observations: map[int64][]domain.RegionalObservation{7: {{ID: 1, MonitorID: 7, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: start}, {ID: 2, MonitorID: 7, ProbeID: "private-region", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: start}}}}
	health := NewMonitorHealthService(healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: monitor}}, assignments, regional, nil)
	svc := &StatusPageService{regionalHealth: health}
	view := &PublicMonitorStatus{ID: 7, Status: "down"}
	if !svc.populatePublicRegionalHealth(t.Context(), 7, view, now) {
		t.Fatal("remote monitor used local stream")
	}
	if view.UptimePercent == nil || *view.UptimePercent != 0 {
		t.Fatalf("local UP concealed remote DOWN: %+v", view.UptimePercent)
	}
	if view.CoveragePercent == nil || math.Abs(*view.CoveragePercent-100.0*120/86400) > 0.00001 {
		t.Fatalf("unknown interval became coverage: %+v", view.CoveragePercent)
	}
	if len(view.UptimeData) != 90 || view.UptimeData[89].Status != "down" || view.Chart != nil {
		t.Fatalf("public timeline or latency fabricated: %+v", view)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-region", "probe_id", "endpoint", "regions", "fingerprint"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("public view disclosed %s", private)
		}
	}
	regional.observations = nil
	unknown := &PublicMonitorStatus{ID: 7, Status: "unknown"}
	svc.populatePublicRegionalHealth(t.Context(), 7, unknown, now)
	if unknown.UptimePercent != nil || unknown.CoveragePercent == nil || *unknown.CoveragePercent != 0 {
		t.Fatalf("missing evidence invented availability: %+v", unknown)
	}
}
