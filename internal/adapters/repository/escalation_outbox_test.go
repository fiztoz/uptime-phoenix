package repository_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func dueEscalationPipeline(t *testing.T, engine string) (reviewPipeline, *services.EscalationService, *domain.EscalationPolicy, int64) {
	t.Helper()
	p := newReviewPipeline(t, engine, true)
	ctx := context.Background()
	nid := p.f.notification(t, p.f.user(t))
	policy := &domain.EscalationPolicy{UserID: p.f.user(t), Name: "durable ladder", Enabled: true,
		Steps: []domain.EscalationStep{
			{StepOrder: 1, NotificationIDs: []int64{nid}},
			{StepOrder: 2, WaitMinutes: 1, NotificationIDs: []int64{nid}},
		}}
	if err := p.repos.EscalationPolicyRepo.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.EscalationAssignmentRepo.AssignMonitor(ctx, p.monitor.ID, policy.ID); err != nil {
		t.Fatal(err)
	}
	applied, err := refreshService(t, p.f, p.f.db).Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svc := services.NewEscalationService(p.repos.EscalationPolicyRepo, p.repos.EscalationAssignmentRepo,
		p.repos.AlertEscalationRepo, p.repos.AlertRepo, p.repos.MonitorRepo, p.repos.MonitorGroupRepo, nil)
	svc.SetAppliedConfigReader(activationRepo(p.f, p.f.db).(ports.LocalAppliedConfigReader))
	svc.SetAssignmentRepository(p.f.assignments)
	svc.SetDeliveryOutbox(repository.NewEscalationOutboxStore(p.f.db, probe.LocalConfigEncoder{}))
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{Status: domain.StatusDown,
		Message: "durable failure", AssignmentGeneration: 1, ConfigRevision: applied.Revision}); err != nil {
		t.Fatal(err)
	}
	// No dispatcher StartForAlert is wired. Registration must be in the heartbeat transaction.
	assertTableCount(t, p.f, "alert_escalations", 1)
	return p, svc, policy, applied.Revision
}

func TestEscalationOutboxContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("RollbackAndClaimFence", func(t *testing.T) {
				p, _, policy, revision := dueEscalationPipeline(t, engine)
				ctx := context.Background()
				now := time.Now().UTC().Truncate(time.Microsecond)
				rows, err := p.repos.AlertEscalationRepo.ClaimDue(ctx, "atomic-step", now, now.Add(time.Minute))
				if err != nil || len(rows) != 1 {
					t.Fatalf("claim: %v %v", rows, err)
				}
				plan := domain.EscalationStepCommit{EscalationID: rows[0].ID, ClaimToken: "atomic-step", ExpectedStep: 1,
					ConfigRevision: revision, At: now, NotificationIDs: policy.Steps[0].NotificationIDs,
					NextStep: 2, NextRunAt: now.Add(time.Minute)}
				store := repository.NewEscalationOutboxStore(p.f.db, probe.LocalConfigEncoder{})
				trigger := "CREATE TRIGGER reject_escalation_advance BEFORE UPDATE ON alert_escalations BEGIN SELECT RAISE(ABORT, 'injected advance failure'); END"
				if engine == "mariadb" {
					trigger = "CREATE TRIGGER reject_escalation_advance BEFORE UPDATE ON alert_escalations FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected advance failure'"
				}
				if _, err := p.f.db.ExecContext(ctx, trigger); err != nil {
					t.Fatal(err)
				}
				ok, err := store.CommitEscalationStep(ctx, plan)
				if ok || err == nil {
					t.Fatalf("injected failure committed: %v %v", ok, err)
				}
				assertTableCount(t, p.f, "probe_delivery_intents", 1)
				if _, err := p.f.db.ExecContext(ctx, "DROP TRIGGER reject_escalation_advance"); err != nil {
					t.Fatal(err)
				}
				state, err := p.repos.AlertEscalationRepo.GetByAlertID(ctx, rows[0].AlertID)
				if err != nil || state.NextStep != 1 {
					t.Fatalf("rollback advanced ladder: %+v %v", state, err)
				}
				stale := plan
				stale.ClaimToken = "other-worker"
				if ok, err := store.CommitEscalationStep(ctx, stale); err != nil || ok {
					t.Fatalf("stale claim: %v %v", ok, err)
				}
				// Raw telemetry pruning must not remove durable escalation authority.
				if _, err := p.f.db.ExecContext(ctx, "DELETE FROM probe_observations"); err != nil {
					t.Fatal(err)
				}
				if ok, err := store.CommitEscalationStep(ctx, plan); err != nil || !ok {
					t.Fatalf("commit: %v %v", ok, err)
				}
				if ok, err := store.CommitEscalationStep(ctx, plan); err != nil || ok {
					t.Fatalf("duplicate receipt: %v %v", ok, err)
				}
				assertTableCount(t, p.f, "probe_delivery_intents", 2)
				if err := runEngineMigration(t, p.f.db, engine, "051_escalation_delivery_context", "down"); err == nil {
					t.Fatal("downgrade erased escalation identity")
				}
			})
			t.Run("ProviderRetrySurvivesRunnerRestart", func(t *testing.T) {
				p, svc, _, _ := dueEscalationPipeline(t, engine)
				ctx := context.Background()
				p.send(t)
				if steps, err := svc.RunDue(ctx); err != nil || steps != 1 {
					t.Fatalf("enqueue: %d %v", steps, err)
				}
				if len(p.sender.alerts) != 1 {
					t.Fatal("runner performed untracked provider I/O")
				}
				p.sender.err = context.DeadlineExceeded
				p.send(t)
				if _, err := p.f.db.ExecContext(ctx, "UPDATE probe_delivery_intents SET available_at = ? WHERE status = ?", time.Now().UTC().Add(-time.Second), domain.DeliveryStatusRetrying); err != nil {
					t.Fatal(err)
				}
				claimed, err := p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), time.Minute, 10)
				if err != nil || len(claimed) != 1 {
					t.Fatalf("durable retry: %v %v", claimed, err)
				}
				item := claimed[0]
				if item.EscalationStep != 1 || item.Attempt != 2 {
					t.Fatalf("lost retry identity: %+v", item)
				}
				// A new consumer with a cold activation reader can finish the attempt.
				restarted := services.NewDeliveryOutboxConsumer(p.outbox, p.repos.NotificationRepo, services.DefaultDeliveryConsumerConfig())
				restarted.SetActivationRepository(activationRepo(p.f, p.f.db))
				restarted.SetAssignmentRepository(p.f.assignments)
				restarted.SetIncidentRepository(p.f.incidents)
				restarted.SetAlertRepository(p.repos.AlertRepo)
				p.sender.err = nil
				restarted.RegisterSender(p.sender)
				if err := restarted.ProcessOne(ctx, item); err != nil {
					t.Fatal(err)
				}
				result, err := p.outbox.GetDeliveryIntent(ctx, domain.LocalProbeID, item.DeliveryID)
				if err != nil || result.Status != domain.DeliveryStatusSent {
					t.Fatalf("retry completion: %+v %v", result, err)
				}
				if !strings.Contains(p.sender.alerts[len(p.sender.alerts)-1].Message, "ESCALATION step 1") {
					t.Fatal("lost escalation rendering")
				}
			})
			t.Run("AckBetweenQueueAndSend", func(t *testing.T) {
				p, svc, _, _ := dueEscalationPipeline(t, engine)
				ctx := context.Background()
				p.send(t)
				if n, err := svc.RunDue(ctx); err != nil || n != 1 {
					t.Fatalf("enqueue: %d %v", n, err)
				}
				alert, err := p.repos.AlertRepo.GetOpenByMonitorID(ctx, p.monitor.ID)
				if err != nil {
					t.Fatal(err)
				}
				alerts := services.NewAlertService(p.repos.AlertRepo)
				alerts.SetEscalationCanceller(svc)
				if _, err := alerts.Acknowledge(ctx, alert.ID, nil); err != nil {
					t.Fatal(err)
				}
				p.send(t)
				if len(p.sender.alerts) != 1 {
					t.Fatal("acknowledged escalation reached provider")
				}
			})
			t.Run("ColdReaderRejectsUnappliedChannel", func(t *testing.T) {
				ctx := context.Background()
				p := newReviewPipeline(t, engine, true)
				links, err := p.repos.MonitorNotificationRepo.ListByMonitor(ctx, p.monitor.ID)
				if err != nil {
					t.Fatal(err)
				}
				channel, err := p.repos.NotificationRepo.GetByID(ctx, links[0].NotificationID)
				if err != nil {
					t.Fatal(err)
				}
				channel.Config = map[string]any{"url": "https://unapplied.example.test/hook"}
				if err := p.repos.NotificationRepo.Update(ctx, channel); err != nil {
					t.Fatal(err)
				}
				n := services.NewNotificationService(p.repos.NotificationRepo, p.repos.MonitorNotificationRepo)
				n.RegisterSender(p.sender)
				n.SetAppliedConfigReader(activationRepo(p.f, p.f.db).(ports.LocalAppliedConfigReader))
				err = n.DispatchToNotificationIDs(ctx, []int64{channel.ID}, domain.AlertContext{MonitorID: p.monitor.ID, Status: domain.StatusDown})
				if !errors.Is(err, ports.ErrConflict) || len(p.sender.alerts) != 0 {
					t.Fatalf("cold reader used mutable configuration: %v %+v", err, p.sender.configs)
				}
			})
		})
	}
}
