package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type regionalHistoryObsFake struct {
	rows      []domain.RegionalObservation
	err       error
	from, to  time.Time
	probe     string
	listCalls int
}

func (f *regionalHistoryObsFake) ListObservations(_ context.Context, monitorID int64, probeID string, from, to time.Time) ([]domain.RegionalObservation, error) {
	f.listCalls++
	f.probe = probeID
	f.from, f.to = from, to
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func regionalHistoryService(obs *regionalHistoryObsFake, assignments healthAssignmentRepo, allow map[int64]bool) *MonitorRegionalService {
	health := NewMonitorHealthService(
		healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: {ID: 7, Active: true, Interval: 60, Timeout: 5}}},
		assignments, &healthRegionalRepo{}, healthAccess{allow: allow})
	svc := NewMonitorRegionalService(health, &regionalLabelRepo{})
	svc.SetHistory(obs, assignments)
	return svc
}

func TestMonitorRegionalHistoryRelationship(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	from, to := now.Add(-time.Hour), now
	rows := []domain.RegionalObservation{{ID: 1, MonitorID: 7, ProbeID: "11111111-1111-4111-8111-111111111111", Status: domain.StatusUp, ObservedAt: now.Add(-time.Minute)}}

	t.Run("CurrentAssignmentIsRelated", func(t *testing.T) {
		obs := &regionalHistoryObsFake{rows: rows}
		svc := regionalHistoryService(obs, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Assignments: []domain.ProbeAssignment{{ProbeID: "11111111-1111-4111-8111-111111111111", Generation: 1}}}}}, map[int64]bool{7: true})
		got, err := svc.RegionalHistory(context.Background(), 1, 7, "11111111-1111-4111-8111-111111111111", from, to)
		if err != nil || len(got) != 1 {
			t.Fatalf("history: %v %v", got, err)
		}
	})

	t.Run("HistoricalAssignmentIsRelated", func(t *testing.T) {
		obs := &regionalHistoryObsFake{rows: rows}
		svc := regionalHistoryService(obs, healthAssignmentRepo{
			sets:    map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7}},
			history: map[int64][]domain.AssignmentInterval{7: {{ProbeID: "11111111-1111-4111-8111-111111111111", From: now.Add(-2 * time.Hour), To: now.Add(-time.Minute)}}},
		}, map[int64]bool{7: true})
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "11111111-1111-4111-8111-111111111111", from, to); err != nil {
			t.Fatalf("historical relationship: %v", err)
		}
	})

	t.Run("ClosedHistoryOutsideWindowIsUnrelated", func(t *testing.T) {
		obs := &regionalHistoryObsFake{rows: rows}
		svc := regionalHistoryService(obs, healthAssignmentRepo{
			sets:    map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7}},
			history: map[int64][]domain.AssignmentInterval{7: {{ProbeID: "11111111-1111-4111-8111-111111111111", From: now.Add(-2 * time.Hour), To: now.Add(-90 * time.Minute)}}},
		}, map[int64]bool{7: true})
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "11111111-1111-4111-8111-111111111111", from, to); !errors.Is(err, ErrProbeNotRelated) {
			t.Fatalf("want ErrProbeNotRelated, got %v", err)
		}
	})

	t.Run("UnrelatedProbeReadsAsMissing", func(t *testing.T) {
		obs := &regionalHistoryObsFake{rows: rows}
		svc := regionalHistoryService(obs, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Assignments: []domain.ProbeAssignment{{ProbeID: "local", Generation: 1}}}}}, map[int64]bool{7: true})
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "22222222-2222-4222-8222-222222222222", from, to); !errors.Is(err, ErrProbeNotRelated) {
			t.Fatalf("want ErrProbeNotRelated, got %v", err)
		}
		if obs.listCalls != 0 {
			t.Fatalf("unrelated probe must fail before any evidence read: %d calls", obs.listCalls)
		}
	})

	t.Run("HiddenMonitorStaysNotFound", func(t *testing.T) {
		obs := &regionalHistoryObsFake{rows: rows}
		svc := regionalHistoryService(obs, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Assignments: []domain.ProbeAssignment{{ProbeID: "local", Generation: 1}}}}}, map[int64]bool{})
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "local", from, to); !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

// TestMonitorRegionalHistoryBoundsAreUTC asserts the Location() of the bounds
// the repository receives. An in-memory fake that compares instants cannot
// catch a local-zoned bound: the wall clock rendered into SQL is what shifts.
func TestMonitorRegionalHistoryBoundsAreUTC(t *testing.T) {
	local := time.FixedZone("UTC+7", 7*3600)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, local)
	obs := &regionalHistoryObsFake{}
	svc := regionalHistoryService(obs, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Assignments: []domain.ProbeAssignment{{ProbeID: "local", Generation: 1}}}}}, map[int64]bool{7: true})
	if _, err := svc.RegionalHistory(context.Background(), 1, 7, "local", now.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if obs.from.Location() != time.UTC || obs.to.Location() != time.UTC {
		t.Fatalf("repository bounds must be UTC, got %v / %v", obs.from.Location(), obs.to.Location())
	}
	if !obs.from.Equal(now.Add(-time.Hour).UTC()) || !obs.to.Equal(now.UTC()) {
		t.Fatalf("bounds shifted: %v / %v", obs.from, obs.to)
	}
}

