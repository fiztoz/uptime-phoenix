package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type conditionRepoFake struct {
	items map[string]*domain.MonitorCondition
}

func newConditionRepoFake() *conditionRepoFake {
	return &conditionRepoFake{items: make(map[string]*domain.MonitorCondition)}
}

func conditionKey(monitorID int64, kind string) string {
	return fmt.Sprintf("%d/%s", monitorID, kind)
}

func (r *conditionRepoFake) Upsert(_ context.Context, condition *domain.MonitorCondition) error {
	r.items[conditionKey(condition.MonitorID, condition.Kind)] = cloneMonitorCondition(condition)
	return nil
}

func (r *conditionRepoFake) Get(_ context.Context, monitorID int64, kind string) (*domain.MonitorCondition, error) {
	condition, ok := r.items[conditionKey(monitorID, kind)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return cloneMonitorCondition(condition), nil
}

func (r *conditionRepoFake) ListAll(_ context.Context) ([]*domain.MonitorCondition, error) {
	return r.list(nil), nil
}

func (r *conditionRepoFake) ListByMonitorIDs(_ context.Context, monitorIDs []int64) ([]*domain.MonitorCondition, error) {
	allowed := make(map[int64]struct{}, len(monitorIDs))
	for _, id := range monitorIDs {
		allowed[id] = struct{}{}
	}
	return r.list(allowed), nil
}

func (r *conditionRepoFake) list(allowed map[int64]struct{}) []*domain.MonitorCondition {
	conditions := make([]*domain.MonitorCondition, 0, len(r.items))
	for _, condition := range r.items {
		if allowed != nil {
			if _, ok := allowed[condition.MonitorID]; !ok {
				continue
			}
		}
		conditions = append(conditions, cloneMonitorCondition(condition))
	}
	sort.Slice(conditions, func(i, j int) bool {
		if conditions[i].MonitorID != conditions[j].MonitorID {
			return conditions[i].MonitorID < conditions[j].MonitorID
		}
		return conditions[i].Kind < conditions[j].Kind
	})
	return conditions
}

func (r *conditionRepoFake) DeleteKind(_ context.Context, monitorID int64, kind string) error {
	delete(r.items, conditionKey(monitorID, kind))
	return nil
}

func (r *conditionRepoFake) DeleteByMonitor(_ context.Context, monitorID int64) error {
	for key, condition := range r.items {
		if condition.MonitorID == monitorID {
			delete(r.items, key)
		}
	}
	return nil
}

func cloneMonitorCondition(condition *domain.MonitorCondition) *domain.MonitorCondition {
	if condition == nil {
		return nil
	}
	clone := *condition
	clone.Used = cloneFloat(condition.Used)
	clone.Limit = cloneFloat(condition.Limit)
	clone.Percent = cloneFloat(condition.Percent)
	clone.Threshold = cloneFloat(condition.Threshold)
	clone.LastSuccessAt = cloneTime(condition.LastSuccessAt)
	clone.LastNotifiedAt = cloneTime(condition.LastNotifiedAt)
	return &clone
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

type conditionNotifierFake struct {
	calls     []domain.AlertContext
	delivered bool
	err       error
}

func (n *conditionNotifierFake) DispatchTracked(_ context.Context, _ *domain.Monitor, alert domain.AlertContext) (bool, error) {
	n.calls = append(n.calls, alert)
	return n.delivered, n.err
}

type conditionBusFake struct {
	events []ports.Event
}

func (b *conditionBusFake) Publish(_ context.Context, event ports.Event) error {
	b.events = append(b.events, event)
	return nil
}

func (b *conditionBusFake) Subscribe(string) <-chan ports.Event { return nil }

func (b *conditionBusFake) Close() {}

func publishedStates(bus *conditionBusFake) []domain.ConditionState {
	out := make([]domain.ConditionState, 0, len(bus.events))
	for _, event := range bus.events {
		condition, ok := event.Payload.(*domain.MonitorCondition)
		if !ok || condition == nil {
			continue
		}
		out = append(out, condition.State)
	}
	return out
}

func assertConditionRow(t *testing.T, stored *domain.MonitorCondition, state, consecutive domain.ConditionState, count int, notified domain.ConditionState) {
	t.Helper()
	if stored.State != state || stored.ConsecutiveState != consecutive || stored.ConsecutiveCount != count || stored.LastNotifiedState != notified {
		t.Fatalf("row state=%q consecutive=%q count=%d notified=%q; want state=%q consecutive=%q count=%d notified=%q",
			stored.State, stored.ConsecutiveState, stored.ConsecutiveCount, stored.LastNotifiedState,
			state, consecutive, count, notified)
	}
}

type conditionMaintenanceFake struct{ active bool }

func (m *conditionMaintenanceFake) IsActive(_ context.Context, _ int64) (bool, error) {
	return m.active, nil
}

func conditionObservation(state domain.ConditionState, percent float64, observedAt time.Time) domain.ConditionObservation {
	used := percent
	limit := 100.0
	threshold := 80.0
	return domain.ConditionObservation{
		Kind:       domain.MonitorConditionSessionPool,
		State:      state,
		Used:       &used,
		Limit:      &limit,
		Percent:    &percent,
		Threshold:  &threshold,
		Unit:       "connections",
		Resource:   "Session pool",
		Scope:      "cluster",
		Source:     "test statistics",
		Message:    fmt.Sprintf("session pool is %.1f%%", percent),
		ObservedAt: observedAt,
		StaleAfter: observedAt.Add(3 * time.Minute),
	}
}

func conditionTestMonitor() *domain.Monitor {
	return &domain.Monitor{ID: 42, UserID: 7, Name: "primary-db", Type: "database", Config: map[string]any{"engine": "postgres"}}
}

func TestMonitorConditionService_StaleAfterFollowsMonitorInterval(t *testing.T) {
	repo := newConditionRepoFake()
	svc := NewMonitorConditionService(repo, &conditionNotifierFake{delivered: true}, &conditionMaintenanceFake{}, nil)
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	monitor.Interval = 120
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{
		conditionObservation(domain.ConditionStateOK, 10, now),
	})
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(6 * time.Minute)
	if !stored.StaleAfter.Equal(want) {
		t.Fatalf("stale_after=%s, want %s", stored.StaleAfter, want)
	}
}

func TestMonitorConditionService_DebouncesWarningAndDoesNotChangeAvailability(t *testing.T) {
	repo := newConditionRepoFake()
	notifier := &conditionNotifierFake{delivered: true}
	bus := &conditionBusFake{}
	svc := NewMonitorConditionService(repo, notifier, &conditionMaintenanceFake{}, bus)
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	observation := conditionObservation(domain.ConditionStateWarning, 92, now)

	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{observation})
	if len(notifier.calls) != 0 {
		t.Fatalf("first warning dispatched %d alerts, want 0", len(notifier.calls))
	}
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, "", domain.ConditionStateWarning, 1, "")
	if len(publishedStates(bus)) != 0 {
		t.Fatalf("first warning published %v, want none", publishedStates(bus))
	}

	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{observation})
	if len(notifier.calls) != 1 {
		t.Fatalf("second warning dispatched %d alerts, want 1", len(notifier.calls))
	}
	stored, err = repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, domain.ConditionStateWarning, domain.ConditionStateWarning, 2, domain.ConditionStateWarning)
	alert := notifier.calls[0]
	if alert.EventKind != domain.AlertEventCapacityCondition || alert.Status != domain.StatusUp {
		t.Fatalf("alert kind/status = %q/%s, want capacity_condition/UP", alert.EventKind, alert.Status)
	}
	if alert.ConditionState != domain.ConditionStateWarning || alert.ConditionPercent == nil || *alert.ConditionPercent != 92 {
		t.Fatalf("condition alert = %+v", alert)
	}

	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{observation})
	if len(notifier.calls) != 1 {
		t.Fatalf("stable warning resent: calls=%d", len(notifier.calls))
	}
}

