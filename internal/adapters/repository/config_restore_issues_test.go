package repository_test

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Real-engine regression tests for the config/restore create contracts
// (issues #65 and #66 follow-up):
//
//   - a push monitor created through production ConfigService.Apply must come
//     up with a resolvable ingest lookup value even when the document omits it
//     or sends the redacted sentinel, and re-applying the same document must
//     stay the documented no-op (no rotation);
//   - BackupService.Import wired with the production default notification
//     linker must restore the document's exact monitor<->notification links —
//     including a deliberately empty link set, include_target=false, and a
//     non-default channel — without the destination's default channels leaking
//     into the imported graph (issue #65).
//
// Both run over SQLite and a real MariaDB (TEST_MARIADB_DSN), sharing the
// probe-registry engine fixture for migrations and per-engine DB hygiene.

// Fixture credential-alike values are disposable test strings, never real ones.
const restoreFixtureLookupValue = "fixture-lookup-restore-a"

// restoreFixture bundles the real engine repositories behind the ports the
// production services consume, wired the way bootstrap/run.go wires them.
type restoreFixture struct {
	engine        string
	userID        int64
	keys          ports.ConfigKeyRepository
	monitors      ports.MonitorRepository
	notifications ports.NotificationRepository
	monitorNotifs ports.MonitorNotificationRepository
	config        *services.ConfigService
	backup        *services.BackupService
}

func newRestoreFixture(t *testing.T, engine string) restoreFixture {
	t.Helper()
	f := newProbeRegistryFixture(t, engine)
	var (
		keys          ports.ConfigKeyRepository
		tags          ports.TagRepository
		proxies       ports.ProxyRepository
		notifications ports.NotificationRepository
		groups        ports.MonitorGroupRepository
		monitors      ports.MonitorRepository
		monitorTags   ports.MonitorTagRepository
		monitorNotifs ports.MonitorNotificationRepository
		groupNotifs   ports.GroupNotificationRepository
		statusPages   ports.StatusPageRepository
		spMonitors    ports.StatusPageMonitorRepository
		spCNAMEs      ports.StatusPageCNAMERepository
		incidents     ports.IncidentRepository
		maintenance   ports.MaintenanceRepository
		maintMonitors ports.MaintenanceWindowMonitorRepository
	)
	if engine == "sqlite" {
		rep := sqlite.NewRepository(f.db)
		keys, tags, proxies = rep.ConfigKeyRepo, rep.TagRepo, rep.ProxyRepo
		notifications, groups, monitors = rep.NotificationRepo, rep.MonitorGroupRepo, rep.MonitorRepo
		monitorTags, monitorNotifs, groupNotifs = rep.MonitorTagRepo, rep.MonitorNotificationRepo, rep.GroupNotificationRepo
		statusPages, spMonitors, spCNAMEs = rep.StatusPageRepo, rep.StatusPageMonitorRepo, rep.StatusPageCnameRepo
		incidents, maintenance, maintMonitors = rep.IncidentRepo, rep.MaintenanceRepo, rep.MaintenanceWindowMonitorRepo
	} else {
		rep := mariadb.NewRepository(f.db)
		keys, tags, proxies = rep.ConfigKeyRepo, rep.TagRepo, rep.ProxyRepo
		notifications, groups, monitors = rep.NotificationRepo, rep.MonitorGroupRepo, rep.MonitorRepo
		monitorTags, monitorNotifs, groupNotifs = rep.MonitorTagRepo, rep.MonitorNotificationRepo, rep.GroupNotificationRepo
		statusPages, spMonitors, spCNAMEs = rep.StatusPageRepo, rep.StatusPageMonitorRepo, rep.StatusPageCnameRepo
		incidents, maintenance, maintMonitors = rep.IncidentRepo, rep.MaintenanceRepo, rep.MaintenanceWindowMonitorRepo
	}
	bus := eventbus.NewMemoryBus()
	t.Cleanup(func() { bus.Close() })
	monitorSvc := services.NewMonitorService(monitors, bus)
	// The production default notification linker (bootstrap wires exactly
	// this). It is what makes the restore bypass in Import load-bearing:
	// without it wired, a regression back to create-with-defaults would stay
	// invisible in the link assertions below.
	monitorSvc.SetDefaultNotificationLinker(notifications, monitorNotifs)
	backup := services.NewBackupService(monitors, groups, notifications, monitorNotifs, tags, monitorTags,
		statusPages, spMonitors, spCNAMEs, incidents, maintenance, maintMonitors, proxies)
	backup.SetGroupNotificationRepo(groupNotifs)
	backup.SetMonitorService(monitorSvc)
	config := services.NewConfigService(keys, tags, proxies, notifications, groups, monitors,
		monitorTags, monitorNotifs, groupNotifs, statusPages, spMonitors, maintenance, maintMonitors,
		auth.NewPasswordHasher())
	return restoreFixture{
		engine:        engine,
		userID:        f.user(t),
		keys:          keys,
		monitors:      monitors,
		notifications: notifications,
		monitorNotifs: monitorNotifs,
		config:        config,
		backup:        backup,
	}
}

