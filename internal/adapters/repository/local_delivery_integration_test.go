package repository_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	checker "github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	notifier "github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	scheduler "github.com/fiztoz/uptime-phoenix/internal/adapters/scheduler"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type reviewSender struct {
	alerts  []domain.AlertContext
	err     error
	configs []map[string]any
}

func (s *reviewSender) Type() string                  { return "webhook" }
func (s *reviewSender) Validate(map[string]any) error { return nil }
func (s *reviewSender) Send(_ context.Context, config map[string]any, a domain.AlertContext) error {
	s.alerts = append(s.alerts, a)
	s.configs = append(s.configs, config)
	return s.err
}

type reviewMaintenance struct{}

func (reviewMaintenance) IsActive(context.Context, int64) (bool, error) { return false, nil }

type reviewPipeline struct {
	f          probeRegistryFixture
	repos      pipelineRepositories
	monitor    *domain.Monitor
	heartbeats *services.HeartbeatService
	consumer   *services.DeliveryOutboxConsumer
	outbox     ports.DeliveryOutboxRepository
	sender     *reviewSender
	active     ports.ProbeConfigActivationRepository
}

type pipelineRepositories struct {
	MonitorRepo                  ports.MonitorRepository
	HeartbeatRepo                ports.HeartbeatRepository
	NotificationRepo             ports.NotificationRepository
	MonitorNotificationRepo      ports.MonitorNotificationRepository
	AlertRepo                    ports.AlertRepository
	MaintenanceRepo              ports.MaintenanceRepository
	MaintenanceWindowMonitorRepo ports.MaintenanceWindowMonitorRepository
	EscalationPolicyRepo         ports.EscalationPolicyRepository
	EscalationAssignmentRepo     ports.EscalationAssignmentRepository
	AlertEscalationRepo          ports.AlertEscalationRepository
	MonitorGroupRepo             ports.MonitorGroupRepository
}

func refreshService(t *testing.T, f probeRegistryFixture, db *bun.DB) *services.LocalProbeConfigRefreshService {
	t.Helper()
	_, prepared := sourceConfigBuilder(t, f, db)
	validator := probe.NewLocalConfigValidator(checker.Get, notifier.Get)
	valSvc := services.NewLocalProbeConfigValidationService(prepared, validator)
	actRepo := activationRepo(f, db)
	actSvc := services.NewLocalProbeConfigActivationService(valSvc, actRepo)
	instRepo := installationRepo(f, db)
	sourceStore := repository.NewLocalProbeConfigSourceStore(db)
	return services.NewLocalProbeConfigRefreshService(
		sourceStore,
		probe.LocalConfigEncoder{},
		prepared,
		actSvc,
		actRepo,
		instRepo,
	)
}

func newReviewPipeline(t *testing.T, engine string, activate bool) reviewPipeline {
	t.Helper()
	ctx := context.Background()
	f := newProbeRegistryFixture(t, engine)
	var repos pipelineRepositories
	if engine == "sqlite" {
		r := sqlite.NewRepository(f.db)
		repos = pipelineRepositories{
			MonitorRepo:                  r.MonitorRepo,
			HeartbeatRepo:                r.HeartbeatRepo,
			NotificationRepo:             r.NotificationRepo,
			MonitorNotificationRepo:      r.MonitorNotificationRepo,
			AlertRepo:                    r.AlertRepo,
			MaintenanceRepo:              r.MaintenanceRepo,
			MaintenanceWindowMonitorRepo: r.MaintenanceWindowMonitorRepo,
			EscalationPolicyRepo:         r.EscalationPolicyRepo,
			EscalationAssignmentRepo:     r.EscalationAssignmentRepo,
			AlertEscalationRepo:          r.AlertEscalationRepo,
			MonitorGroupRepo:             r.MonitorGroupRepo,
		}
	} else {
		r := mariadb.NewRepository(f.db)
		repos = pipelineRepositories{
			MonitorRepo:                  r.MonitorRepo,
			HeartbeatRepo:                r.HeartbeatRepo,
			NotificationRepo:             r.NotificationRepo,
			MonitorNotificationRepo:      r.MonitorNotificationRepo,
			AlertRepo:                    r.AlertRepo,
			MaintenanceRepo:              r.MaintenanceRepo,
			MaintenanceWindowMonitorRepo: r.MaintenanceWindowMonitorRepo,
			EscalationPolicyRepo:         r.EscalationPolicyRepo,
			EscalationAssignmentRepo:     r.EscalationAssignmentRepo,
			AlertEscalationRepo:          r.AlertEscalationRepo,
			MonitorGroupRepo:             r.MonitorGroupRepo,
		}
	}
	uid := f.user(t)
	id := localMonitor(t, f)
	if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET user_id = ? WHERE id = ?", uid, id); err != nil {
		t.Fatal(err)
	}
	nid := f.notification(t, uid)
	if err := repos.MonitorNotificationRepo.Attach(ctx, id, nid, true); err != nil {
		t.Fatal(err)
	}
	monitor, err := repos.MonitorRepo.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	monitor.MaxRetries = 0
	monitor.ResendInterval = 1
	active := activationRepo(f, f.db)
	if activate {
		hub := "11111111-2222-4333-8444-555555555555"
		seedInstallation(t, f)
		builder, _ := sourceConfigBuilder(t, f, f.db)
		now := time.Now().UTC()
		meta, err := builder.Prepare(ctx, hub, 0, now, now)
		if err != nil {
			t.Fatal(err)
		}
		_, err = active.ActivateLocal(ctx, ports.LocalActivationParams{Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AppliedAt: now, AssignmentCount: -1})
		if err != nil {
			t.Fatal(err)
		}
	}
	sender := &reviewSender{}
	notifier := services.NewNotificationService(repos.NotificationRepo, repos.MonitorNotificationRepo)
	notifier.RegisterSender(sender)
	if activate {
		if reader, ok := active.(ports.LocalAppliedConfigReader); ok {
			notifier.SetAppliedConfigReader(reader)
		}
	}
	dispatcher := services.NewNotificationDispatcher(notifier, reviewMaintenance{})
	dispatcher.SetAssignmentRepository(f.assignments)
	dispatcher.SetAlertLifecycle(services.NewAlertService(repos.AlertRepo))
	dispatcher.SetThrottleRepository(repository.NewNotificationThrottleStore(f.db))
	dispatcher.SetOutboxDelivery(activate)
	heartbeats := services.NewHeartbeatService(repos.HeartbeatRepo, eventbus.NewMemoryBus())
	heartbeats.SetRegionalRecorder(f.assignments, f.localHeartbeat)
	if activate {
		heartbeats.SetActivationRepo(active)
		heartbeats.SetMonitorNotificationRepo(repos.MonitorNotificationRepo)
	}
	heartbeats.SetDispatcher(dispatcher)
	outbox := f.commits.(ports.DeliveryOutboxRepository)
	consumer := services.NewDeliveryOutboxConsumer(outbox, repos.NotificationRepo, services.DefaultDeliveryConsumerConfig())
	consumer.SetAssignmentRepository(f.assignments)
	consumer.SetActivationRepository(active)
	consumer.SetIncidentRepository(f.incidents)
	consumer.SetMonitorNotificationRepository(repos.MonitorNotificationRepo)
	consumer.SetMonitorRepository(repos.MonitorRepo)
	consumer.SetAlertRepository(repos.AlertRepo)
	consumer.SetMaintenanceChecker(reviewMaintenance{})
	consumer.RegisterSender(sender)
	return reviewPipeline{f, repos, monitor, heartbeats, consumer, outbox, sender, active}
}