func TestMonitorConditionService_RecoveryRequiresTwoSamplesBelowDeadband(t *testing.T) {
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	run := func(t *testing.T, percents []float64, lastState, lastConsecutive, lastNotified domain.ConditionState, lastCount, notifies int) {
		t.Helper()
		repo := newConditionRepoFake()
		notifier := &conditionNotifierFake{delivered: true}
		svc := NewMonitorConditionService(repo, notifier, &conditionMaintenanceFake{}, &conditionBusFake{})
		svc.now = func() time.Time { return now }
		monitor := conditionTestMonitor()
		for i, percent := range percents {
			state := domain.ConditionStateOK
			if percent >= 80 {
				state = domain.ConditionStateWarning
			}
			svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{
				conditionObservation(state, percent, now.Add(time.Duration(i)*time.Minute)),
			})
		}
		stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
		if err != nil {
			t.Fatal(err)
		}
		assertConditionRow(t, stored, lastState, lastConsecutive, lastCount, lastNotified)
		if len(notifier.calls) != notifies {
			t.Fatalf("notifies=%d, want %d", len(notifier.calls), notifies)
		}
	}

	t.Run("82,82 warning once", func(t *testing.T) {
		run(t, []float64{82, 82}, domain.ConditionStateWarning, domain.ConditionStateWarning, domain.ConditionStateWarning, 2, 1)
	})
	t.Run("82,82,74,77 stays warning", func(t *testing.T) {
		run(t, []float64{82, 82, 74, 77}, domain.ConditionStateWarning, domain.ConditionStateWarning, domain.ConditionStateWarning, 1, 1)
	})
	t.Run("82,82,74,74 recovers once", func(t *testing.T) {
		run(t, []float64{82, 82, 74, 74}, domain.ConditionStateOK, domain.ConditionStateOK, domain.ConditionStateOK, 2, 2)
	})
	t.Run("82,82,74,77,74,74 recovers after final pair", func(t *testing.T) {
		run(t, []float64{82, 82, 74, 77, 74, 74}, domain.ConditionStateOK, domain.ConditionStateOK, domain.ConditionStateOK, 2, 2)
	})
}