func TestMonitorRegionalHistoryFailures(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	from, to := now.Add(-time.Hour), now
	assignments := healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Assignments: []domain.ProbeAssignment{{ProbeID: "local", Generation: 1}}}}}

	t.Run("InvalidWindow", func(t *testing.T) {
		svc := regionalHistoryService(&regionalHistoryObsFake{}, assignments, map[int64]bool{7: true})
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "local", to, from); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("want ErrValidation, got %v", err)
		}
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "", from, to); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("want ErrValidation for empty probe, got %v", err)
		}
	})

	t.Run("StorageFailureIsNotRelationship", func(t *testing.T) {
		obs := &regionalHistoryObsFake{err: errors.New("boom")}
		svc := regionalHistoryService(obs, assignments, map[int64]bool{7: true})
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "local", from, to); err == nil || errors.Is(err, ErrProbeNotRelated) {
			t.Fatalf("storage failure must propagate, got %v", err)
		}
	})

	t.Run("UnwiredHistoryStaysInternal", func(t *testing.T) {
		svc := NewMonitorRegionalService(NewMonitorHealthService(healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: {ID: 7}}}, assignments, &healthRegionalRepo{}, healthAccess{allow: map[int64]bool{7: true}}), &regionalLabelRepo{})
		if _, err := svc.RegionalHistory(context.Background(), 1, 7, "local", from, to); !errors.Is(err, domain.ErrInternal) {
			t.Fatalf("want ErrInternal, got %v", err)
		}
	})
}

func TestMonitorRegionalOverallHistory(t *testing.T) {
	from := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	remote := "11111111-1111-4111-8111-111111111111"

	t.Run("LocalOnlyPreservesMeasuredStream", func(t *testing.T) {
		svc := regionalHistoryService(&regionalHistoryObsFake{}, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown, Assignments: []domain.ProbeAssignment{{ProbeID: domain.LocalProbeID, Generation: 1}}}}}, map[int64]bool{7: true})
		got, err := svc.OverallHistory(context.Background(), 1, 7, from, to)
		if err != nil || !got.LocalOnly || len(got.Rows) != 0 {
			t.Fatalf("local-only must stay measured: %+v %v", got, err)
		}
	})

	t.Run("LegacyMonitorReadsAsLocal", func(t *testing.T) {
		svc := regionalHistoryService(&regionalHistoryObsFake{}, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7}}}, map[int64]bool{7: true})
		got, err := svc.OverallHistory(context.Background(), 1, 7, from, to)
		if err != nil || !got.LocalOnly {
			t.Fatalf("legacy monitor must read as local: %+v %v", got, err)
		}
	})

	t.Run("RemoteMemberServesOverallSegments", func(t *testing.T) {
		assignments := healthAssignmentRepo{
			sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown, Assignments: []domain.ProbeAssignment{{ProbeID: domain.LocalProbeID, Generation: 1}, {ProbeID: remote, Generation: 1}}}},
			history: map[int64][]domain.AssignmentInterval{7: {
				{ProbeID: domain.LocalProbeID, From: from, To: to, Policy: domain.HealthPolicyAnyDown, Revision: 3},
				{ProbeID: remote, From: from, To: to, Policy: domain.HealthPolicyAnyDown, Revision: 3},
			}},
		}
		region := &healthRegionalRepo{observations: map[int64][]domain.RegionalObservation{7: {
			{MonitorID: 7, ProbeID: remote, Status: domain.StatusUp, ObservedAt: from.Add(time.Minute), ReceivedAt: from.Add(time.Minute), AssignmentGeneration: 1, ConfigRevision: 1, Seq: 1, StreamID: "s"},
		}}}
		health := NewMonitorHealthService(healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: {ID: 7, Active: true, Interval: 60, Timeout: 5}}}, assignments, region, healthAccess{allow: map[int64]bool{7: true}})
		svc := NewMonitorRegionalService(health, &regionalLabelRepo{})
		svc.SetHistory(&regionalHistoryObsFake{}, assignments)

		got, err := svc.OverallHistory(context.Background(), 1, 7, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if got.LocalOnly {
			t.Fatal("a remote member must switch the stream to overall")
		}
		if len(got.Rows) != len(got.Intervals) || len(got.Rows) == 0 {
			t.Fatalf("rows must mirror the policy intervals: %+v", got)
		}
		for i, row := range got.Rows {
			if row.From != got.Intervals[i].From || row.Status != got.Intervals[i].Status || row.Reason != got.Intervals[i].Reason {
				t.Fatalf("row %d drifted from its interval: %+v vs %+v", i, row, got.Intervals[i])
			}
			if i > 0 && row.Important != (row.Status != got.Rows[i-1].Status) {
				t.Fatalf("row %d importance must mark status changes: %+v", i, row)
			}
		}
	})

	t.Run("HistoryBoundsAreUTC", func(t *testing.T) {
		// healthAssignmentRepo.ListHistory rejects non-UTC bounds outright, so a
		// local-zoned input that reached storage would fail here.
		local := time.FixedZone("UTC+7", 7*3600)
		at := time.Date(2026, 9, 27, 12, 0, 0, 0, local)
		svc := regionalHistoryService(&regionalHistoryObsFake{}, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown, Assignments: []domain.ProbeAssignment{{ProbeID: remote, Generation: 1}}}}}, map[int64]bool{7: true})
		if _, err := svc.OverallHistory(context.Background(), 1, 7, at.Add(-time.Hour), at); err != nil {
			t.Fatalf("bounds must be normalized to UTC before storage: %v", err)
		}
	})

	t.Run("HiddenMonitorStaysNotFound", func(t *testing.T) {
		svc := regionalHistoryService(&regionalHistoryObsFake{}, healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: {MonitorID: 7, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown, Assignments: []domain.ProbeAssignment{{ProbeID: remote, Generation: 1}}}}}, map[int64]bool{})
		if _, err := svc.OverallHistory(context.Background(), 1, 7, from, to); !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}