func (p reviewPipeline) record(t *testing.T, status domain.Status) {
	t.Helper()
	if err := p.heartbeats.Record(context.Background(), p.monitor, ports.CheckResult{Status: status, Message: "review sample", AssignmentGeneration: 1, ConfigRevision: 1}); err != nil {
		t.Fatalf("record status %v: %v", status, err)
	}
}
func (p reviewPipeline) send(t *testing.T) {
	t.Helper()
	if _, err := p.consumer.ProcessBatch(context.Background(), domain.LocalProbeID, time.Now().UTC(), 50); err != nil {
		t.Fatal(err)
	}
}

func testLocalDeliveryDefaultAvailabilitySend(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, false)
	p.record(t, domain.StatusDown)
	p.send(t)
	if len(p.sender.alerts) != 1 {
		t.Fatalf("default local DOWN sent %d notifications, want 1", len(p.sender.alerts))
	}
}

func testLocalDeliveryResendAfterInterval(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)
	p.send(t)
	if len(p.sender.alerts) != 1 {
		t.Fatalf("control initial DOWN: sends=%d", len(p.sender.alerts))
	}
	if _, err := p.f.db.ExecContext(context.Background(), "UPDATE notification_throttles SET last_attempt_at = ?", time.Now().UTC().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p.record(t, domain.StatusDown)
	p.send(t)
	if len(p.sender.alerts) != 2 {
		t.Fatalf("after due resend: total sends=%d, want 2", len(p.sender.alerts))
	}
}

func testLocalDeliveryAckBetweenClaimAndFirstSend(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)
	ctx := context.Background()
	claimed, err := p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), 5*time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	alert, err := p.repos.AlertRepo.GetOpenByMonitorID(ctx, p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.NewAlertService(p.repos.AlertRepo).Acknowledge(ctx, alert.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.consumer.ProcessOne(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if len(p.sender.alerts) != 0 {
		t.Fatalf("acknowledged before provider I/O, but sent %d notifications", len(p.sender.alerts))
	}
}

func testLocalDeliveryDownAfterMaintenance(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)
	p.record(t, domain.StatusMaintenance)
	p.record(t, domain.StatusDown)
}

func testLocalDeliveryRecoveryAfterAck(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)
	p.send(t)
	ctx := context.Background()
	alert, err := p.repos.AlertRepo.GetOpenByMonitorID(ctx, p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.NewAlertService(p.repos.AlertRepo).Acknowledge(ctx, alert.ID, nil); err != nil {
		t.Fatal(err)
	}
	p.record(t, domain.StatusUp)
	p.send(t)
	if len(p.sender.alerts) != 2 || p.sender.alerts[1].Status != domain.StatusUp {
		t.Fatalf("acknowledgement swallowed recovery: %+v", p.sender.alerts)
	}
}

func testLocalDeliveryRejectForeignSnapshotKey(t *testing.T, engine string) {
	f := newProbeRegistryFixture(t, engine)
	ctx := context.Background()
	doc, target := configDocument(t, domain.LocalProbeID, 1, "fixture-secret")
	keyA, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{99}, 32))
	if err != nil {
		t.Fatal(err)
	}
	instSvc := services.NewProbeInstallationService(installationRepo(f, f.db))
	if _, err := instSvc.InitializeOrVerify(ctx, keyA, target.HubID); err != nil {
		t.Fatal(err)
	}
	// This helper uses a different key (byte 37), against the same installed hub.
	_, err = protectedConfigService(t, configRepo(f, f.db)).Prepare(ctx, target, doc, 0)
	if err == nil {
		t.Fatal("snapshot writer accepted a different key after installation key was bound")
	}
}

