package services

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// --- Test doubles --------------------------------------------------------

type notifyCall struct {
	monitorID int64
	status    domain.Status
	prev      domain.Status
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

func (f *fakeNotifier) Notify(_ context.Context, m *domain.Monitor, status, prev domain.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, notifyCall{monitorID: m.ID, status: status, prev: prev})
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeNotifier) last() notifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

type fakeMaintenance struct {
	active bool
	err    error
}

func (f *fakeMaintenance) IsActive(_ context.Context, _ int64) (bool, error) {
	return f.active, f.err
}

func ptrStatus(s domain.Status) *domain.Status { return &s }

func newDispatcher(n alertNotifier, m maintenanceChecker) *NotificationDispatcher {
	return NewNotificationDispatcher(n, m)
}

// --- Tests ---------------------------------------------------------------

func TestDispatcher_AlertsOnConfirmedDown(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{})

	monitor := &domain.Monitor{ID: 1, Name: "api"}
	hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}

	// prevStatus nil => first heartbeat, treated as a transition from UP.
	d.OnHeartbeat(context.Background(), monitor, hb, nil)

	if notif.count() != 1 {
		t.Fatalf("Notify called %d times; want 1", notif.count())
	}
	if got := notif.last().status; got != domain.StatusDown {
		t.Errorf("notified status = %v; want DOWN", got)
	}
}

func TestDispatcher_NoAlertOnPending(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{})

	monitor := &domain.Monitor{ID: 1}
	hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusPending}

	d.OnHeartbeat(context.Background(), monitor, hb, ptrStatus(domain.StatusUp))

	if notif.count() != 0 {
		t.Errorf("Notify called %d times; want 0 (PENDING must not alert)", notif.count())
	}
}

func TestDispatcher_AlertsOnRecovery(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{})

	monitor := &domain.Monitor{ID: 1}
	hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusUp}

	d.OnHeartbeat(context.Background(), monitor, hb, ptrStatus(domain.StatusDown))

	if notif.count() != 1 {
		t.Fatalf("Notify called %d times; want 1", notif.count())
	}
	if got := notif.last().status; got != domain.StatusUp {
		t.Errorf("recovery notified status = %v; want UP", got)
	}
}

func TestDispatcher_SuppressedDuringMaintenance(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{active: true})

	monitor := &domain.Monitor{ID: 1}
	hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}

	d.OnHeartbeat(context.Background(), monitor, hb, ptrStatus(domain.StatusUp))

	if notif.count() != 0 {
		t.Errorf("Notify called %d times during maintenance; want 0", notif.count())
	}
}

func TestDispatcher_NoAlertWhenStableUp(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{})

	monitor := &domain.Monitor{ID: 1}
	hb := &domain.Heartbeat{MonitorID: 1, Status: domain.StatusUp}

	d.OnHeartbeat(context.Background(), monitor, hb, ptrStatus(domain.StatusUp))

	if notif.count() != 0 {
		t.Errorf("Notify called %d times on stable UP; want 0", notif.count())
	}
}

func TestDispatcher_ResendThrottle(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{})

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	d.now = func() time.Time { return clock }

	monitor := &domain.Monitor{ID: 1, ResendInterval: 5} // re-alert every 5 minutes

	// Initial confirmed DOWN transition fires once.
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusUp))
	if notif.count() != 1 {
		t.Fatalf("after first DOWN: Notify count = %d; want 1", notif.count())
	}

	// Still down, only 2 minutes later — within the resend window, no re-alert.
	clock = base.Add(2 * time.Minute)
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusDown))
	if notif.count() != 1 {
		t.Fatalf("within resend window: Notify count = %d; want 1", notif.count())
	}

	// Still down, now 6 minutes after the last alert — resend fires.
	clock = base.Add(6 * time.Minute)
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusDown))
	if notif.count() != 2 {
		t.Fatalf("after resend window: Notify count = %d; want 2", notif.count())
	}
}

func TestDispatcher_NoResendWhenIntervalZero(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{})

	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }

	monitor := &domain.Monitor{ID: 1, ResendInterval: 0} // resend disabled

	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusUp))
	clock = clock.Add(time.Hour)
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusDown))

	if notif.count() != 1 {
		t.Errorf("Notify count = %d; want 1 (no resend when interval is 0)", notif.count())
	}
}

func TestDispatcher_RecoveryClearsThrottle(t *testing.T) {
	notif := &fakeNotifier{}
	d := newDispatcher(notif, &fakeMaintenance{})

	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }

	monitor := &domain.Monitor{ID: 1, ResendInterval: 5}

	// Down -> recovery -> down again should produce 3 alerts (down, up, down),
	// with the second down firing immediately because recovery cleared the throttle.
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusUp))
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusUp}, ptrStatus(domain.StatusDown))
	d.OnHeartbeat(context.Background(), monitor, &domain.Heartbeat{MonitorID: 1, Status: domain.StatusDown}, ptrStatus(domain.StatusUp))

	if notif.count() != 3 {
		t.Errorf("Notify count = %d; want 3 (down, recovery, down)", notif.count())
	}
}
