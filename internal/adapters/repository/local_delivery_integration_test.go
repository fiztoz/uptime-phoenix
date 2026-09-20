package repository_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
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
}

type pipelineRepositories struct {
	MonitorRepo             ports.MonitorRepository
	HeartbeatRepo           ports.HeartbeatRepository
	NotificationRepo        ports.NotificationRepository
	MonitorNotificationRepo ports.MonitorNotificationRepository
	AlertRepo               ports.AlertRepository
}

func newReviewPipeline(t *testing.T, engine string, activate bool) reviewPipeline {
	t.Helper()
	ctx := context.Background()
	f := newProbeRegistryFixture(t, engine)
	var repos pipelineRepositories
	if engine == "sqlite" {
		r := sqlite.NewRepository(f.db)
		repos = pipelineRepositories{r.MonitorRepo, r.HeartbeatRepo, r.NotificationRepo, r.MonitorNotificationRepo, r.AlertRepo}
	} else {
		r := mariadb.NewRepository(f.db)
		repos = pipelineRepositories{r.MonitorRepo, r.HeartbeatRepo, r.NotificationRepo, r.MonitorNotificationRepo, r.AlertRepo}
	}
	id := localMonitor(t, f)
	nid := f.notification(t, f.user(t))
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
	return reviewPipeline{f, repos, monitor, heartbeats, consumer, outbox, sender}
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
	// Model an installation that already applied 045 from the upstream commit.
	// Recreate the empty outbox with that exact old schema; leave migration tracking intact.
	if _, err := p.f.db.ExecContext(ctx, "DROP TABLE probe_delivery_intents"); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(engine, "migrations/045_probe_delivery_outbox.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(old), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := p.f.db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.f.db.ExecContext(ctx, "DELETE FROM _migrations WHERE filename = ?", "050_delivery_cancellation.up.sql"); err != nil {
		t.Fatal(err)
	}
	p.record(t, domain.StatusDown) // Pending work exists under the already-applied 045 schema.
	claimed, err := p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("pre-upgrade lease: %v %v", claimed, err)
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