func testLocalDeliveryExisting045Upgrade(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	ctx := context.Background()
	p.record(t, domain.StatusDown)
	claimed, err := p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("pre-upgrade lease: %v %v", claimed, err)
	}
	// Revert additive migrations while preserving an actual pending receipt.
	for _, name := range []string{"059_probe_watchdog_source", "051_escalation_delivery_context", "050_delivery_cancellation"} {
		if err := runEngineMigration(t, p.f.db, engine, name, "down"); err != nil {
			t.Fatal(err)
		}
		if _, err := p.f.db.ExecContext(ctx, "DELETE FROM _migrations WHERE filename = ?", name+".up.sql"); err != nil {
			t.Fatal(err)
		}
	}

	if err := repository.RunMigrations(p.f.db.DB, engine); err != nil {
		t.Fatal(err)
	}
	after, err := p.outbox.GetDeliveryIntent(ctx, domain.LocalProbeID, claimed[0].DeliveryID)
	if err != nil || after.LeaseToken != claimed[0].LeaseToken || after.Attempt != claimed[0].Attempt || after.SourceAlertID != claimed[0].SourceAlertID {
		t.Fatalf("upgrade lost leased work: %+v %v", after, err)
	}
	// Returning the lease to a retry also checks preservation of its source parent.
	if err := p.outbox.FinishDelivery(ctx, domain.DeliveryClaim{DeliveryID: after.DeliveryID, ProbeID: after.ProbeID, Attempt: after.Attempt, LeaseToken: after.LeaseToken}, domain.DeliveryResult{Status: domain.DeliveryStatusRetrying, ErrorCode: domain.ErrCodeNetworkTimeout, At: time.Now().UTC(), RetryAt: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	p.record(t, domain.StatusUp)
	// Cancellation before a first claim is new in 050 and cannot be downgraded.
	p.record(t, domain.StatusDown)
	p.record(t, domain.StatusUp)
	if err := runEngineMigration(t, p.f.db, engine, "059_probe_watchdog_source", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, p.f.db, engine, "051_escalation_delivery_context", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, p.f.db, engine, "050_delivery_cancellation", "down"); err == nil {
		t.Fatal("downgrade lost attempt-zero cancellation")
	}
	if err := p.repos.MonitorRepo.Delete(ctx, p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, p.f.db, engine, "050_delivery_cancellation", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, p.f.db, engine, "050_delivery_cancellation", "up"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, p.f.db, engine, "051_escalation_delivery_context", "up"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, p.f.db, engine, "059_probe_watchdog_source", "up"); err != nil {
		t.Fatal(err)
	}
}

func testLocalDeliveryDelayedSummaryKeepsDurableRetry(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)
	ctx := context.Background()
	claimed, err := p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), 5*time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	p.record(t, domain.StatusUp)
	p.sender.err = context.DeadlineExceeded
	if err := p.consumer.ProcessOne(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	p.send(t)
	if len(p.sender.alerts) != 1 || p.sender.alerts[0].Status != domain.StatusUp || p.sender.alerts[0].EventKind != domain.DeliveryEventIncidentSummary || strings.Contains(p.sender.alerts[0].Message, "unknown") {
		t.Fatalf("summary did not report the actual recovery: %+v", p.sender.alerts)
	}
	count, err := p.f.db.NewSelect().TableExpr("probe_delivery_intents").Where("event_kind = ? AND status IN (?, ?)", domain.DeliveryEventIncidentSummary, domain.DeliveryStatusPending, domain.DeliveryStatusRetrying).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("summary provider timeout left %d durable summary retries, want 1; sent context=%+v", count, p.sender.alerts)
	}
}

func testLocalDeliveryChannelEditRequiresNewAppliedVersion(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)
	ctx := context.Background()
	links, err := p.repos.MonitorNotificationRepo.ListByMonitor(ctx, p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	notif, err := p.repos.NotificationRepo.GetByID(ctx, links[0].NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	notif.Config = map[string]any{"url": "https://new-channel.invalid/hook"}
	if err := p.repos.NotificationRepo.Update(ctx, notif); err != nil {
		t.Fatal(err)
	}
	p.send(t)
	if len(p.sender.alerts) != 0 {
		t.Fatalf("queued version 1 sent using newly edited, unapplied channel settings: %+v", p.sender.configs)
	}
}

func testLocalDeliveryNewConfigDuringOutage(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	ctx := context.Background()
	p.record(t, domain.StatusDown)
	p.send(t)
	first, err := p.repos.AlertRepo.GetOpenByMonitorID(ctx, p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstIncident, err := p.f.incidents.GetIncident(ctx, first.SourceAlertID)
	if err != nil {
		t.Fatal(err)
	}
	builder, _ := sourceConfigBuilder(t, p.f, p.f.db)
	now := time.Now().UTC()
	meta, err := builder.Prepare(ctx, "11111111-2222-4333-8444-555555555555", 1, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = activationRepo(p.f, p.f.db).ActivateLocal(ctx, ports.LocalActivationParams{Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, ExpectedActiveRevision: 1, AppliedAt: now, AssignmentCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.db.ExecContext(ctx, "UPDATE notification_throttles SET last_attempt_at = ?", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
		if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{Status: status, Message: "new config", AssignmentGeneration: 1, ConfigRevision: meta.Revision}); err != nil {
			t.Fatal(err)
		}
		p.send(t)
	}
	resolved, err := p.repos.AlertRepo.GetByID(ctx, first.ID)
	if err != nil || resolved.SourceAlertID != first.SourceAlertID || resolved.Status != domain.AlertStatusResolved {
		t.Fatalf("outage identity changed: %+v %v", resolved, err)
	}
	if len(p.sender.alerts) != 3 || p.sender.alerts[2].Status != domain.StatusUp || !p.sender.alerts[2].StartedAt.Equal(firstIncident.StartedAt) {
		t.Fatalf("config refresh lost outage identity or delivery: %+v", p.sender.alerts)
	}
}

func testLocalDeliveryFirstActivationAndRefresh(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, false)
	seedInstallation(t, p.f)
	refreshSvc := refreshService(t, p.f, p.f.db)

	// 1. First activation creates revision 1.
	active, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("first activation failed: %v", err)
	}
	if active.Revision != 1 {
		t.Fatalf("active revision = %d, want 1", active.Revision)
	}

	// 2. Refresh when unchanged returns revision 1 without new preparation.
	active2, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("second refresh failed: %v", err)
	}
	if active2.Revision != 1 {
		t.Fatalf("active2 revision = %d, want 1", active2.Revision)
	}

	// 3. Edit source (monitor name).
	p.monitor.Name = "Refreshed Monitor"
	if err := p.repos.MonitorRepo.Update(ctx, p.monitor); err != nil {
		t.Fatal(err)
	}

	// 4. Calling refresh detects change, prepares revision 2 and activates it.
	active3, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh on source edit failed: %v", err)
	}
	if active3.Revision != 2 {
		t.Fatalf("active3 revision = %d, want 2", active3.Revision)
	}

	// 5. Verify activation repo has revision 2 as active.
	current, err := activationRepo(p.f, p.f.db).GetActive(ctx, domain.LocalProbeID)
	if err != nil || current.Revision != 2 {
		t.Fatalf("stored active revision = %v, err = %v, want 2", current, err)
	}
}

