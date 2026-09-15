package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type healthAccess struct {
	allow map[int64]bool
	err   error
}

func (a healthAccess) CanViewMonitor(_ context.Context, _, monitorID int64) (bool, error) {
	if a.err != nil {
		return false, a.err
	}
	return a.allow[monitorID], nil
}

type healthMonitorRepo struct {
	monitors map[int64]*domain.Monitor
}

func (r healthMonitorRepo) Create(context.Context, *domain.Monitor) error { return nil }
func (r healthMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	m, ok := r.monitors[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return m, nil
}
func (r healthMonitorRepo) GetByPushToken(context.Context, string) (*domain.Monitor, error) {
	return nil, ports.ErrNotFound
}
func (r healthMonitorRepo) List(context.Context, ports.MonitorFilter) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r healthMonitorRepo) ListActive(context.Context) ([]*domain.Monitor, error) { return nil, nil }
func (r healthMonitorRepo) Update(context.Context, *domain.Monitor) error         { return nil }
func (r healthMonitorRepo) Delete(context.Context, int64) error                   { return nil }
func (r healthMonitorRepo) ClaimBatch(context.Context, string, int, time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r healthMonitorRepo) RefreshLease(context.Context, string) (int64, error)  { return 0, nil }
func (r healthMonitorRepo) ReleaseLeases(context.Context, string) (int64, error) { return 0, nil }

type healthAssignmentRepo struct {
	sets map[int64]*domain.MonitorProbeAssignments
}

func (r healthAssignmentRepo) InitializeLocal(context.Context, int64) (*domain.MonitorProbeAssignments, error) {
	return nil, nil
}
func (r healthAssignmentRepo) GetByMonitorID(_ context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	set, ok := r.sets[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return set, nil
}
func (r healthAssignmentRepo) Replace(context.Context, int64, int64, []string, domain.HealthPolicy) (*domain.MonitorProbeAssignments, error) {
	return nil, nil
}
func (r healthAssignmentRepo) ExecutableByLocal(context.Context, []int64) (map[int64]struct{}, error) {
	return map[int64]struct{}{}, nil
}

type healthRegionalRepo struct {
	states        map[int64][]domain.RegionalState
	listCalls     int
	lastMonitorID int64
}

func (r *healthRegionalRepo) Commit(context.Context, domain.RegionalCommit) error { return nil }
func (r *healthRegionalRepo) GetState(context.Context, int64, string) (*domain.RegionalState, error) {
	return nil, ports.ErrNotFound
}
func (r *healthRegionalRepo) ListStates(_ context.Context, monitorID int64) ([]domain.RegionalState, error) {
	r.listCalls++
	r.lastMonitorID = monitorID
	return r.states[monitorID], nil
}
func (r *healthRegionalRepo) ListObservations(context.Context, int64, string, time.Time, time.Time) ([]domain.RegionalObservation, error) {
	return nil, nil
}

var (
	_ ports.MonitorRepository                = healthMonitorRepo{}
	_ ports.MonitorProbeAssignmentRepository = healthAssignmentRepo{}
	_ ports.RegionalCommitRepository         = (*healthRegionalRepo)(nil)
)

func TestMonitorHealthCurrentPolicyAndAccess(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	localZone := now.In(time.FixedZone("UTC+7", 7*3600))
	monitor := &domain.Monitor{ID: 7, Active: true, Interval: 60, RetryInterval: 0, Timeout: 5}
	set := &domain.MonitorProbeAssignments{
		MonitorID: 7, HealthPolicy: domain.HealthPolicyAnyDown,
		Assignments: []domain.ProbeAssignment{
			{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1},
			{MonitorID: 7, ProbeID: "asia", Generation: 1},
		},
	}
	states := []domain.RegionalState{
		{MonitorID: 7, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: localZone},
		{MonitorID: 7, ProbeID: "asia", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: now},
	}
	regional := &healthRegionalRepo{states: map[int64][]domain.RegionalState{7: states, 8: {{MonitorID: 8, ProbeID: "other"}}}}
	svc := NewMonitorHealthService(
		healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: monitor}},
		healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: set}},
		regional,
		healthAccess{allow: map[int64]bool{7: true}},
	)

	got, err := svc.Current(context.Background(), 1, 7, now)
	if err != nil || got.Health.Status != domain.StatusDown || got.Policy != domain.HealthPolicyAnyDown {
		t.Fatalf("any_down: %+v %v", got, err)
	}
	if regional.lastMonitorID != 7 || len(got.Regions) != 2 {
		t.Fatalf("leaked or incomplete regions: last=%d regions=%+v", regional.lastMonitorID, got.Regions)
	}

	set.HealthPolicy = domain.HealthPolicyAllDown
	got, err = svc.Current(context.Background(), 1, 7, now)
	if err != nil || got.Health.Status != domain.StatusUp {
		t.Fatalf("all_down: %+v %v", got, err)
	}

	denied := NewMonitorHealthService(
		healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: monitor}},
		healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: set}},
		regional,
		healthAccess{allow: map[int64]bool{}},
	)
	calls := regional.listCalls
	if _, err := denied.Current(context.Background(), 2, 7, now); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("no-grant: %v", err)
	}
	if regional.listCalls != calls {
		t.Fatal("no-grant reader queried regional state")
	}

	anyDown := *set
	anyDown.HealthPolicy = domain.HealthPolicyAnyDown
	missing := NewMonitorHealthService(
		healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: monitor}},
		healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: &anyDown}},
		&healthRegionalRepo{states: map[int64][]domain.RegionalState{
			7: {{MonitorID: 7, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: now}},
		}},
		healthAccess{allow: map[int64]bool{7: true}},
	)
	got, err = missing.Current(context.Background(), 1, 7, now)
	if err != nil || got.Health.Status != domain.StatusUnknown || got.Health.Reason != "incomplete_evidence" {
		t.Fatalf("missing remote evidence: %+v %v", got, err)
	}
}
