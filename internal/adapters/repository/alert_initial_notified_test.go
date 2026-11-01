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
