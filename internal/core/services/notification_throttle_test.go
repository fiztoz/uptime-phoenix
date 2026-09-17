package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type throttleCall struct {
	key      domain.NotificationThrottleKey
	at       time.Time
	interval time.Duration
}

type throttleSpy struct {
	allowed bool
	err     error
	calls   []throttleCall
	cleared []domain.NotificationThrottleKey
}

type throttleGroupSpy struct{ calls int }

func (g *throttleGroupSpy) OnHeartbeat(context.Context, *domain.Monitor) { g.calls++ }

func (s *throttleSpy) Reserve(_ context.Context, key domain.NotificationThrottleKey, at time.Time, interval time.Duration) (bool, error) {
	s.calls = append(s.calls, throttleCall{key: key, at: at, interval: interval})
	return s.allowed, s.err
}

func (s *throttleSpy) Clear(_ context.Context, key domain.NotificationThrottleKey) error {
	s.cleared = append(s.cleared, key)
	return s.err
}

func TestDispatcherDurableThrottleGuards(t *testing.T) {
	ctx := context.Background()
	for name, storeErr := range map[string]error{"not_due": nil, "storage_error": errors.New("storage unavailable")} {
		t.Run(name, func(t *testing.T) {
			notif := &fakeNotifier{}
			store := &throttleSpy{err: storeErr}
			d := newDispatcher(notif, &fakeMaintenance{})
			d.SetThrottleRepository(store)
			alerts := NewAlertService(newFakeAlertRepo())
			d.SetAlertLifecycle(alerts)
			monitor := &domain.Monitor{ID: 1, ResendInterval: 5}
			hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown, ProbeID: domain.LocalProbeID, AssignmentGeneration: 3}
			d.OnHeartbeat(ctx, monitor, hb, nil)
			rows, err := alerts.List(ctx, ports.AlertFilter{})
			if err != nil || len(rows) != 0 || notif.count() != 0 || len(store.calls) != 1 {
				t.Fatalf("denied reservation performed work: alerts=%d notifications=%d calls=%d err=%v", len(rows), notif.count(), len(store.calls), err)
			}
		})
	}
}

func TestDispatcherThrottleIdentityAndUTC(t *testing.T) {
	ctx := context.Background()
	notif := &fakeNotifier{}
	store := &throttleSpy{allowed: true}
	maintenance := &fakeMaintenance{}
	d := newDispatcher(notif, maintenance)
	d.SetThrottleRepository(store)
	d.now = func() time.Time { return time.Date(2026, 9, 17, 18, 0, 0, 0, time.FixedZone("UTC+7", 7*3600)) }
	monitor := &domain.Monitor{ID: 1, ResendInterval: 5}
	hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown, AssignmentGeneration: 3}
	d.OnHeartbeat(ctx, monitor, hb, ptrStatus(domain.StatusDown))
	if len(store.calls) != 1 || store.calls[0].key != (domain.NotificationThrottleKey{MonitorID: 1, ProbeID: domain.LocalProbeID, AssignmentGeneration: 3}) || store.calls[0].at.Location() != time.UTC || store.calls[0].interval != 5*time.Minute {
		t.Fatalf("wrong reservation: %+v", store.calls)
	}
	hb.Status = domain.StatusUp
	d.OnHeartbeat(ctx, monitor, hb, ptrStatus(domain.StatusDown))
	if len(store.cleared) != 1 || store.cleared[0] != store.calls[0].key {
		t.Fatalf("recovery cleared wrong assignment: %+v", store.cleared)
	}
	maintenance.active = true
	hb.Status = domain.StatusDown
	d.OnHeartbeat(ctx, monitor, hb, nil)
	if len(store.calls) != 1 {
		t.Fatal("maintenance consumed a reservation")
	}
}

func TestDispatcherRejectsRemoteBeforeSideEffects(t *testing.T) {
	ctx := context.Background()
	notif := &fakeNotifier{}
	store := &throttleSpy{allowed: true}
	d := newDispatcher(notif, &fakeMaintenance{})
	d.SetThrottleRepository(store)
	groups := &throttleGroupSpy{}
	d.SetGroupEvaluator(groups)
	alerts := NewAlertService(newFakeAlertRepo())
	d.SetAlertLifecycle(alerts)
	monitor := &domain.Monitor{ID: 1}
	for _, hb := range []*domain.Heartbeat{
		nil,
		{MonitorID: 1, ProbeID: "11111111-1111-4111-8111-111111111111", AssignmentGeneration: 1, Status: domain.StatusDown},
		{MonitorID: 2, Status: domain.StatusDown},
		{MonitorID: 1, AssignmentGeneration: -1, Status: domain.StatusDown},
	} {
		d.OnHeartbeat(ctx, monitor, hb, nil)
	}
	rows, err := alerts.List(ctx, ports.AlertFilter{})
	if err != nil || len(rows) != 0 || notif.count() != 0 || len(store.calls) != 0 || groups.calls != 0 {
		t.Fatalf("invalid heartbeat performed work: alerts=%d notifications=%d reservations=%d groups=%d err=%v", len(rows), notif.count(), len(store.calls), groups.calls, err)
	}
}

func TestDispatcherAckAndDisabledResendDoNotReserve(t *testing.T) {
	ctx := context.Background()
	notif := &fakeNotifier{}
	store := &throttleSpy{allowed: true}
	d := newDispatcher(notif, &fakeMaintenance{})
	d.SetThrottleRepository(store)
	alerts := NewAlertService(newFakeAlertRepo())
	d.SetAlertLifecycle(alerts)
	monitor := &domain.Monitor{ID: 1}
	hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}
	d.OnHeartbeat(ctx, monitor, hb, ptrStatus(domain.StatusDown))
	a, err := alerts.OpenOnDown(ctx, monitor, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alerts.Acknowledge(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	monitor.ResendInterval = 5
	d.OnHeartbeat(ctx, monitor, hb, ptrStatus(domain.StatusDown))
	if len(store.calls) != 0 || notif.count() != 0 {
		t.Fatal("acknowledged or disabled resend consumed a reservation")
	}
}