func testLocalDeliveryInheritedAndDisabledChannels(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, true)

	// Channel 1 is direct active link.
	links, err := p.repos.MonitorNotificationRepo.ListByMonitor(ctx, p.monitor.ID)
	if err != nil || len(links) == 0 {
		t.Fatalf("expected channel 1 attached: %v", err)
	}
	notif1, err := p.repos.NotificationRepo.GetByID(ctx, links[0].NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	notif1.Config = map[string]any{"url": "https://channel1.example.com"}
	if err := p.repos.NotificationRepo.Update(ctx, notif1); err != nil {
		t.Fatal(err)
	}

	// Create Channel 2 that is disabled.
	nid2 := p.f.notification(t, p.f.user(t))
	notif2, err := p.repos.NotificationRepo.GetByID(ctx, nid2)
	if err != nil {
		t.Fatal(err)
	}
	notif2.Active = false
	notif2.Config = map[string]any{"url": "https://channel2.example.com"}
	if err := p.repos.NotificationRepo.Update(ctx, notif2); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.MonitorNotificationRepo.Attach(ctx, p.monitor.ID, nid2, true); err != nil {
		t.Fatal(err)
	}

	// Create Channel 3 for inherited escalation from group.
	nid3 := p.f.notification(t, p.f.user(t))
	notif3, err := p.repos.NotificationRepo.GetByID(ctx, nid3)
	if err != nil {
		t.Fatal(err)
	}
	notif3.Config = map[string]any{"url": "https://channel3-inherited.example.com"}
	if err := p.repos.NotificationRepo.Update(ctx, notif3); err != nil {
		t.Fatal(err)
	}

	// Create parent group and attach monitor to it.
	grp := &domain.MonitorGroup{UserID: p.f.user(t), Name: "Parent Group"}
	if err := p.repos.MonitorGroupRepo.Create(ctx, grp); err != nil {
		t.Fatal(err)
	}
	p.monitor.GroupID = &grp.ID
	if err := p.repos.MonitorRepo.Update(ctx, p.monitor); err != nil {
		t.Fatal(err)
	}

	// Create escalation policy with Step 1 paging Channel 3, assigned to group.
	policy := &domain.EscalationPolicy{
		UserID:  p.f.user(t),
		Name:    "Group Escalation",
		Enabled: true,
		Steps: []domain.EscalationStep{
			{
				StepOrder:       1,
				WaitMinutes:     0,
				NotificationIDs: []int64{nid3},
			},
		},
	}
	if err := p.repos.EscalationPolicyRepo.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.EscalationAssignmentRepo.AssignGroup(ctx, grp.ID, policy.ID); err != nil {
		t.Fatal(err)
	}

	// Activate config with channels and group-inherited policy.
	refreshSvc := refreshService(t, p.f, p.f.db)
	activeCfg, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Wire escalation service to test inherited channel execution.
	notifier := services.NewNotificationService(p.repos.NotificationRepo, p.repos.MonitorNotificationRepo)
	notifier.RegisterSender(p.sender)
	escalationSvc := services.NewEscalationService(
		p.repos.EscalationPolicyRepo,
		p.repos.EscalationAssignmentRepo,
		p.repos.AlertEscalationRepo,
		p.repos.AlertRepo,
		p.repos.MonitorRepo,
		p.repos.MonitorGroupRepo,
		notifier,
	)
	escalationSvc.SetAssignmentRepository(p.f.assignments)
	escalationSvc.SetAppliedConfigReader(p.active.(ports.LocalAppliedConfigReader))
	escalationSvc.SetDeliveryOutbox(repository.NewEscalationOutboxStore(p.f.db, probe.LocalConfigEncoder{}))

	dispatcher := services.NewNotificationDispatcher(notifier, reviewMaintenance{})
	dispatcher.SetAssignmentRepository(p.f.assignments)
	alertSvc := services.NewAlertService(p.repos.AlertRepo)
	alertSvc.SetEscalationCanceller(escalationSvc)
	dispatcher.SetAlertLifecycle(alertSvc)
	dispatcher.SetThrottleRepository(repository.NewNotificationThrottleStore(p.f.db))
	dispatcher.SetOutboxDelivery(true)
	dispatcher.SetEscalationStarter(escalationSvc)
	p.heartbeats.SetDispatcher(dispatcher)

	// Configure pipeline with active activation repo and outbox delivery.
	actRepo := activationRepo(p.f, p.f.db)
	p.heartbeats.SetActivationRepo(actRepo)
	p.heartbeats.SetMonitorNotificationRepo(p.repos.MonitorNotificationRepo)
	p.consumer.SetActivationRepository(actRepo)

	// Record DOWN with active revision.
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "failure",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfg.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	// Send step 0 notifications via consumer.
	p.send(t)

	// Verify only the direct active channel received Step 0. Disabled channel 2 received nothing.
	if len(p.sender.alerts) != 1 {
		t.Fatalf("expected 1 notification sent for active channel, got %d: %+v", len(p.sender.alerts), p.sender.alerts)
	}
	if len(p.sender.configs) != 1 || p.sender.configs[0]["url"] != "https://channel1.example.com" {
		t.Fatalf("unexpected configs sent: %+v", p.sender.configs)
	}

	// Now run due escalation step 1 -> executes inherited Channel 3!
	sent, err := escalationSvc.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("expected 1 escalation step sent for inherited channel, got %d", sent)
	}
	p.send(t)
	if len(p.sender.alerts) != 2 {
		t.Fatalf("expected total 2 alerts (step 0 direct + step 1 inherited), got %d", len(p.sender.alerts))
	}
	if p.sender.configs[1]["url"] != "https://channel3-inherited.example.com" {
		t.Fatalf("inherited channel config mismatch: %+v", p.sender.configs[1])
	}
}

