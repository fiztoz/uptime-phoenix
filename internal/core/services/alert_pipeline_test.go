package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestHeartbeatToDispatcher_AlertLifecycleEffects drives the production service
// chain from recorded check results through retry confirmation, notification
// dispatch, persisted acknowledgement suppression, and recovery resolution.
// Status-code-only tests cannot prove any of these effects happened.
func TestHeartbeatToDispatcher_AlertLifecycleEffects(t *testing.T) {
	ctx := context.Background()
	heartbeats := newFakeHeartbeatRepo()
	alerts := newFakeAlertRepo()
	alertSvc := NewAlertService(alerts)
	notifier := &fakeNotifier{}
	dispatcher := NewNotificationDispatcher(notifier, &fakeMaintenance{})
	dispatcher.SetAlertLifecycle(alertSvc)

	base := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	clock := base
	dispatcher.now = func() time.Time { return clock }

	heartbeatSvc := NewHeartbeatService(heartbeats, newFakeBus())
	heartbeatSvc.SetDispatcher(dispatcher)
	monitor := &domain.Monitor{
		ID:             42,
		Name:           "checkout-api",
		Type:           "http",
		MaxRetries:     1,
		ResendInterval: 1,
	}

	// The first failure is still inside the retry window: PENDING must neither
	// notify nor create an alert row.
	if err := heartbeatSvc.Record(ctx, monitor, ports.CheckResult{
		Status:  domain.StatusDown,
		Message: "connection refused",
	}); err != nil {
		t.Fatalf("record first failure: %v", err)
	}
	if notifier.count() != 0 {
		t.Fatalf("first retry notified %d times; want 0", notifier.count())
	}
	if _, err := alerts.GetOpenByMonitorID(ctx, monitor.ID); err == nil {
		t.Fatal("first retry created an alert before DOWN was confirmed")
	}

	// The second consecutive failure confirms DOWN. It must persist one firing
	// alert and send step zero exactly once.
	if err := heartbeatSvc.Record(ctx, monitor, ports.CheckResult{
		Status:  domain.StatusDown,
		Message: "connection refused",
	}); err != nil {
		t.Fatalf("record confirmed failure: %v", err)
	}
	if notifier.count() != 1 {
		t.Fatalf("confirmed DOWN notified %d times; want exactly 1", notifier.count())
	}
	if call := notifier.last(); call.status != domain.StatusDown || call.monitorID != monitor.ID {
		t.Fatalf("confirmed DOWN notification = %+v; want monitor %d DOWN", call, monitor.ID)
	}
	open, getOpenErr := alerts.GetOpenByMonitorID(ctx, monitor.ID)
	if getOpenErr != nil {
		t.Fatalf("confirmed DOWN did not persist an open alert: %v", getOpenErr)
	}
	if open.Status != domain.AlertStatusFiring ||
		open.OpenMonitorID == nil ||
		*open.OpenMonitorID != monitor.ID ||
		open.AckToken == "" {
		t.Fatalf("persisted firing alert is incomplete: %+v", open)
	}

	userID := int64(7)
	acked, ackErr := alertSvc.Acknowledge(ctx, open.ID, &userID)
	if ackErr != nil {
		t.Fatalf("acknowledge open alert: %v", ackErr)
	}
	if acked.Status != domain.AlertStatusAcked ||
		acked.OpenMonitorID == nil ||
		*acked.OpenMonitorID != monitor.ID {
		t.Fatalf("acknowledgement closed or corrupted the outage: %+v", acked)
	}

	// Past the resend interval, the heartbeat still reaches the dispatcher but
	// the persisted acked status must suppress the notification effect.
	clock = base.Add(2 * time.Minute)
	if err := heartbeatSvc.Record(ctx, monitor, ports.CheckResult{
		Status:  domain.StatusDown,
		Message: "still refusing connections",
	}); err != nil {
		t.Fatalf("record still-down heartbeat: %v", err)
	}
	if notifier.count() != 1 {
		t.Fatalf("acked outage resent %d total notifications; want 1", notifier.count())
	}

	// Recovery resolves the same row, clears open_monitor_id, and sends one UP
	// notification. This is the end-to-end lifecycle effect F2.3 relies on.
	clock = base.Add(3 * time.Minute)
	if err := heartbeatSvc.Record(ctx, monitor, ports.CheckResult{
		Status:  domain.StatusUp,
		Message: "HTTP 200",
	}); err != nil {
		t.Fatalf("record recovery: %v", err)
	}
	if notifier.count() != 2 {
		t.Fatalf("recovery produced %d total notifications; want DOWN + UP", notifier.count())
	}
	if call := notifier.last(); call.status != domain.StatusUp || call.prev != domain.StatusDown {
		t.Fatalf("recovery notification = %+v; want DOWN -> UP", call)
	}
	resolved, getResolvedErr := alerts.GetByID(ctx, open.ID)
	if getResolvedErr != nil {
		t.Fatalf("read resolved alert: %v", getResolvedErr)
	}
	if resolved.Status != domain.AlertStatusResolved ||
		resolved.ResolvedAt == nil ||
		resolved.OpenMonitorID != nil {
		t.Fatalf("recovery did not resolve the persisted alert: %+v", resolved)
	}
}
