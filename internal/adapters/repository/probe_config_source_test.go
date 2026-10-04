package repository_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestLocalConfigSourceContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"CompleteProtectedGraph":   testLocalConfigSourceGraph,
				"ConsistentConcurrentEdit": testLocalConfigSourceConsistency,
				"EmptyLegacyAndFailure":    testLocalConfigSourceFailure,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func insertConfigModel(t *testing.T, db *bun.DB, model any) {
	t.Helper()
	if _, err := db.NewInsert().Model(model).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func sourceConfigBuilder(t *testing.T, f probeRegistryFixture, db *bun.DB) (*services.LocalProbeConfigBuilder, *services.ProbeConfigService) {
	t.Helper()
	prepared := protectedConfigService(t, configRepo(f, db))
	return services.NewLocalProbeConfigBuilder(repository.NewLocalProbeConfigSourceStore(db), probe.LocalConfigEncoder{}, prepared), prepared
}

func testLocalConfigSourceGraph(t *testing.T, f probeRegistryFixture) {
	ctx := context.Background()
	userID := f.user(t)
	local, paused, remote := f.monitor(t), f.monitor(t), f.monitor(t)
	for _, id := range []int64{local, paused, remote} {
		if _, err := f.assignments.InitializeLocal(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	f.remote(t, probeRegistryID1, "edge")
	if _, err := f.assignments.Replace(ctx, remote, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	// Local remove/re-add must use the durable generation, never a literal one.
	if _, err := f.assignments.Replace(ctx, local, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(ctx, local, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC)
	parent := &repository.MonitorGroupModel{UserID: userID, Name: "parent", Owner: "operations", CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, parent)
	child := &repository.MonitorGroupModel{UserID: userID, Name: "child", ParentID: &parent.ID, CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, child)
	proxy := &repository.ProxyModel{UserID: userID, Protocol: "http", Host: "proxy.test", Port: 8080, Auth: true, Username: "fixture-user", Password: "fixture-proxy-secret", Active: true}
	insertConfigModel(t, f.db, proxy)
	for _, id := range []int64{local, paused} {
		if _, err := f.db.NewUpdate().Table("monitors").Set("group_id = ?", child.ID).Set("inherit_group_owner = ?", true).Where("id = ?", id).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.NewUpdate().Table("monitors").Set("proxy_id = ?", proxy.ID).Set("config = ?", repository.JSONField{"url": "https://local.test", "token": "fixture-monitor-secret"}).Where("id = ?", local).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.NewUpdate().Table("monitors").Set("type = ?", "push").Set("active = ?", false).Where("id = ?", paused).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	template := &repository.NotificationTemplateModel{UserID: userID, Name: "template", Provider: "webhook", BodyTemplate: "{{monitor.name}}", Config: repository.JSONField{}, CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, template)
	direct := &repository.NotificationModel{UserID: userID, Name: "direct", Type: "webhook", Active: true, IncludeAckURL: true, TemplateID: &template.ID, Config: repository.JSONField{"url": "https://example.test/hook", "token": "fixture-direct-secret"}, CreatedAt: at, UpdatedAt: at}
	escalation := &repository.NotificationModel{UserID: userID, Name: "escalation", Type: "webhook", Active: true, Config: repository.JSONField{"url": "https://example.test/escalate"}, CreatedAt: at, UpdatedAt: at}
	unused := &repository.NotificationModel{UserID: userID, Name: "group-and-parent", Type: "webhook", Active: true, Config: repository.JSONField{"secret": "never-export-parent-secret"}, CreatedAt: at, UpdatedAt: at}
	for _, n := range []*repository.NotificationModel{direct, escalation, unused} {
		insertConfigModel(t, f.db, n)
	}
	insertConfigModel(t, f.db, &repository.MonitorNotificationModel{MonitorID: local, NotificationID: direct.ID, IncludeTarget: false})
	insertConfigModel(t, f.db, &repository.GroupNotificationModel{GroupID: child.ID, NotificationID: unused.ID})
	insertConfigModel(t, f.db, &repository.MonitorNotificationModel{MonitorID: remote, NotificationID: unused.ID, IncludeTarget: true})
	policy := &repository.EscalationPolicyModel{UserID: userID, Name: "disabled direct", Enabled: false, CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, policy)
	step := &repository.EscalationStepModel{PolicyID: policy.ID, StepOrder: 1, WaitMinutes: 5}
	insertConfigModel(t, f.db, step)
	insertConfigModel(t, f.db, &repository.EscalationStepNotificationModel{StepID: step.ID, NotificationID: escalation.ID})
	insertConfigModel(t, f.db, &repository.EscalationPolicyMonitorModel{MonitorID: local, PolicyID: policy.ID})
	parentPolicy := &repository.EscalationPolicyModel{UserID: userID, Name: "parent", Enabled: true, CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, parentPolicy)
	parentStep := &repository.EscalationStepModel{PolicyID: parentPolicy.ID, StepOrder: 1, WaitMinutes: 1}
	insertConfigModel(t, f.db, parentStep)
	insertConfigModel(t, f.db, &repository.EscalationStepNotificationModel{StepID: parentStep.ID, NotificationID: unused.ID})
	insertConfigModel(t, f.db, &repository.EscalationPolicyGroupModel{GroupID: parent.ID, PolicyID: parentPolicy.ID})
	emptyPolicy := &repository.EscalationPolicyModel{UserID: userID, Name: "empty nearest", Enabled: true, CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, emptyPolicy)
	insertConfigModel(t, f.db, &repository.EscalationPolicyGroupModel{GroupID: child.ID, PolicyID: emptyPolicy.ID})
	tag := &repository.TagModel{Name: "service", Color: "#abcdef"}
	insertConfigModel(t, f.db, tag)
	insertConfigModel(t, f.db, &repository.MonitorTagModel{MonitorID: local, TagID: tag.ID, Value: "payments"})
	window := &repository.MaintenanceWindowModel{UserID: userID, Title: "shared", Active: true, Strategy: "single", StartDate: at, EndDate: at.Add(time.Hour), Timezone: "UTC"}
	insertConfigModel(t, f.db, window)
	for _, id := range []int64{local, remote} {
		insertConfigModel(t, f.db, &repository.MaintenanceWindowMonitorModel{MaintenanceWindowID: window.ID, MonitorID: id})
	}
	// No-link and remote-only windows must never suppress local checks.
	for _, linked := range []bool{false, true} {
		w := &repository.MaintenanceWindowModel{UserID: userID, Title: "excluded", Active: true, Strategy: "single", StartDate: at, EndDate: at.Add(time.Hour), Timezone: "UTC"}
		insertConfigModel(t, f.db, w)
		if linked {
			insertConfigModel(t, f.db, &repository.MaintenanceWindowMonitorModel{MaintenanceWindowID: w.ID, MonitorID: remote})
		}
	}
	builder, prepared := sourceConfigBuilder(t, f, f.db)
	meta, err := builder.Prepare(ctx, probeRegistryID2, 0, at, at)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := prepared.Read(ctx, meta.ProbeConfigTarget, 1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := probe.DecodeLocalConfigSnapshot(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Assignments) != 2 || snapshot.Assignments[0].Generation != 2 || snapshot.Assignments[1].Active || snapshot.Assignments[1].Monitor.Type != "push" || snapshot.Assignments[0].Monitor.EffectiveOwner != "operations" {
		t.Fatal("lost local generation, pause or inherited metadata")
	}
	if len(snapshot.NotificationChannels) != 2 || len(snapshot.NotificationTemplates) != 1 || len(snapshot.EscalationPolicies) != 2 || snapshot.EscalationPolicies[0].Enabled || len(snapshot.EscalationPolicies[1].Steps) != 0 || snapshot.Assignments[0].NotificationLinks[0].IncludeTarget || !snapshot.NotificationChannels[0].IncludeAckURL {
		t.Fatal("dependency closure changed live preferences/precedence")
	}
	if len(snapshot.MaintenanceWindows) != 1 || !reflect.DeepEqual(snapshot.MaintenanceWindows[0].MonitorIDs, []int64{local}) || len(snapshot.Assignments[1].MaintenanceIDs) != 0 || len(snapshot.Assignments[0].Monitor.Tags) != 1 {
		t.Fatal("maintenance or tags changed")
	}
	if bytes.Contains(doc, []byte("never-export-parent-secret")) || len(snapshot.ProxyBindings) != 1 {
		t.Fatal("unreferenced or missing credentials")
	}
	stored, err := configRepo(f, f.db).Latest(ctx, domain.LocalProbeID)
	if err != nil || bytes.Contains(stored.ProtectedPayload, []byte("fixture-direct-secret")) || bytes.Contains(stored.ProtectedPayload, []byte("fixture-monitor-secret")) {
		t.Fatal("snapshot not protected")
	}
	// Reconnecting and repeating the same preparation returns the original bytes.
	builder2, prepared2 := sourceConfigBuilder(t, f, reopenConfigDB(t, f))
	meta2, err := builder2.Prepare(ctx, probeRegistryID2, 0, at, at)
	if err != nil || !domain.SameProbeConfigMetadata(meta, meta2) {
		t.Fatal("identical build was not idempotent")
	}
	if _, err := f.db.NewUpdate().Table("notifications").Set("config = ?", repository.JSONField{"token": "rotated-secret"}).Where("id = ?", direct.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := builder2.Prepare(ctx, probeRegistryID2, 0, at, at); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("same revision changed content")
	}
	if _, err := builder2.Prepare(ctx, probeRegistryID2, 1, at.Add(time.Second), at); err != nil {
		t.Fatal(err)
	}
	old, _, err := prepared2.Read(ctx, meta.ProbeConfigTarget, 1)
	if err != nil || !bytes.Equal(doc, old) {
		t.Fatal("new source configuration changed retained bytes")
	}
	latest, _, err := prepared2.Read(ctx, meta.ProbeConfigTarget, 0)
	if err != nil || !bytes.Contains(latest, []byte("rotated-secret")) {
		t.Fatal("new revision did not carry current credential")
	}
}

type configReadBarrier struct {
	once    sync.Once
	reached chan struct{}
	done    chan struct{}
}

func (h *configReadBarrier) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *configReadBarrier) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	query := strings.NewReplacer("`", "", `"`, "").Replace(event.Query)
	if event.Err != nil || !strings.HasPrefix(query, "SELECT ") || !strings.Contains(query, "FROM monitors AS monitor_model") {
		return
	}
	h.once.Do(func() {
		close(h.reached)
		select {
		case <-h.done:
		case <-ctx.Done():
		}
	})
}

func testLocalConfigSourceConsistency(t *testing.T, f probeRegistryFixture) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, id); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	n := &repository.NotificationModel{UserID: f.user(t), Name: "channel", Type: "webhook", Active: true, Config: repository.JSONField{"generation": "before"}, CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, n)
	insertConfigModel(t, f.db, &repository.MonitorNotificationModel{MonitorID: id, NotificationID: n.ID})
	f.db.SetMaxOpenConns(1)
	if f.engine == "sqlite" {
		if _, err := f.db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			t.Fatal(err)
		}
	} else {
		// The source must override a weaker deployment/session default.
		if _, err := f.db.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			t.Fatal(err)
		}
	}
	peer := reopenConfigDB(t, f)
	barrier := &configReadBarrier{reached: make(chan struct{}), done: make(chan struct{})}
	f.db.AddQueryHook(barrier)
	writer := make(chan error, 1)
	go func() {
		defer close(barrier.done)
		select {
		case <-barrier.reached:
		case <-ctx.Done():
			writer <- ctx.Err()
			return
		}
		writer <- peer.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewUpdate().Table("monitors").Set("name = ?", "after").Where("id = ?", id).Exec(ctx); err != nil {
				return err
			}
			_, err := tx.NewUpdate().Table("notifications").Set("config = ?", repository.JSONField{"generation": "after"}).Where("id = ?", n.ID).Exec(ctx)
			return err
		})
	}()
	reader := repository.NewLocalProbeConfigSourceStore(f.db)
	source, err := reader.ReadLocal(ctx)
	writeErr := <-writer
	if err != nil || writeErr != nil {
		t.Fatalf("concurrent snapshot: read %v write %v", err, writeErr)
	}
	if source.Assignments[0].Monitor.Name != "Legacy monitor" || source.Notifications[n.ID].Config["generation"] != "before" {
		t.Fatal("source mixed configurations across one committed edit")
	}
	source, err = reader.ReadLocal(ctx)
	if err != nil || source.Assignments[0].Monitor.Name != "after" || source.Notifications[n.ID].Config["generation"] != "after" {
		t.Fatal("next source read missed committed configuration")
	}
}

func testLocalConfigSourceFailure(t *testing.T, f probeRegistryFixture) {
	ctx := context.Background()
	reader := repository.NewLocalProbeConfigSourceStore(f.db)
	if source, err := reader.ReadLocal(ctx); err != nil || len(source.Assignments) != 0 {
		t.Fatal("empty local installation failed")
	}
	id := f.monitor(t)
	if source, err := reader.ReadLocal(ctx); !errors.Is(err, domain.ErrValidation) || source != nil {
		t.Fatal("legacy assignment generation invented")
	}
	if _, err := f.assignments.InitializeLocal(ctx, id); err != nil {
		t.Fatal(err)
	}
	if source, err := reader.ReadLocal(ctx); err != nil || source.Assignments[0].Generation != 1 {
		t.Fatal("initialized assignment unavailable")
	}
	if _, err := f.db.NewUpdate().Table("probes").Set("enabled = ?", false).Where("id = ?", domain.LocalProbeID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if source, err := reader.ReadLocal(ctx); !errors.Is(err, domain.ErrValidation) || source != nil {
		t.Fatal("disabled registration supplied source")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if source, err := reader.ReadLocal(canceled); !errors.Is(err, context.Canceled) || source != nil {
		t.Fatal("ignored cancellation")
	}
}