func TestMonitorConditionService_FirstErrorIsUnconfirmed(t *testing.T) {
	repo := newConditionRepoFake()
	notifier := &conditionNotifierFake{delivered: true}
	bus := &conditionBusFake{}
	svc := NewMonitorConditionService(repo, notifier, &conditionMaintenanceFake{}, bus)
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{
		conditionObservation(domain.ConditionStateError, 0, now),
	})
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, "", domain.ConditionStateError, 1, "")
	if len(notifier.calls) != 0 || len(publishedStates(bus)) != 0 {
		t.Fatalf("first error leaked: notifies=%d published=%v", len(notifier.calls), publishedStates(bus))
	}
}

func TestMonitorConditionService_ErrorAndWarningDoNotShareCounts(t *testing.T) {
	repo := newConditionRepoFake()
	notifier := &conditionNotifierFake{delivered: true}
	svc := NewMonitorConditionService(repo, notifier, &conditionMaintenanceFake{}, &conditionBusFake{})
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateError, 0, now)})
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateError, 0, now)})
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateWarning, 90, now)})
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, domain.ConditionStateError, domain.ConditionStateWarning, 1, domain.ConditionStateError)
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateWarning, 90, now)})
	stored, err = repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, domain.ConditionStateWarning, domain.ConditionStateWarning, 2, domain.ConditionStateWarning)
	if len(notifier.calls) != 2 {
		t.Fatalf("notifies=%d, want error then warning", len(notifier.calls))
	}
}

func TestMonitorConditionService_TransitionsWithoutNotifier(t *testing.T) {
	repo := newConditionRepoFake()
	svc := NewMonitorConditionService(repo, nil, &conditionMaintenanceFake{}, &conditionBusFake{})
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateWarning, 91, now)})
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateWarning, 91, now)})
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, domain.ConditionStateWarning, domain.ConditionStateWarning, 2, "")
}

func TestMonitorConditionService_RestartKeepsCandidateCount(t *testing.T) {
	repo := newConditionRepoFake()
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	monitor := conditionTestMonitor()
	first := NewMonitorConditionService(repo, &conditionNotifierFake{delivered: true}, &conditionMaintenanceFake{}, &conditionBusFake{})
	first.now = func() time.Time { return now }
	first.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateWarning, 88, now)})
	second := NewMonitorConditionService(repo, &conditionNotifierFake{delivered: true}, &conditionMaintenanceFake{}, &conditionBusFake{})
	second.now = func() time.Time { return now }
	second.OnCheck(context.Background(), monitor, []domain.ConditionObservation{conditionObservation(domain.ConditionStateWarning, 88, now)})
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, domain.ConditionStateWarning, domain.ConditionStateWarning, 2, domain.ConditionStateWarning)
}