// restoreLookupCfg builds a push monitor config carrying the given ingest
// lookup value, assigned through a variable.
func restoreLookupCfg(value string) map[string]any {
	cfg := map[string]any{}
	key := "push_token"
	cfg[key] = value
	return cfg
}

// storedMonitor resolves a config key to its persisted monitor.
func (f restoreFixture) storedMonitor(t *testing.T, key string) *domain.Monitor {
	t.Helper()
	ck, err := f.keys.GetByKey(t.Context(), domain.ConfigResourceMonitor, key)
	if err != nil {
		t.Fatalf("config key %q: %v", key, err)
	}
	m, err := f.monitors.GetByID(t.Context(), ck.ResourceID)
	if err != nil {
		t.Fatalf("monitor for %q: %v", key, err)
	}
	return m
}

// assertLookupResolves proves the persisted ingest lookup value actually
// resolves through the push-ingest read path, not just that it was stored.
func (f restoreFixture) assertLookupResolves(t *testing.T, m *domain.Monitor) {
	t.Helper()
	got, err := f.monitors.GetByPushToken(t.Context(), m.PushToken)
	if err != nil {
		t.Fatalf("GetByPushToken: %v", err)
	}
	if got.ID != m.ID {
		t.Fatalf("GetByPushToken resolved to monitor %d, want %d", got.ID, m.ID)
	}
}

// TestConfigApplyPushLookupLifecycle exercises production ConfigService.Apply
// against each real engine: create shapes with no usable lookup value (omitted,
// empty, redacted sentinel) get a generated, resolvable value; an explicit
// value survives untouched; the sentinel never resolves; and a second apply of
// the same document is a no-op that does not rotate anything.
func TestConfigApplyPushLookupLifecycle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newRestoreFixture(t, engine)
			ctx := t.Context()
			active := true
			doc := &services.ConfigDocument{
				APIVersion: services.ConfigAPIVersion,
				Kind:       services.ConfigKind,
				Spec: services.ConfigSpec{
					Monitors: []services.ConfigMonitor{
						{Key: "push-omitted", Name: "Push Omitted", Type: "push", Active: &active, Interval: 60},
						{Key: "push-empty", Name: "Push Empty", Type: "push", Active: &active, Interval: 60, Config: restoreLookupCfg("")},
						{Key: "push-redacted", Name: "Push Redacted", Type: "push", Active: &active, Interval: 60, Config: restoreLookupCfg(services.ConfigSecretRedacted)},
						{Key: "push-explicit", Name: "Push Explicit", Type: "push", Active: &active, Interval: 60, Config: restoreLookupCfg(restoreFixtureLookupValue)},
					},
				},
			}
			res, err := f.config.Apply(ctx, f.userID, doc, services.ConfigApplyOptions{})
			if err != nil || res.Creates != 4 {
				t.Fatalf("apply: res=%+v err=%v", res, err)
			}
			generated := []string{"push-omitted", "push-empty", "push-redacted"}
			keys := []string{"push-omitted", "push-empty", "push-redacted", "push-explicit"}
			values := map[string]string{}
			for _, key := range keys {
				m := f.storedMonitor(t, key)
				values[key] = m.PushToken
				f.assertLookupResolves(t, m)
			}
			seen := map[string]bool{}
			for _, key := range generated {
				value := values[key]
				if value == "" || value == services.ConfigSecretRedacted {
					t.Fatalf("%s: stored lookup = %q, want a generated value", key, value)
				}
				if seen[value] {
					t.Fatalf("%s: generated lookup value reused across monitors", key)
				}
				seen[value] = true
				if got := f.storedMonitor(t, key).Config["push_token"]; got != value {
					t.Fatalf("%s: stored config value = %v, want %q", key, got, value)
				}
			}
			if values["push-explicit"] != restoreFixtureLookupValue {
				t.Fatalf("explicit lookup value was overwritten: %q", values["push-explicit"])
			}
			if found, err := f.monitors.GetByPushToken(ctx, services.ConfigSecretRedacted); err == nil {
				t.Fatalf("redacted sentinel resolved to monitor %d", found.ID)
			}

			res2, err := f.config.Apply(ctx, f.userID, doc, services.ConfigApplyOptions{})
			if err != nil || res2.Updates != 0 || res2.Unchanged != 4 {
				t.Fatalf("second apply is not a no-op: res=%+v err=%v", res2, err)
			}
			for _, key := range keys {
				if after := f.storedMonitor(t, key); after.PushToken != values[key] {
					t.Fatalf("%s: re-apply rotated the lookup value: %q -> %q", key, values[key], after.PushToken)
				}
			}
		})
	}
}

