package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// A hub killed after the DOWN heartbeat and its alert are committed, but
// before the provider is called, restarts with prev=DOWN. The next DOWN check
// must still send the initial alert once (at-least-once), and only once.
func TestLegacyDispatcherSendsInitialDownLostToACrash(t *testing.T) {
	ctx := context.Background()
	notif := &fakeNotifier{}
	alerts := NewAlertService(newFakeAlertRepo())
	monitor := &domain.Monitor{ID: 1}
	down := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}

	// Before the crash: the transition opened the alert, then the process died.
	if _, err := alerts.OpenOnDown(ctx, monitor, time.Now()); err != nil {
		t.Fatal(err)
	}

	// After the restart: a fresh dispatcher sees DOWN after DOWN.
	d := newDispatcher(notif, &fakeMaintenance{})
	d.SetAlertLifecycle(alerts)
	d.OnHeartbeat(ctx, monitor, down, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("initial DOWN lost to the crash: notifications=%d, want 1", notif.count())
	}
	if got := notif.last(); got.status != domain.StatusDown || got.prev == domain.StatusDown {
		t.Fatalf("recovered alert = %s after %s, want a DOWN transition", got.status, got.prev)
	}
	d.OnHeartbeat(ctx, monitor, down, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("recovered alert sent again: notifications=%d, want 1", notif.count())
	}
}

// The normal path marks the initial alert as sent, so a later restart does
// not repeat it, and an acknowledged alert is never re-sent by recovery.
func TestLegacyDispatcherDoesNotRepeatADeliveredInitialDown(t *testing.T) {
	ctx := context.Background()
	notif := &fakeNotifier{}
	alerts := NewAlertService(newFakeAlertRepo())
	monitor := &domain.Monitor{ID: 2}
	down := &domain.Heartbeat{MonitorID: 2, Status: domain.StatusDown}

	d := newDispatcher(notif, &fakeMaintenance{})
	d.SetAlertLifecycle(alerts)
	d.OnHeartbeat(ctx, monitor, down, ptrStatus(domain.StatusUp))
	if notif.count() != 1 {
		t.Fatalf("transition notifications=%d, want 1", notif.count())
	}
	restarted := newDispatcher(notif, &fakeMaintenance{})
	restarted.SetAlertLifecycle(alerts)
	restarted.OnHeartbeat(ctx, monitor, down, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("delivered initial DOWN repeated after restart: notifications=%d", notif.count())
	}

	acked := &domain.Monitor{ID: 3}
	a, err := alerts.OpenOnDown(ctx, acked, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alerts.Acknowledge(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	restarted.OnHeartbeat(ctx, acked, &domain.Heartbeat{MonitorID: 3, Status: domain.StatusDown}, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("acknowledged alert re-sent by recovery: notifications=%d", notif.count())
	}
}

// A monitor that went DOWN inside a maintenance window has no open alert when
// the window ends. The next DOWN after DOWN with a resend interval opens the
// alert and sends it from the resend branch. That send is the alert's initial
// attempt, so the following check must not send it again as a lost initial.
func TestLegacyDispatcherDoesNotRepeatAResendOpenedAlert(t *testing.T) {
	ctx := context.Background()
	notif := &fakeNotifier{}
	alerts := NewAlertService(newFakeAlertRepo())
	monitor := &domain.Monitor{ID: 7, ResendInterval: 60}
	down := &domain.Heartbeat{MonitorID: 7, Status: domain.StatusDown}
	maint := &fakeMaintenance{active: true}

	d := newDispatcher(notif, maint)
	d.SetAlertLifecycle(alerts)
	d.OnHeartbeat(ctx, monitor, down, ptrStatus(domain.StatusUp))
	if notif.count() != 0 {
		t.Fatalf("DOWN inside maintenance notified: notifications=%d, want 0", notif.count())
	}
	maint.active = false
	d.OnHeartbeat(ctx, monitor, down, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("resend after maintenance: notifications=%d, want 1", notif.count())
	}
	d.OnHeartbeat(ctx, monitor, down, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("resend-opened alert sent again as lost initial: notifications=%d, want 1", notif.count())
	}
}
