package services

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestReconstructOverallHistoryPolicyAndFreshness(t *testing.T) {
	start := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	fresh := 90 * time.Second
	localZone := start.In(time.FixedZone("UTC+7", 7*3600))
	assignments := []domain.AssignmentInterval{
		{ProbeID: "local", Generation: 1, Policy: domain.HealthPolicyAnyDown, Revision: 1, From: start},
		{ProbeID: "asia", Generation: 1, Policy: domain.HealthPolicyAnyDown, Revision: 1, From: start},
	}
	observations := []domain.RegionalObservation{
		{ID: 1, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: localZone},
		{ID: 2, ProbeID: "asia", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: start},
	}
	got, err := ReconstructOverallHistory(domain.OverallHistoryInput{
		MonitorID: 7, From: start, To: start.Add(2 * time.Minute), FreshFor: fresh,
		Assignments: assignments, Observations: observations,
	})
	if err != nil || len(got.Intervals) != 2 {
		t.Fatalf("any_down: %+v %v", got, err)
	}
	if got.Intervals[0].Status != domain.StatusDown || got.Intervals[0].Cause != domain.HealthHistoryCauseRegional {
		t.Fatalf("confirmed disagreement: %+v", got.Intervals[0])
	}
	if got.Intervals[1].Status != domain.StatusUnknown || got.Intervals[1].Cause != domain.HealthHistoryCauseFreshness {
		t.Fatalf("freshness expiry: %+v", got.Intervals[1])
	}
	if got.Durations.Down != fresh || got.Durations.Unknown != 2*time.Minute-fresh {
		t.Fatalf("durations: %+v", got.Durations)
	}

	for i := range assignments {
		assignments[i].Policy = domain.HealthPolicyAllDown
	}
	got, err = ReconstructOverallHistory(domain.OverallHistoryInput{
		MonitorID: 7, From: start, To: start.Add(fresh), FreshFor: fresh,
		Assignments: assignments, Observations: observations,
	})
	if err != nil || len(got.Intervals) != 1 || got.Intervals[0].Status != domain.StatusUp {
		t.Fatalf("all_down: %+v %v", got, err)
	}
}

func TestReconstructOverallHistoryAssignmentAndLateData(t *testing.T) {
	start := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	fresh := 2 * time.Minute
	local := domain.AssignmentInterval{ProbeID: "local", Generation: 1, Policy: domain.HealthPolicyAnyDown, Revision: 1, From: start}
	remote := domain.AssignmentInterval{ProbeID: "asia", Generation: 1, Policy: domain.HealthPolicyAnyDown, Revision: 1, From: start.Add(time.Minute)}
	localObs := domain.RegionalObservation{ID: 1, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: start}
	earlyRemote := domain.RegionalObservation{ID: 2, ProbeID: "asia", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: start}
	got, err := ReconstructOverallHistory(domain.OverallHistoryInput{
		MonitorID: 3, From: start, To: start.Add(2 * time.Minute), FreshFor: fresh,
		Assignments:  []domain.AssignmentInterval{local, remote},
		Observations: []domain.RegionalObservation{localObs, earlyRemote},
	})
	if err != nil || len(got.Intervals) != 2 {
		t.Fatalf("assignment gate: %+v %v", got, err)
	}
	if got.Intervals[0].Status != domain.StatusUp || got.Intervals[0].Counts.Assigned != 1 {
		t.Fatalf("remote must not participate before assignment: %+v", got.Intervals[0])
	}
	if got.Intervals[1].Status != domain.StatusDown || got.Intervals[1].Cause != domain.HealthHistoryCauseAssignment {
		t.Fatalf("remote joins at assignment: %+v", got.Intervals[1])
	}

	late := domain.RegionalObservation{ID: 3, ProbeID: "asia", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: start.Add(time.Minute), ReceivedAt: start.Add(10 * time.Minute)}
	got, err = ReconstructOverallHistory(domain.OverallHistoryInput{
		MonitorID: 3, From: start, To: start.Add(2 * time.Minute), FreshFor: fresh,
		Assignments:  []domain.AssignmentInterval{local, remote},
		Observations: []domain.RegionalObservation{localObs, late},
	})
	if err != nil || got.Intervals[1].Status != domain.StatusDown {
		t.Fatalf("late observed_at still reconstructs: %+v %v", got, err)
	}

	sameSecond := []domain.RegionalObservation{
		{ID: 4, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusPending, ObservedAt: start},
		{ID: 5, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: start},
	}
	got, err = ReconstructOverallHistory(domain.OverallHistoryInput{
		MonitorID: 3, From: start, To: start.Add(time.Second), FreshFor: fresh,
		Assignments: []domain.AssignmentInterval{local}, Observations: sameSecond,
	})
	if err != nil || len(got.Intervals) != 1 || got.Intervals[0].Status != domain.StatusUp || got.Durations.Up != time.Second {
		t.Fatalf("same-second id tie-break: %+v %v", got, err)
	}
}