func TestMonitorConditionService_HysteresisAndRecovery(t *testing.T) {
	repo := newConditionRepoFake()
	notifier := &conditionNotifierFake{delivered: true}
	svc := NewMonitorConditionService(repo, notifier, &conditionMaintenanceFake{}, nil)
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()

	warning := conditionObservation(domain.ConditionStateWarning, 82, now)
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{warning})
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{warning})

	insideDeadband := conditionObservation(domain.ConditionStateOK, 77, now.Add(time.Minute))
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{insideDeadband})
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.ConditionStateWarning {
		t.Fatalf("deadband state=%s, want warning", stored.State)
	}

	recovered := conditionObservation(domain.ConditionStateOK, 74, now.Add(2*time.Minute))
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{recovered})
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{recovered})
	if len(notifier.calls) != 2 {
		t.Fatalf("calls=%d, want warning and recovery", len(notifier.calls))
	}
	if notifier.calls[1].ConditionState != domain.ConditionStateOK || notifier.calls[1].ConditionPreviousState != domain.ConditionStateWarning {
		t.Fatalf("recovery alert=%+v", notifier.calls[1])
	}
}

func TestMonitorConditionService_MaintenanceAndDeliveryDoNotMarkUnsent(t *testing.T) {
	repo := newConditionRepoFake()
	notifier := &conditionNotifierFake{}
	maintenance := &conditionMaintenanceFake{active: true}
	svc := NewMonitorConditionService(repo, notifier, maintenance, nil)
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	warning := conditionObservation(domain.ConditionStateWarning, 95, now)

	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{warning})
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{warning})
	if len(notifier.calls) != 0 {
		t.Fatalf("maintenance dispatched %d alerts", len(notifier.calls))
	}

	maintenance.active = false
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{warning})
	if len(notifier.calls) != 1 {
		t.Fatalf("post-maintenance attempts=%d, want 1", len(notifier.calls))
	}
	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastNotifiedState != "" {
		t.Fatalf("undelivered alert marked as %s", stored.LastNotifiedState)
	}

	notifier.delivered = true
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{warning})
	stored, err = repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastNotifiedState != domain.ConditionStateWarning {
		t.Fatalf("delivered alert cursor=%s, want warning", stored.LastNotifiedState)
	}
}

func TestConditionStaleAfterUsesMonitorInterval(t *testing.T) {
	observed := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := conditionStaleAfter(observed, 20); !got.Equal(observed.Add(3 * time.Minute)) {
		t.Fatalf("sub-minute interval stale=%s, want 3m", got)
	}
	if got := conditionStaleAfter(observed, 120); !got.Equal(observed.Add(6 * time.Minute)) {
		t.Fatalf("120s interval stale=%s, want 6m", got)
	}
}

func TestMonitorConditionService_OverlappingChecksSerializePerKind(t *testing.T) {
	repo := newConditionRepoFake()
	notifier := &conditionNotifierFake{delivered: true}
	svc := NewMonitorConditionService(repo, notifier, &conditionMaintenanceFake{}, &conditionBusFake{})
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	observation := conditionObservation(domain.ConditionStateWarning, 91, now)

	const workers = 8
	done := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		go func() {
			svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{observation})
			done <- struct{}{}
		}()
	}
	for i := 0; i < workers; i++ {
		<-done
	}

	stored, err := repo.Get(context.Background(), monitor.ID, domain.MonitorConditionSessionPool)
	if err != nil {
		t.Fatal(err)
	}
	assertConditionRow(t, stored, domain.ConditionStateWarning, domain.ConditionStateWarning, workers, domain.ConditionStateWarning)
	if len(notifier.calls) != 1 {
		t.Fatalf("serialized overlapping checks notified %d times, want 1", len(notifier.calls))
	}
}

func TestMonitorConditionService_AllDeliveryFailuresRemainRetryable(t *testing.T) {
	repo := newConditionRepoFake()
	notifier := &conditionNotifierFake{err: errors.New("smtp unavailable")}
	svc := NewMonitorConditionService(repo, notifier, &conditionMaintenanceFake{}, nil)
	now := time.Date(2032, 1, 2, 3, 4, 5, 0, time.UTC)
	svc.now = func() time.Time { return now }
	monitor := conditionTestMonitor()
	errorObservation := conditionObservation(domain.ConditionStateError, 0, now)

	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{errorObservation})
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{errorObservation})
	notifier.err = nil
	notifier.delivered = true
	svc.OnCheck(context.Background(), monitor, []domain.ConditionObservation{errorObservation})
	if len(notifier.calls) != 2 {
		t.Fatalf("dispatch attempts=%d, want failure then retry", len(notifier.calls))
	}
}