func testLocalDeliveryMaintenanceParity(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, true)
	p.consumer.SetCronEvaluator(scheduler.NewCronEvaluator())
	now := time.Now().UTC()

	// 1. Create active single maintenance window covering this monitor.
	mw := &domain.MaintenanceWindow{
		UserID:    p.f.user(t),
		Title:     "Emergency Maintenance",
		Strategy:  "single",
		Active:    true,
		StartDate: now.Add(-10 * time.Minute),
		EndDate:   now.Add(10 * time.Minute),
		Timezone:  "UTC",
	}
	if err := p.repos.MaintenanceRepo.Create(ctx, mw); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.MaintenanceWindowMonitorRepo.Assign(ctx, mw.ID, p.monitor.ID); err != nil {
		t.Fatal(err)
	}

	// Refresh config so maintenance is in applied configuration.
	refreshSvc := refreshService(t, p.f, p.f.db)
	activeCfg, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Record DOWN during maintenance window.
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "down during maint",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfg.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	// Assert no deliveries queued and no alerts sent.
	p.send(t)
	if len(p.sender.alerts) != 0 {
		t.Fatalf("maintenance window should suppress all alerts, but sent %d", len(p.sender.alerts))
	}

	// 2. Test cron maintenance window parity.
	mw.Active = false
	if err := p.repos.MaintenanceRepo.Update(ctx, mw); err != nil {
		t.Fatal(err)
	}
	mwCron := &domain.MaintenanceWindow{
		UserID:   p.f.user(t),
		Title:    "Nightly Cron Maintenance",
		Strategy: "cron",
		CronExpr: "* * * * *",
		Duration: 60,
		Active:   true,
		Timezone: "UTC",
	}
	if err := p.repos.MaintenanceRepo.Create(ctx, mwCron); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.MaintenanceWindowMonitorRepo.Assign(ctx, mwCron.ID, p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	activeCfgCron, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Record DOWN during cron maintenance window -> suppressed.
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "down during cron maint",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfgCron.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	p.send(t)
	if len(p.sender.alerts) != 0 {
		t.Fatalf("cron maintenance window should suppress alerts, but sent %d", len(p.sender.alerts))
	}

	// 3. Deactivate cron window, refresh config, and queue a DOWN delivery.
	mwCron.Active = false
	if err := p.repos.MaintenanceRepo.Update(ctx, mwCron); err != nil {
		t.Fatal(err)
	}
	activeCfg2, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Record DOWN when maintenance is inactive -> queues delivery.
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "down before maint",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfg2.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	// Now activate a new maintenance window and refresh config before consumer sends.
	mw2 := &domain.MaintenanceWindow{
		UserID:    p.f.user(t),
		Title:     "New Maintenance",
		Strategy:  "single",
		Active:    true,
		StartDate: now.Add(-5 * time.Minute),
		EndDate:   now.Add(15 * time.Minute),
		Timezone:  "UTC",
	}
	if err := p.repos.MaintenanceRepo.Create(ctx, mw2); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.MaintenanceWindowMonitorRepo.Assign(ctx, mw2.ID, p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	_, err = refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Now consumer runs -> should supersede the delivery due to active maintenance in snapshot.
	p.send(t)
	if len(p.sender.alerts) != 0 {
		t.Fatalf("consumer should supersede delivery during maintenance, but sent %d", len(p.sender.alerts))
	}
}

func testLocalDeliveryRestartReclaim(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, true)
	p.record(t, domain.StatusDown)

	// Worker 1 claims delivery with 1-second lease.
	claimed, err := p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim failed: %v, len=%d", err, len(claimed))
	}
	if claimed[0].Attempt != 1 {
		t.Fatalf("claimed attempt = %d, want 1", claimed[0].Attempt)
	}

	// Worker 1 "crashes" and lease expires.
	expiredLeasedAt := time.Now().UTC().Add(-2 * time.Minute)
	expiredLeaseUntil := time.Now().UTC().Add(-1 * time.Minute)
	if _, err := p.f.db.ExecContext(ctx, "UPDATE probe_delivery_intents SET leased_at = ?, lease_until = ?", expiredLeasedAt, expiredLeaseUntil); err != nil {
		t.Fatal(err)
	}

	// Stale Worker 1 attempts to finish delivery with old lease token -> must fail with ErrConflict.
	claim := domain.DeliveryClaim{
		DeliveryID: claimed[0].DeliveryID,
		ProbeID:    claimed[0].ProbeID,
		Attempt:    claimed[0].Attempt,
		LeaseToken: claimed[0].LeaseToken,
	}
	err = p.outbox.FinishDelivery(ctx, claim, domain.DeliveryResult{
		Status: domain.DeliveryStatusSent,
		At:     time.Now().UTC(),
	})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict on expired lease token commit, got: %v", err)
	}

	// Worker 2 (restarted consumer) reclaims the expired delivery and sends.
	p.send(t)

	if len(p.sender.alerts) != 1 {
		t.Fatalf("restarted consumer should have sent 1 alert, got %d", len(p.sender.alerts))
	}

	// Verify delivery is finished (no pending/retrying).
	count, err := p.f.db.NewSelect().TableExpr("probe_delivery_intents").
		Where("status IN (?, ?)", domain.DeliveryStatusPending, domain.DeliveryStatusRetrying).
		Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 pending/retrying deliveries after reclaim, got %d", count)
	}
}

