package repository_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestProbeWatchdogSettingsContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"SavedGraphWithoutMonitors": testWatchdogSettingsGraph,
				"ConcurrentCASAndRestart":   testWatchdogSettingsCAS,
				"LateRollbackAndBounds":     testWatchdogSettingsRollback,
				"MigrationRoundTrip":        testWatchdogSettingsMigration,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func testWatchdogSettingsGraph(t *testing.T, f probeRegistryFixture) {
	syncer, protector, monitor, channel := remoteSyncFixture(t, f)
	ctx := t.Context()
	if _, err := f.assignments.Replace(ctx, monitor, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	template := &repository.NotificationTemplateModel{UserID: f.user(t), Name: "probe template", Provider: "webhook", BodyTemplate: "{{message}}", Config: repository.JSONField{}, CreatedAt: at, UpdatedAt: at}
	insertConfigModel(t, f.db, template)
	if _, err := f.db.NewUpdate().Table("notifications").Set("template_id = ?", template.ID).Set("active = ?", false).Where("id = ?", channel).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	registered, err := f.registry.GetByID(ctx, probeRegistryID1)
	if err != nil {
		t.Fatal(err)
	}
	registered.Name, registered.Location = "BKK edge", "Bangkok"
	if err := f.registry.Update(ctx, registered, registered.Revision); err != nil {
		t.Fatal(err)
	}
	store := repository.NewProbeWatchdogSettingsStore(f.db)
	saved, err := store.Get(ctx, probeRegistryID1)
	if err != nil || !reflect.DeepEqual(saved, domain.DefaultProbeWatchdogSettings()) {
		t.Fatalf("default: %+v %v", saved, err)
	}
	unchanged, err := store.Replace(ctx, probeRegistryID1, 0, saved)
	if err != nil || !reflect.DeepEqual(unchanged, saved) {
		t.Fatal("saving unchanged defaults must preserve revision-zero intent", err)
	}
	saved.NotificationIDs = []int64{channel}
	saved.ResendInterval = 5
	saved, err = store.Replace(ctx, probeRegistryID1, 0, saved)
	if err != nil || saved.Revision != 1 {
		t.Fatalf("save: %+v %v", saved, err)
	}
	meta, err := syncer.RefreshRemote(ctx, syncTarget(), at)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := repository.NewProbeConfigStore(f.db).Latest(ctx, probeRegistryID1)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protector.Open(ctx, meta, latest.ProtectedPayload)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := probe.DecodeConfigSnapshot(plain)
	clear(plain)
	if err != nil || len(snapshot.Assignments) != 0 || len(snapshot.NotificationChannels) != 1 || snapshot.NotificationChannels[0].Active || len(snapshot.NotificationTemplates) != 1 || snapshot.Probe == nil || snapshot.Probe.Name != "BKK edge" || snapshot.Probe.Location != "Bangkok" || snapshot.Watchdog.ResendInterval != 5 || !reflect.DeepEqual(snapshot.Watchdog.NotificationIDs, []int64{channel}) {
		t.Fatalf("incomplete watchdog-only source graph: %+v %v", snapshot.Watchdog, err)
	}
	// Deletion removes dangling settings links, and changes the complete config
	// despite no operator settings revision change or event-bus notification.
	if _, err := f.db.NewDelete().Table("notifications").Where("id = ?", channel).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Get(ctx, probeRegistryID1)
	if err != nil || len(deleted.NotificationIDs) != 0 {
		t.Fatal("dangling channel", err)
	}
	next, err := syncer.RefreshRemote(ctx, syncTarget(), at.Add(time.Second))
	if err != nil || next.Revision != meta.Revision+1 || next.SHA256 == meta.SHA256 {
		t.Fatal("channel removal not published", err)
	}
	// Saved enabled intent publishes a complete new graph, not an applied receipt.
	deleted.Enabled = true
	if _, err := store.Replace(ctx, probeRegistryID1, deleted.Revision, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.RefreshRemote(ctx, syncTarget(), at); err != nil {
		t.Fatal("enabled watchdog graph rejected", err)
	}
	retained, err := repository.NewProbeConfigStore(f.db).Latest(ctx, probeRegistryID1)
	if err != nil || retained.Revision != next.Revision+1 {
		t.Fatal("enabled graph not published at a new revision", err)
	}
}

func testWatchdogSettingsCAS(t *testing.T, f probeRegistryFixture) {
	f.remote(t, probeRegistryID1, "watchdog-edge")
	store := repository.NewProbeWatchdogSettingsStore(f.db)
	peer := repository.NewProbeWatchdogSettingsStore(reopenConfigDB(t, f))
	one, two := domain.DefaultProbeWatchdogSettings(), domain.DefaultProbeWatchdogSettings()
	one.Enabled, two.LostAfterSeconds = true, 120
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var group sync.WaitGroup
	for i, adapter := range []*repository.ProbeWatchdogSettingsStore{store, peer} {
		group.Go(func() {
			<-start
			desired := one
			if i == 1 {
				desired = two
			}
			_, err := adapter.Replace(t.Context(), probeRegistryID1, 0, desired)
			errorsCh <- err
		})
	}
	close(start)
	group.Wait()
	close(errorsCh)
	wins, conflicts := 0, 0
	for err := range errorsCh {
		if err == nil {
			wins++
		} else if errors.Is(err, ports.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	saved, err := peer.Get(t.Context(), probeRegistryID1)
	if err != nil || saved.Revision != 1 {
		t.Fatal("restart lost winner", err)
	}
	again, err := store.Replace(t.Context(), probeRegistryID1, 1, saved)
	if err != nil || !reflect.DeepEqual(again, saved) {
		t.Fatal("no-op changed revision", err)
	}
	if _, err := peer.Replace(t.Context(), probeRegistryID1, 0, saved); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale no-op not fenced", err)
	}
}

func testWatchdogSettingsRollback(t *testing.T, f probeRegistryFixture) {
	f.remote(t, probeRegistryID1, "watchdog-edge")
	store := repository.NewProbeWatchdogSettingsStore(f.db)
	channel := f.notification(t, f.user(t))
	original := domain.DefaultProbeWatchdogSettings()
	original.ResendInterval = 3
	original, err := store.Replace(t.Context(), probeRegistryID1, 0, original)
	if err != nil {
		t.Fatal(err)
	}
	trigger := "CREATE TRIGGER fail_watchdog_settings BEFORE INSERT ON probe_watchdog_notifications BEGIN SELECT RAISE(ABORT,'settings fault'); END"
	if f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_watchdog_settings BEFORE INSERT ON probe_watchdog_notifications FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='settings fault'"
	}
	if _, err := f.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.db.ExecContext(t.Context(), "DROP TRIGGER IF EXISTS fail_watchdog_settings") })
	next := original
	next.Enabled, next.NotificationIDs = true, []int64{channel}
	if _, err := store.Replace(t.Context(), probeRegistryID1, original.Revision, next); err == nil {
		t.Fatal("late failure committed")
	}
	after, err := store.Get(t.Context(), probeRegistryID1)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("scalar or link partially committed", err)
	}
	for _, mutate := range []func(*domain.ProbeWatchdogSettings){
		func(s *domain.ProbeWatchdogSettings) { s.LostAfterSeconds = 0 },
		func(s *domain.ProbeWatchdogSettings) { s.RecoverAfterSeconds = -1 },
		func(s *domain.ProbeWatchdogSettings) { s.ResendInterval = 2147483647 },
		func(s *domain.ProbeWatchdogSettings) { s.NotificationIDs = []int64{channel, channel} },
		func(s *domain.ProbeWatchdogSettings) { s.NotificationIDs = []int64{channel + 100000} },
	} {
		bad := original
		mutate(&bad)
		if _, err := store.Replace(t.Context(), probeRegistryID1, original.Revision, bad); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("bad settings not rejected", err)
		}
	}
	if _, err := store.Get(t.Context(), domain.LocalProbeID); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("local watchdog allowed", err)
	}
	if _, err := store.Get(t.Context(), probeRegistryID2); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("missing probe defaulted", err)
	}
	p, err := f.registry.GetByID(t.Context(), probeRegistryID1)
	if err != nil {
		t.Fatal(err)
	}
	p.Enabled = false
	if err := f.registry.Update(t.Context(), p, p.Revision); err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Get(t.Context(), probeRegistryID1); err != nil || !reflect.DeepEqual(saved, original) {
		t.Fatal("disabled registration hid saved intent", err)
	}
	if _, err := store.Replace(t.Context(), probeRegistryID1, original.Revision, original); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("disabled registration allowed write", err)
	}
}

func testWatchdogSettingsMigration(t *testing.T, f probeRegistryFixture) {
	for _, direction := range []string{"down", "up", "up"} {
		if err := runEngineMigration(t, f.db, f.engine, "060_probe_watchdog_settings", direction); err != nil {
			t.Fatal(err)
		}
	}
	f.remote(t, probeRegistryID1, "watchdog-edge")
	store := repository.NewProbeWatchdogSettingsStore(f.db)
	s := domain.DefaultProbeWatchdogSettings()
	s.Enabled = true
	if _, err := store.Replace(t.Context(), probeRegistryID1, 0, s); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_watchdog_settings SET lost_after_seconds=0 WHERE probe_id=?", probeRegistryID1); err == nil {
		t.Fatal("zero timing accepted by schema")
	}
	if f.engine == "sqlite" {
		if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_watchdog_settings SET lost_after_seconds=1.5 WHERE probe_id=?", probeRegistryID1); err == nil {
			t.Fatal("fractional timing accepted by SQLite schema")
		}
	}
}
