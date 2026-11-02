package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// A hub killed after the DOWN alert row commits and before the providers are
// called must still announce the outage after a restart, exactly once per
// successful attempt, on both engines.
func TestLegacyInitialDownSurvivesACrash(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			repo := alertScopeRepos(f).alerts
			mid := localMonitor(t, f)
			monitor := &domain.Monitor{ID: mid, Name: "crash target"}

			// Before the crash: the alert row committed, no provider call.
			lost := openScopedAlert(t, repo, mid, time.Now().UTC().Truncate(time.Second))
			if lost.InitialNotifiedAt != nil {
				t.Fatalf("new alert already marked notified: %v", lost.InitialNotifiedAt)
			}

			// After the restart.
			notif := &regionalAlertNotifier{}
			d := services.NewNotificationDispatcher(notif, &throttleMaintenance{})
			d.SetAlertLifecycle(services.NewAlertService(repo))
			down := &domain.Heartbeat{MonitorID: mid, Status: domain.StatusDown}
			prev := domain.StatusDown
			d.OnHeartbeat(ctx, monitor, down, &prev)
			if notif.notifications+len(notif.alerts) != 1 {
				t.Fatalf("initial DOWN lost to the crash: sends=%d, want 1", notif.notifications+len(notif.alerts))
			}
			stored, err := repo.GetByID(ctx, lost.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.InitialNotifiedAt == nil {
				t.Fatal("recovered send was not recorded")
			}
			d.OnHeartbeat(ctx, monitor, down, &prev)
			if notif.notifications+len(notif.alerts) != 1 {
				t.Fatalf("recovered alert sent again: sends=%d", notif.notifications+len(notif.alerts))
			}

			// A second mark keeps the first attempt time.
			first := *stored.InitialNotifiedAt
			if err := repo.(ports.AlertInitialNotificationRecorder).MarkInitialNotified(ctx, lost.ID, first.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			again, err := repo.GetByID(ctx, lost.ID)
			if err != nil {
				t.Fatal(err)
			}
			if again.InitialNotifiedAt == nil || !again.InitialNotifiedAt.Equal(first) {
				t.Fatalf("second mark moved the attempt time: %v, want %v", again.InitialNotifiedAt, first)
			}
		})
	}
}

// The resend branch can open the alert itself: a monitor that went DOWN inside
// a maintenance window has none when the window ends. Its send is the initial
// attempt and must be recorded, or the next check pages it again as lost.
func TestLegacyResendOpenedAlertIsMarkedInitialNotified(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			repo := alertScopeRepos(f).alerts
			mid := localMonitor(t, f)
			monitor := &domain.Monitor{ID: mid, Name: "maintenance target", ResendInterval: 60}

			notif := &regionalAlertNotifier{}
			maint := &throttleMaintenance{active: true}
			d := services.NewNotificationDispatcher(notif, maint)
			alerts := services.NewAlertService(repo)
			d.SetAlertLifecycle(alerts)
			down := &domain.Heartbeat{MonitorID: mid, Status: domain.StatusDown}
			sends := func() int { return notif.notifications + len(notif.alerts) }

			up := domain.StatusUp
			d.OnHeartbeat(ctx, monitor, down, &up)
			if sends() != 0 {
				t.Fatalf("DOWN inside maintenance sent: sends=%d, want 0", sends())
			}
			maint.active = false
			prev := domain.StatusDown
			d.OnHeartbeat(ctx, monitor, down, &prev)
			if sends() != 1 {
				t.Fatalf("resend after maintenance: sends=%d, want 1", sends())
			}
			open, err := repo.GetOpenByMonitorID(ctx, mid)
			if err != nil {
				t.Fatal(err)
			}
			if open == nil {
				t.Fatal("resend did not open an alert")
			}
			if open.InitialNotifiedAt == nil {
				t.Fatal("resend-opened alert was not marked initial notified")
			}
			d.OnHeartbeat(ctx, monitor, down, &prev)
			if sends() != 1 {
				t.Fatalf("resend-opened alert sent again as lost initial: sends=%d, want 1", sends())
			}
		})
	}
}
