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
	if err != nil || len(empty.Intervals) != 1 || empty.Intervals[0].Reason != "missing_assignment_history" || empty.Durations.Unknown != time.Minute {
		t.Fatalf("no assignments: %+v %v", empty, err)
	}
}

func TestReconstructOverallHistoryAccountsForMissingMembershipIntervals(t *testing.T) {
	start := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	got, err := ReconstructOverallHistory(domain.OverallHistoryInput{
		From: start, To: start.Add(4 * time.Minute), FreshFor: 10 * time.Minute,
		Assignments: []domain.AssignmentInterval{
			{ProbeID: "local", Generation: 1, Revision: 1, Policy: domain.HealthPolicyAnyDown, From: start.Add(time.Minute), To: start.Add(2 * time.Minute)},
			{ProbeID: "local", Generation: 2, Revision: 3, Policy: domain.HealthPolicyAnyDown, From: start.Add(3 * time.Minute)},
		},
		Observations: []domain.RegionalObservation{
			{ID: 1, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: start.Add(time.Minute)},
			{ID: 2, ProbeID: "local", AssignmentGeneration: 2, Status: domain.StatusDown, ObservedAt: start.Add(3 * time.Minute)},
		},
	})
	if err != nil || len(got.Intervals) != 4 || got.Durations.Up != time.Minute || got.Durations.Down != time.Minute || got.Durations.Unknown != 2*time.Minute {
		t.Fatalf("history dropped a gap or neighboring interval: %+v %v", got, err)
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

func TestCoalesceOverallHealthIntervals_FullWindowEquivalence(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)
	t3 := t0.Add(3 * time.Minute)
	t4 := t0.Add(4 * time.Minute)
	fresh := 10 * time.Minute

	testCases := []struct {
		name         string
		policy       domain.HealthPolicy
		observations []domain.RegionalObservation
	}{
		{
			name:   "AnyDown_ExactBoundaryTransitionAndCountChange",
			policy: domain.HealthPolicyAnyDown,
			observations: []domain.RegionalObservation{
				{ID: 1, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: t0},
				{ID: 2, ProbeID: "remote", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: t0},
				// Exactly on the minute boundary (t2 = 12:02:00.000000): local goes DOWN.
				// Causes genuine UP -> DOWN transition on the minute boundary.
				{ID: 3, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: t2},
				// At 12:02:30: remote also goes DOWN.
				// Status stays DOWN, but Counts changes from {Up: 1, Down: 1} to {Up: 0, Down: 2}.
				{ID: 4, ProbeID: "remote", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: t2.Add(30 * time.Second)},
			},
		},
		{
			name:   "AllDown_StatusPreservedWithCountChangeAndBoundaryTransition",
			policy: domain.HealthPolicyAllDown,
			observations: []domain.RegionalObservation{
				{ID: 1, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: t0},
				{ID: 2, ProbeID: "remote", AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: t0},
				// At 12:01:30: local goes DOWN.
				// Under all_down, overall Status stays UP, but Counts changes to {Up: 1, Down: 1}.
				{ID: 3, ProbeID: "local", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: t1.Add(30 * time.Second)},
				// Exactly on the minute boundary (t2 = 12:02:00.000000): remote also goes DOWN.
				// Under all_down, all probes are now DOWN -> genuine UP -> DOWN transition on minute boundary.
				{ID: 4, ProbeID: "remote", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: t2},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assignments := []domain.AssignmentInterval{
				{ProbeID: "local", Generation: 1, Revision: 1, Policy: tc.policy, From: t0},
				{ProbeID: "remote", Generation: 1, Revision: 1, Policy: tc.policy, From: t0},
			}

			// 1. Reconstruct full window [t0, t4)
			full, err := ReconstructOverallHistory(domain.OverallHistoryInput{
				MonitorID: 14, From: t0, To: t4, FreshFor: fresh,
				Assignments:  assignments,
				Observations: tc.observations,
			})
			if err != nil {
				t.Fatalf("reconstruct full window: %v", err)
			}

			// 2. Reconstruct minute-by-minute simulating 1-minute dirty buckets
			var minuteRows []domain.MonitorHealthInterval
			bounds := []time.Time{t0, t1, t2, t3, t4}
			for i := 0; i < len(bounds)-1; i++ {
				mStart, mEnd := bounds[i], bounds[i+1]
				part, err := ReconstructOverallHistory(domain.OverallHistoryInput{
					MonitorID: 14, From: mStart, To: mEnd, FreshFor: fresh,
					Assignments:  assignments,
					Observations: tc.observations,
				})
				if err != nil {
					t.Fatalf("reconstruct bucket [%v, %v): %v", mStart, mEnd, err)
				}
				minuteRows = append(minuteRows, part.Intervals...)
			}

			// 3. Coalesce the concatenated minute rows
			coalesced := CoalesceOverallHealthIntervals(minuteRows)

			// 4. Assert full-window equivalence
			if len(coalesced) != len(full.Intervals) {
				t.Fatalf("length mismatch: got %d coalesced intervals, want %d full intervals\ncoalesced: %+v\nfull:      %+v",
					len(coalesced), len(full.Intervals), coalesced, full.Intervals)
			}
			for i := range full.Intervals {
				got := coalesced[i]
				want := full.Intervals[i]
				if !got.From.Equal(want.From) || !got.To.Equal(want.To) ||
					got.Status != want.Status || got.Reason != want.Reason ||
					got.Policy != want.Policy || got.PolicyRevision != want.PolicyRevision ||
					got.Counts != want.Counts || got.Cause != want.Cause {
					t.Fatalf("interval %d mismatch:\ngot:  %+v\nwant: %+v", i, got, want)
				}
			}

			// Also verify SameOverallHealthSegment matches definition
			if len(coalesced) >= 2 {
				for i := 0; i < len(coalesced)-1; i++ {
					if SameOverallHealthSegment(coalesced[i], coalesced[i+1]) && coalesced[i].To.Equal(coalesced[i+1].From) {
						t.Fatalf("adjacent intervals %d and %d should have been coalesced: %+v vs %+v",
							i, i+1, coalesced[i], coalesced[i+1])
					}
				}
			}
		})
	}

	// 5. Verify edge cases: empty input and non-positive intervals
	t.Run("EmptyAndInvalidIntervalsSkipped", func(t *testing.T) {
		if got := CoalesceOverallHealthIntervals(nil); got != nil {
			t.Fatalf("expected nil for nil input, got: %+v", got)
		}
		if got := CoalesceOverallHealthIntervals([]domain.MonitorHealthInterval{}); len(got) != 0 {
			t.Fatalf("expected empty for empty input, got: %+v", got)
		}

		valid := domain.MonitorHealthInterval{
			From: t0, To: t1, Status: domain.StatusUp, Policy: domain.HealthPolicyAnyDown, PolicyRevision: 1,
		}
		inverted := domain.MonitorHealthInterval{
			From: t1, To: t0, Status: domain.StatusUp, Policy: domain.HealthPolicyAnyDown, PolicyRevision: 1,
		}
		zero := domain.MonitorHealthInterval{
			From: t1, To: t1, Status: domain.StatusUp, Policy: domain.HealthPolicyAnyDown, PolicyRevision: 1,
		}

		coalesced := CoalesceOverallHealthIntervals([]domain.MonitorHealthInterval{inverted, valid, zero})
		if len(coalesced) != 1 || !coalesced[0].From.Equal(t0) || !coalesced[0].To.Equal(t1) {
			t.Fatalf("expected only the valid interval to survive, got: %+v", coalesced)
		}
	})
}