func testLocalDeliveryCompleteEscalationBehavior(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, true)

	// Create Channel 2 for escalation.
	nid2 := p.f.notification(t, p.f.user(t))

	// Create escalation policy with Step 1 (wait 0 min, pages Channel 2).
	policy := &domain.EscalationPolicy{
		UserID:  p.f.user(t),
		Name:    "Test Escalation",
		Enabled: true,
		Steps: []domain.EscalationStep{
			{
				StepOrder:       1,
				WaitMinutes:     0,
				NotificationIDs: []int64{nid2},
			},
		},
	}
	if err := p.repos.EscalationPolicyRepo.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.EscalationAssignmentRepo.AssignMonitor(ctx, p.monitor.ID, policy.ID); err != nil {
		t.Fatal(err)
	}

	// Refresh config so escalation policy is in applied configuration.
	refreshSvc := refreshService(t, p.f, p.f.db)
	activeCfg, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Setup escalation service wired to dispatcher.
	notifier := services.NewNotificationService(p.repos.NotificationRepo, p.repos.MonitorNotificationRepo)
	notifier.RegisterSender(p.sender)
	if reader, ok := p.active.(ports.LocalAppliedConfigReader); ok {
		notifier.SetAppliedConfigReader(reader)
	}
	escalationSvc := services.NewEscalationService(
		p.repos.EscalationPolicyRepo,
		p.repos.EscalationAssignmentRepo,
		p.repos.AlertEscalationRepo,
		p.repos.AlertRepo,
		p.repos.MonitorRepo,
		p.repos.MonitorGroupRepo,
		notifier,
	)
	escalationSvc.SetAssignmentRepository(p.f.assignments)
	escalationSvc.SetDeliveryOutbox(repository.NewEscalationOutboxStore(p.f.db, probe.LocalConfigEncoder{}))
	if reader, ok := p.active.(ports.LocalAppliedConfigReader); ok {
		escalationSvc.SetAppliedConfigReader(reader)
	}

	dispatcher := services.NewNotificationDispatcher(notifier, reviewMaintenance{})
	dispatcher.SetAssignmentRepository(p.f.assignments)
	alertSvc := services.NewAlertService(p.repos.AlertRepo)
	alertSvc.SetEscalationCanceller(escalationSvc)
	dispatcher.SetAlertLifecycle(alertSvc)
	dispatcher.SetThrottleRepository(repository.NewNotificationThrottleStore(p.f.db))
	dispatcher.SetOutboxDelivery(true)
	dispatcher.SetEscalationStarter(escalationSvc)
	p.heartbeats.SetDispatcher(dispatcher)

	// Record DOWN.
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "escalation test down",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfg.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	// Send step 0 via consumer.
	p.send(t)
	if len(p.sender.alerts) != 1 {
		t.Fatalf("expected step 0 alert sent, got %d", len(p.sender.alerts))
	}

	// Run due escalation step 1.
	sent, err := escalationSvc.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("expected 1 escalation step sent, got %d", sent)
	}
	if len(p.sender.alerts) != 1 {
		t.Fatal("escalation runner performed provider I/O before the outbox consumer")
	}
	p.send(t)
	if len(p.sender.alerts) != 2 {
		t.Fatalf("expected total 2 alerts (step 0 + step 1), got %d", len(p.sender.alerts))
	}

	// Record UP (recovery).
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusUp,
		Message:              "recovered",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfg.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	// Verify escalation is canceled and cannot send more steps.
	sentAfterResolve, err := escalationSvc.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sentAfterResolve != 0 {
		t.Fatalf("expected 0 escalation steps sent after resolution, got %d", sentAfterResolve)
	}
}

func testLocalDeliverySourceEditsThroughSupportedAppPath(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, true)

	// 1. Initial applied revision 1 is active.
	reader, ok := activationRepo(p.f, p.f.db).(ports.LocalAppliedConfigReader)
	if !ok {
		t.Fatal("activation repo does not implement LocalAppliedConfigReader")
	}
	applied, err := reader.ReadAppliedLocal(ctx)
	if err != nil || applied.Revision != 1 {
		t.Fatalf("expected applied revision 1, got %v, err=%v", applied, err)
	}

	// 2. Edit monitor through MonitorRepo.Update (supported app path).
	p.monitor.Name = "Updated Name"
	if err := p.repos.MonitorRepo.Update(ctx, p.monitor); err != nil {
		t.Fatal(err)
	}

	// 3. ReadAppliedLocal now detects source mismatch and returns ErrConflict.
	_, err = reader.ReadAppliedLocal(ctx)
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict after monitor update, got: %v", err)
	}

	// 4. Refresh prepares and activates revision 2.
	refreshSvc := refreshService(t, p.f, p.f.db)
	active2, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh after update failed: %v", err)
	}
	if active2.Revision != 2 {
		t.Fatalf("active revision after update = %d, want 2", active2.Revision)
	}

	// 5. ReadAppliedLocal now returns applied revision 2.
	applied2, err := reader.ReadAppliedLocal(ctx)
	if err != nil || applied2.Revision != 2 {
		t.Fatalf("expected applied revision 2, got %v, err=%v", applied2, err)
	}

	// 6. Record DOWN and send successfully with revision 2.
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "down rev2",
		AssignmentGeneration: 1,
		ConfigRevision:       2,
	}); err != nil {
		t.Fatal(err)
	}
	p.send(t)
	if len(p.sender.alerts) != 1 {
		t.Fatalf("expected 1 alert sent, got %d", len(p.sender.alerts))
	}
}

func testLocalDeliveryBootstrapWiringMustCreateOutboxWork(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	// Match run.go key-configured wiring: activation and monitorNotificationRepo are both wired.
	p.heartbeats.SetMonitorNotificationRepo(p.repos.MonitorNotificationRepo)
	p.record(t, domain.StatusDown)
	p.send(t)
	count, err := p.f.db.NewSelect().TableExpr("probe_delivery_intents").Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.sender.alerts) != 1 {
		t.Fatalf("key-configured bootstrap: provider sends=%d queued intents=%d; want one initial DOWN", len(p.sender.alerts), count)
	}
}

