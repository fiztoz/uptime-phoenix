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
	sets    map[int64]*domain.MonitorProbeAssignments
	history map[int64][]domain.AssignmentInterval
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

func (r healthAssignmentRepo) ListHistory(_ context.Context, monitorID int64, from, to time.Time) ([]domain.AssignmentInterval, error) {
	if from.Location() != time.UTC || to.Location() != time.UTC {
		return nil, errors.New("history bounds must be UTC")
	}
	return r.history[monitorID], nil
}

type healthRegionalRepo struct {
	states         map[int64][]domain.RegionalState
	observations   map[int64][]domain.RegionalObservation
	listCalls      int
	listRangeCalls int
	lastMonitorID  int64
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

func (r *healthRegionalRepo) ListObservationsInRange(_ context.Context, monitorID int64, _, _ time.Time) ([]domain.RegionalObservation, error) {
	r.listRangeCalls++
	r.lastMonitorID = monitorID
	return r.observations[monitorID], nil
}

type healthProjectionRepo struct {
	states    map[int64]domain.MonitorHealthState
	history   map[int64][]domain.MonitorHealthInterval
	dirty     []domain.DirtyBucket
	putCalls  int
	lastState *domain.MonitorHealthState
}

func (r *healthProjectionRepo) PutHealthState(_ context.Context, state *domain.MonitorHealthState) error {
	r.putCalls++
	if r.states == nil {
		r.states = map[int64]domain.MonitorHealthState{}
	}
	r.states[state.MonitorID] = *state
	copied := *state
	r.lastState = &copied
	return nil
}

func (r *healthProjectionRepo) GetHealthState(_ context.Context, monitorID int64) (*domain.MonitorHealthState, error) {
	state, ok := r.states[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	out := state
	return &out, nil
}

func (r *healthProjectionRepo) ReplaceHealthHistory(_ context.Context, monitorID int64, from, to time.Time, intervals []domain.MonitorHealthInterval) error {
	if r.history == nil {
		r.history = map[int64][]domain.MonitorHealthInterval{}
	}
	kept := make([]domain.MonitorHealthInterval, 0, len(r.history[monitorID]))
	for _, interval := range r.history[monitorID] {
		if interval.To.After(from) && interval.From.Before(to) {
			continue
		}
		kept = append(kept, interval)
	}
	r.history[monitorID] = append(kept, intervals...)
	return nil
}

func (r *healthProjectionRepo) ListHealthHistory(_ context.Context, monitorID int64, _, _ time.Time) ([]domain.MonitorHealthInterval, error) {
	return r.history[monitorID], nil
}

func (r *healthProjectionRepo) MarkDirty(_ context.Context, buckets []domain.DirtyBucket) error {
	r.dirty = append(r.dirty, buckets...)
	return nil
}

func (r *healthProjectionRepo) ListDirty(_ context.Context, resolution string, _ int) ([]domain.DirtyBucket, error) {
	out := make([]domain.DirtyBucket, 0, len(r.dirty))
	for _, bucket := range r.dirty {
		if bucket.Resolution == resolution {
			out = append(out, bucket)
		}
	}
	return out, nil
}

func (r *healthProjectionRepo) ClearDirty(_ context.Context, buckets []domain.DirtyBucket) error {
	remain := r.dirty[:0]
	for _, existing := range r.dirty {
		keep := true
		for _, bucket := range buckets {
			if existing.MonitorID == bucket.MonitorID && existing.ProbeID == bucket.ProbeID &&
				existing.Resolution == bucket.Resolution && existing.Bucket.Equal(bucket.Bucket) {
				keep = false
				break
			}
		}
		if keep {
			remain = append(remain, existing)
		}
	}
	r.dirty = remain
	return nil
}

var _ ports.MonitorHealthProjectionRepository = (*healthProjectionRepo)(nil)

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

func TestMonitorHealthHistoryAndProjection(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 2, 0, 0, time.UTC)
	start := now.Add(-2 * time.Minute)
	monitor := &domain.Monitor{ID: 7, Active: true, Interval: 60, RetryInterval: 0, Timeout: 5}
	set := &domain.MonitorProbeAssignments{
		MonitorID: 7, Revision: 1, HealthPolicy: domain.HealthPolicyAnyDown,
		Assignments: []domain.ProbeAssignment{
			{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1, CreatedAt: start},
			{MonitorID: 7, ProbeID: "asia", Generation: 1, CreatedAt: start},
		},
	}
	observations := []domain.RegionalObservation{
		{ID: 1, MonitorID: 7, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: start},
		{ID: 2, MonitorID: 7, ProbeID: "asia", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: start},
	}
	regional := &healthRegionalRepo{
		states: map[int64][]domain.RegionalState{
			7: {
				{MonitorID: 7, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1, Status: domain.StatusUp, ObservedAt: start},
				{MonitorID: 7, ProbeID: "asia", AssignmentGeneration: 1, Status: domain.StatusDown, ObservedAt: start},
			},
		},
		observations: map[int64][]domain.RegionalObservation{7: observations, 8: {{MonitorID: 8, ProbeID: "other"}}},
	}
	store := &healthProjectionRepo{}
	svc := NewMonitorHealthService(
		healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: monitor}},
		healthAssignmentRepo{
			sets: map[int64]*domain.MonitorProbeAssignments{7: set},
			history: map[int64][]domain.AssignmentInterval{7: {
				{ProbeID: domain.LocalProbeID, Generation: 1, Revision: 1, Policy: domain.HealthPolicyAnyDown, From: start},
				{ProbeID: "asia", Generation: 1, Revision: 1, Policy: domain.HealthPolicyAnyDown, From: start},
			}},
		},
		regional,
		healthAccess{allow: map[int64]bool{7: true}},
	)
	svc.SetProjections(store)

	history, err := svc.History(context.Background(), 1, 7, start, now)
	if err != nil || len(history.Intervals) == 0 || history.Intervals[0].Status != domain.StatusDown {
		t.Fatalf("history: %+v %v", history, err)
	}
	if regional.lastMonitorID != 7 {
		t.Fatal("history leaked another monitor")
	}

	denied := NewMonitorHealthService(
		healthMonitorRepo{monitors: map[int64]*domain.Monitor{7: monitor}},
		healthAssignmentRepo{sets: map[int64]*domain.MonitorProbeAssignments{7: set}},
		regional,
		healthAccess{allow: map[int64]bool{}},
	)
	calls := regional.listRangeCalls
	if _, err := denied.History(context.Background(), 2, 7, start, now); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("no-grant history: %v", err)
	}
	if regional.listRangeCalls != calls {
		t.Fatal("no-grant history queried observations")
	}

	if err := svc.ProjectCurrent(context.Background(), 7, now); err != nil {
		t.Fatal(err)
	}
	if store.lastState == nil || store.lastState.Status != domain.StatusDown || store.lastState.ProjectionVersion != 1 {
		t.Fatalf("first projection: %+v", store.lastState)
	}
	if err := svc.ProjectCurrent(context.Background(), 7, now); err != nil {
		t.Fatal(err)
	}
	if store.lastState.ProjectionVersion != 1 || store.putCalls != 2 {
		t.Fatalf("unchanged snapshot bumped version: %+v puts=%d", store.lastState, store.putCalls)
	}
	set.HealthPolicy = domain.HealthPolicyAllDown
	if err := svc.ProjectCurrent(context.Background(), 7, now); err != nil {
		t.Fatal(err)
	}
	if store.lastState.Status != domain.StatusUp || store.lastState.ProjectionVersion != 2 {
		t.Fatalf("policy change: %+v", store.lastState)
	}
	set.HealthPolicy = domain.HealthPolicyAnyDown

	bucket := start.Truncate(time.Minute)
	store.dirty = []domain.DirtyBucket{{MonitorID: 7, ProbeID: "asia", Resolution: domain.DirtyResolutionOverall, Bucket: bucket}}
	processed, err := svc.ProcessDirty(context.Background(), now, 10)
	if err != nil || processed != 1 || len(store.dirty) != 0 {
		t.Fatalf("process dirty: processed=%d dirty=%d err=%v", processed, len(store.dirty), err)
	}
	rows := store.history[7]
	if len(rows) == 0 || rows[0].Status != domain.StatusDown {
		t.Fatalf("persisted history: %+v", rows)
	}
}