func TestReconstructOverallHistoryRejectsInvalidInput(t *testing.T) {
	start := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	valid := domain.AssignmentInterval{ProbeID: "local", Generation: 1, Policy: domain.HealthPolicyAnyDown, Revision: 1, From: start}
	for _, in := range []domain.OverallHistoryInput{
		{From: time.Time{}, To: start.Add(time.Minute), FreshFor: time.Minute, Assignments: []domain.AssignmentInterval{valid}},
		{From: start, To: start, FreshFor: time.Minute, Assignments: []domain.AssignmentInterval{valid}},
		{From: start, To: start.Add(time.Minute), FreshFor: -1, Assignments: []domain.AssignmentInterval{valid}},
		{From: start, To: start.Add(time.Minute), FreshFor: time.Minute, Assignments: []domain.AssignmentInterval{{ProbeID: "local", From: start, To: start, Policy: domain.HealthPolicyAnyDown}}},
		{From: start, To: start.Add(time.Minute), FreshFor: time.Minute, Assignments: []domain.AssignmentInterval{valid, valid}},
		{From: start, To: start.Add(time.Minute), FreshFor: time.Minute, Observations: []domain.RegionalObservation{{Status: domain.StatusUp, ObservedAt: start}}},
	} {
		if _, err := ReconstructOverallHistory(in); !errors.Is(err, ErrInvalidHealthEvidence) {
			t.Fatalf("got error %v for %+v", err, in)
		}
	}
	empty, err := ReconstructOverallHistory(domain.OverallHistoryInput{From: start, To: start.Add(time.Minute), FreshFor: time.Minute})
	if err != nil || len(empty.Intervals) != 0 || empty.Durations != (domain.HealthDurations{}) {
		t.Fatalf("no assignments: %+v %v", empty, err)
	}
}

func TestDirtyBucketsForObservation(t *testing.T) {
	at := time.Date(2026, 9, 16, 10, 7, 30, 0, time.UTC)
	got := domain.DirtyBucketsForObservation(domain.RegionalObservation{
		MonitorID: 4, ProbeID: "asia", ObservedAt: at.In(time.FixedZone("UTC+7", 7*3600)),
	})
	want := []domain.DirtyBucket{
		{MonitorID: 4, ProbeID: "asia", Resolution: domain.DirtyResolution1m, Bucket: at.Truncate(time.Minute)},
		{MonitorID: 4, ProbeID: "asia", Resolution: domain.DirtyResolution1h, Bucket: at.Truncate(time.Hour)},
		{MonitorID: 4, ProbeID: "asia", Resolution: domain.DirtyResolution1d, Bucket: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)},
		{MonitorID: 4, ProbeID: "asia", Resolution: domain.DirtyResolutionOverall, Bucket: at.Truncate(time.Minute)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bucket %d: %+v want %+v", i, got[i], want[i])
		}
	}
}