func testLocalDeliveryChannelEditMustRefreshWithoutManualTrigger(t *testing.T, engine string) {
	p := newReviewPipeline(t, engine, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bus := eventbus.NewMemoryBus()
	defer bus.Close()
	refresh := refreshService(t, p.f, p.f.db)
	refresh.SetReconcileInterval(20 * time.Millisecond)
	refresh.StartEventSubscription(ctx, bus)
	links, err := p.repos.MonitorNotificationRepo.ListByMonitor(ctx, p.monitor.ID)
	if err != nil {
		t.Fatal(err)
	}
	n, err := p.repos.NotificationRepo.GetByID(ctx, links[0].NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	n.Config = map[string]any{"url": "https://edited-channel.example.com/hook"}
	svc := services.NewNotificationService(p.repos.NotificationRepo, p.repos.MonitorNotificationRepo)
	if err := svc.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	reader := activationRepo(p.f, p.f.db).(ports.LocalAppliedConfigReader)
	deadline := time.Now().Add(2 * time.Second)
	refreshed := false
	for time.Now().Before(deadline) {
		applied, err := reader.ReadAppliedLocal(ctx)
		if err == nil && applied.Revision > 1 {
			refreshed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !refreshed {
		t.Fatal("notification service update left applied graph stale; scheduler cannot capture checks")
	}
}

func testLocalDeliveryEscalationMustNotUseUnappliedChannel(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, true)

	// Create Channel 2 for escalation.
	nid2 := p.f.notification(t, p.f.user(t))

	// Create escalation policy with Step 1 (wait 0 min, pages Channel 2).
	policy := &domain.EscalationPolicy{
		UserID:  p.f.user(t),
		Name:    "Test Escalation",
		Enabled: true,
		Steps: []domain.EscalationStep{
			{
				StepOrder:       1,
				WaitMinutes:     0,
				NotificationIDs: []int64{nid2},
			},
		},
	}
	if err := p.repos.EscalationPolicyRepo.Create(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if err := p.repos.EscalationAssignmentRepo.AssignMonitor(ctx, p.monitor.ID, policy.ID); err != nil {
		t.Fatal(err)
	}

	// Refresh config so escalation policy is in applied configuration.
	refreshSvc := refreshService(t, p.f, p.f.db)
	activeCfg, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Setup escalation service wired to dispatcher and applied reader.
	notifier := services.NewNotificationService(p.repos.NotificationRepo, p.repos.MonitorNotificationRepo)
	notifier.RegisterSender(p.sender)
	if reader, ok := p.active.(ports.LocalAppliedConfigReader); ok {
		notifier.SetAppliedConfigReader(reader)
	}
	escalationSvc := services.NewEscalationService(
		p.repos.EscalationPolicyRepo,
		p.repos.EscalationAssignmentRepo,
		p.repos.AlertEscalationRepo,
		p.repos.AlertRepo,
		p.repos.MonitorRepo,
		p.repos.MonitorGroupRepo,
		notifier,
	)
	escalationSvc.SetAssignmentRepository(p.f.assignments)
	escalationSvc.SetDeliveryOutbox(repository.NewEscalationOutboxStore(p.f.db, probe.LocalConfigEncoder{}))
	if reader, ok := p.active.(ports.LocalAppliedConfigReader); ok {
		escalationSvc.SetAppliedConfigReader(reader)
	}

	dispatcher := services.NewNotificationDispatcher(notifier, reviewMaintenance{})
	dispatcher.SetAssignmentRepository(p.f.assignments)
	alertSvc := services.NewAlertService(p.repos.AlertRepo)
	alertSvc.SetEscalationCanceller(escalationSvc)
	dispatcher.SetAlertLifecycle(alertSvc)
	dispatcher.SetThrottleRepository(repository.NewNotificationThrottleStore(p.f.db))
	dispatcher.SetOutboxDelivery(true)
	dispatcher.SetEscalationStarter(escalationSvc)
	p.heartbeats.SetDispatcher(dispatcher)

	// Record DOWN.
	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "escalation test down",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfg.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	// Send step 0 via consumer.
	p.send(t)
	if len(p.sender.alerts) != 1 {
		t.Fatalf("expected step 0 alert sent, got %d", len(p.sender.alerts))
	}

	editedChannel, err := p.repos.NotificationRepo.GetByID(ctx, nid2)
	if err != nil {
		t.Fatal(err)
	}
	editedChannel.Config = map[string]any{"url": "https://unapplied-escalation.example.com/hook"}
	if err := p.repos.NotificationRepo.Update(ctx, editedChannel); err != nil {
		t.Fatal(err)
	}
	// Run due escalation step 1.
	sent, err := escalationSvc.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 0 || len(p.sender.alerts) != 1 {
		t.Fatalf("unapplied channel must fail closed: steps=%d sends=%d", sent, len(p.sender.alerts))
	}
	assertTableCount(t, p.f, "probe_delivery_intents", 1)
}

func testLocalDeliveryAckURLMustIncludePublicURL(t *testing.T, engine string) {
	ctx := context.Background()
	p := newReviewPipeline(t, engine, true)

	// Set IncludeAckURL on the attached channel and refresh applied config
	links, err := p.repos.MonitorNotificationRepo.ListByMonitor(ctx, p.monitor.ID)
	if err != nil || len(links) == 0 {
		t.Fatalf("expected notification channel attached: %v", err)
	}
	notif, err := p.repos.NotificationRepo.GetByID(ctx, links[0].NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	notif.IncludeAckURL = true
	if err := p.repos.NotificationRepo.Update(ctx, notif); err != nil {
		t.Fatal(err)
	}
	refreshSvc := refreshService(t, p.f, p.f.db)
	activeCfg, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}

	consumerConfig := services.DefaultDeliveryConsumerConfig()
	consumerConfig.PublicURL = "https://uptime.example.com"
	consumer := services.NewDeliveryOutboxConsumer(p.outbox, p.repos.NotificationRepo, consumerConfig)
	consumer.SetAssignmentRepository(p.f.assignments)
	consumer.SetActivationRepository(p.active)
	consumer.SetIncidentRepository(p.f.incidents)
	consumer.SetMonitorNotificationRepository(p.repos.MonitorNotificationRepo)
	consumer.SetMonitorRepository(p.repos.MonitorRepo)
	consumer.SetAlertRepository(p.repos.AlertRepo)
	consumer.SetMaintenanceChecker(reviewMaintenance{})
	consumer.RegisterSender(p.sender)

	if err := p.heartbeats.Record(ctx, p.monitor, ports.CheckResult{
		Status:               domain.StatusDown,
		Message:              "review sample",
		AssignmentGeneration: 1,
		ConfigRevision:       activeCfg.Revision,
	}); err != nil {
		t.Fatalf("record status: %v", err)
	}
	if _, err := consumer.ProcessBatch(ctx, domain.LocalProbeID, time.Now().UTC(), 50); err != nil {
		t.Fatal(err)
	}
	if len(p.sender.alerts) != 1 {
		t.Fatalf("expected 1 alert sent, got %d", len(p.sender.alerts))
	}
	ackURL := p.sender.alerts[0].AckURL
	if !strings.HasPrefix(ackURL, "https://uptime.example.com/ack/") {
		t.Fatalf("expected ackURL with public URL prefix, got %q", ackURL)
	}
}

func TestLocalDeliveryContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("NewConfigDuringOutage", func(t *testing.T) { testLocalDeliveryNewConfigDuringOutage(t, engine) })
			t.Run("DefaultAvailabilitySend", func(t *testing.T) { testLocalDeliveryDefaultAvailabilitySend(t, engine) })
			t.Run("ResendAfterInterval", func(t *testing.T) { testLocalDeliveryResendAfterInterval(t, engine) })
			t.Run("AckBetweenClaimAndFirstSend", func(t *testing.T) { testLocalDeliveryAckBetweenClaimAndFirstSend(t, engine) })
			t.Run("DownAfterMaintenance", func(t *testing.T) { testLocalDeliveryDownAfterMaintenance(t, engine) })
			t.Run("RecoveryAfterAck", func(t *testing.T) { testLocalDeliveryRecoveryAfterAck(t, engine) })
			t.Run("RejectForeignSnapshotKey", func(t *testing.T) { testLocalDeliveryRejectForeignSnapshotKey(t, engine) })
			t.Run("Existing045Upgrade", func(t *testing.T) { testLocalDeliveryExisting045Upgrade(t, engine) })
			t.Run("DelayedSummaryKeepsDurableRetry", func(t *testing.T) { testLocalDeliveryDelayedSummaryKeepsDurableRetry(t, engine) })
			t.Run("ChannelEditRequiresNewAppliedVersion", func(t *testing.T) { testLocalDeliveryChannelEditRequiresNewAppliedVersion(t, engine) })
			t.Run("FirstActivationAndRefresh", func(t *testing.T) { testLocalDeliveryFirstActivationAndRefresh(t, engine) })
			t.Run("InheritedAndDisabledChannels", func(t *testing.T) { testLocalDeliveryInheritedAndDisabledChannels(t, engine) })
			t.Run("MaintenanceParity", func(t *testing.T) { testLocalDeliveryMaintenanceParity(t, engine) })
			t.Run("RestartReclaim", func(t *testing.T) { testLocalDeliveryRestartReclaim(t, engine) })
			t.Run("CompleteEscalationBehavior", func(t *testing.T) { testLocalDeliveryCompleteEscalationBehavior(t, engine) })
			t.Run("SourceEditsThroughSupportedAppPath", func(t *testing.T) { testLocalDeliverySourceEditsThroughSupportedAppPath(t, engine) })
			t.Run("BootstrapWiringMustCreateOutboxWork", func(t *testing.T) { testLocalDeliveryBootstrapWiringMustCreateOutboxWork(t, engine) })
			t.Run("ChannelEditMustRefreshWithoutManualTrigger", func(t *testing.T) { testLocalDeliveryChannelEditMustRefreshWithoutManualTrigger(t, engine) })
			t.Run("EscalationMustNotUseUnappliedChannel", func(t *testing.T) { testLocalDeliveryEscalationMustNotUseUnappliedChannel(t, engine) })
			t.Run("AckURLMustIncludePublicURL", func(t *testing.T) { testLocalDeliveryAckURLMustIncludePublicURL(t, engine) })
		})
	}
}

func TestProbeInstallationRacesSnapshotWriter(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			doc, target := configDocument(t, domain.LocalProbeID, 1, "race-secret")
			key, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{99}, 32))
			if err != nil {
				t.Fatal(err)
			}
			installer := services.NewProbeInstallationService(installationRepo(f, f.db))
			writer := protectedConfigService(t, configRepo(f, reopenConfigDB(t, f)))
			start := make(chan struct{})
			var wg sync.WaitGroup
			var initErr, saveErr error
			wg.Add(2)
			go func() { defer wg.Done(); <-start; _, initErr = installer.InitializeOrVerify(ctx, key, target.HubID) }()
			go func() { defer wg.Done(); <-start; _, saveErr = writer.Prepare(ctx, target, doc, 0) }()
			close(start)
			wg.Wait()
			if (initErr == nil) == (saveErr == nil) {
				t.Fatalf("need exactly one authority winner: init=%v save=%v", initErr, saveErr)
			}
			if initErr == nil {
				assertTableCount(t, f, "probe_config_snapshots", 0)
			} else {
				assertTableCount(t, f, "probe_installation", 0)
				assertTableCount(t, f, "probe_config_snapshots", 1)
			}
		})
	}
}