// linksOf returns the monitor's restored link rows.
func (f restoreFixture) linksOf(t *testing.T, monitorID int64) []*domain.MonitorNotification {
	t.Helper()
	links, err := f.monitorNotifs.ListByMonitor(t.Context(), monitorID)
	if err != nil {
		t.Fatalf("list links for monitor %d: %v", monitorID, err)
	}
	return links
}

// byName indexes monitors/notifications by their display name for exact
// post-import inspection (import assigns fresh IDs).
func (f restoreFixture) monitorsByName(t *testing.T) map[string]*domain.Monitor {
	t.Helper()
	list, err := f.monitors.List(t.Context(), ports.MonitorFilter{UserID: f.userID})
	if err != nil {
		t.Fatalf("list monitors: %v", err)
	}
	out := map[string]*domain.Monitor{}
	for _, m := range list {
		out[m.Name] = m
	}
	return out
}

func (f restoreFixture) notificationsByName(t *testing.T) map[string]*domain.Notification {
	t.Helper()
	list, err := f.notifications.List(t.Context(), f.userID)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	out := map[string]*domain.Notification{}
	for _, n := range list {
		out[n.Name] = n
	}
	return out
}

// assertExactLinks asserts the monitor's link set is exactly the expected
// notification->include_target map: no extras (a leaked default channel), no
// missing links, and the per-link target flag preserved.
func assertExactLinks(t *testing.T, f restoreFixture, monitorID int64, want map[int64]bool) {
	t.Helper()
	links := f.linksOf(t, monitorID)
	if len(links) != len(want) {
		t.Fatalf("monitor %d has %d links, want %d: %+v", monitorID, len(links), len(want), links)
	}
	for _, link := range links {
		include, ok := want[link.NotificationID]
		if !ok {
			t.Fatalf("monitor %d has unexpected link to notification %d", monitorID, link.NotificationID)
		}
		if link.IncludeTarget != include {
			t.Fatalf("monitor %d link to notification %d include_target = %v, want %v",
				monitorID, link.NotificationID, link.IncludeTarget, include)
		}
	}
}

// TestBackupImportNotificationLinkFidelity restores a document through
// production BackupService.Import (monitor service carrying the production
// default notification linker) and asserts the exact restored link graph: a
// monitor with no links stays link-free despite default channels existing on
// both sides, include_target=false survives, and a non-default channel link
// restores the create default (include_target=true) and nothing else.
func TestBackupImportNotificationLinkFidelity(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newRestoreFixture(t, engine)
			ctx := t.Context()
			// A destination default channel that is NOT in the document. A
			// regression to create-with-defaults shows up as an extra link to it
			// (and to the document's own default channel) on restored monitors.
			destDefault := &domain.Notification{
				UserID: f.userID, Name: "dest-default", Type: "webhook", Active: true, IsDefault: true,
				Config: map[string]any{"url": "https://example.test/dest"},
			}
			if err := f.notifications.Create(ctx, destDefault); err != nil {
				t.Fatalf("seed destination default: %v", err)
			}
			noTarget := false
			doc := &services.BackupDocument{
				Version: services.BackupDocumentVersion,
				Notifications: []services.BackupNotification{
					{ID: 1, Name: "doc-default", Type: "webhook", Active: true, IsDefault: true, Config: map[string]any{"url": "https://example.test/doc-default"}},
					{ID: 2, Name: "doc-secondary", Type: "webhook", Active: true, Config: map[string]any{"url": "https://example.test/doc-secondary"}},
				},
				Monitors: []services.BackupMonitor{
					{ID: 11, Name: "m-no-links", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{"url": "http://example.test"}},
					{ID: 12, Name: "m-false-target", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{"url": "http://example.test"}},
					{ID: 13, Name: "m-nondefault", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{"url": "http://example.test"}},
				},
				MonitorNotifications: []services.BackupMonitorNotification{
					{MonitorID: 12, NotificationID: 1, IncludeTarget: &noTarget},
					{MonitorID: 13, NotificationID: 2},
				},
			}
			summary, err := f.backup.Import(ctx, f.userID, doc)
			if err != nil || summary.MonitorsCreated != 3 || summary.NotificationsCreated != 2 ||
				summary.MonitorNotificationsCreated != 2 || len(summary.Skipped) != 0 {
				t.Fatalf("import: summary=%+v err=%v", summary, err)
			}
			mons := f.monitorsByName(t)
			notifs := f.notificationsByName(t)
			// Deliberately empty link set: no link at all — not to the
			// destination default and not to the document's default channel.
			if links := f.linksOf(t, mons["m-no-links"].ID); len(links) != 0 {
				t.Fatalf("m-no-links restored %d links, want 0: %+v", len(links), links)
			}
			// include_target=false restores exactly, not reset to the default.
			assertExactLinks(t, f, mons["m-false-target"].ID, map[int64]bool{notifs["doc-default"].ID: false})
			// A non-default channel link restores the create default
			// (include_target=true) and nothing else.
			assertExactLinks(t, f, mons["m-nondefault"].ID, map[int64]bool{notifs["doc-secondary"].ID: true})
		})
	}
}
