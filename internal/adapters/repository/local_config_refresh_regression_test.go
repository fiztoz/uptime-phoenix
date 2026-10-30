package repository_test

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestLocalConfigRefreshWithPushAndSMTP(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			p := newReviewPipeline(t, engine, true)
			push := &domain.Monitor{UserID: p.monitor.UserID, Name: "inbound", Type: "push", Active: true, Interval: 60, RetryInterval: 60, Timeout: 30}
			svc := services.NewMonitorService(p.repos.MonitorRepo, eventbus.NewMemoryBus())
			if err := svc.Create(t.Context(), push); err != nil {
				t.Fatal(err)
			}
			if _, err := p.f.assignments.InitializeLocal(t.Context(), push.ID); err != nil {
				t.Fatal(err)
			}
			if push.PushToken == "" || push.Config["push_token"] != push.PushToken {
				t.Fatal("monitor service did not create both inbound credential representations")
			}
			notif := &domain.Notification{UserID: p.monitor.UserID, Name: "mail", Type: "smtp", Active: true,
				Config: map[string]any{"host": "mail.example.test", "port": 587, "from": "ops@example.test", "to": []any{"one@example.test", "two@example.test"}}}
			notificationSvc := services.NewNotificationService(p.repos.NotificationRepo, p.repos.MonitorNotificationRepo)
			sender, ok := notifier.Get("smtp")
			if !ok {
				t.Fatal("SMTP provider is not registered")
			}
			notificationSvc.RegisterSender(sender)
			if err := notificationSvc.Create(t.Context(), notif); err != nil {
				t.Fatal(err)
			}
			if err := p.repos.MonitorNotificationRepo.Attach(t.Context(), p.monitor.ID, notif.ID, true); err != nil {
				t.Fatal(err)
			}
			active, err := refreshService(t, p.f, p.f.db).Refresh(t.Context())
			if err != nil {
				t.Fatalf("persisted push/SMTP config blocked refresh: %v", err)
			}
			if _, err := p.active.(ports.LocalAppliedConfigReader).ReadAppliedLocal(t.Context()); err != nil {
				t.Fatalf("capture settings after refresh: %v", err)
			}
			found, err := svc.GetByPushToken(t.Context(), push.PushToken)
			if err != nil || found.ID != push.ID {
				t.Fatal("snapshot encoding damaged inbound push lookup")
			}
			if err := p.heartbeats.Record(t.Context(), p.monitor, ports.CheckResult{Status: domain.StatusUp, AssignmentGeneration: 1, ConfigRevision: active.Revision}); err != nil {
				t.Fatalf("unrelated local heartbeat blocked: %v", err)
			}
			beat, err := p.repos.HeartbeatRepo.GetLatest(t.Context(), p.monitor.ID)
			if err != nil || beat.Status != domain.StatusUp {
				t.Fatal("unrelated heartbeat did not persist")
			}
		})
	}
}
