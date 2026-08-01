package repository_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type repositorySet struct {
	users                 ports.UserRepository
	monitors              ports.MonitorRepository
	heartbeats            ports.HeartbeatRepository
	notifications         ports.NotificationRepository
	monitorNotifications  ports.MonitorNotificationRepository
	maintenance           ports.MaintenanceRepository
	statusPages           ports.StatusPageRepository
	statusPageMonitors    ports.StatusPageMonitorRepository
	monitorGroups         ports.MonitorGroupRepository
	userPermissions       ports.UserPermissionRepository
	alerts                ports.AlertRepository
	escalationPolicies    ports.EscalationPolicyRepository
	escalationAssignments ports.EscalationAssignmentRepository
	alertEscalations      ports.AlertEscalationRepository
}

type repositoryFactory func(*testing.T) repositorySet

func sqliteFactory(t *testing.T) repositorySet {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "matrix.db") + "?cache=shared"
	db, err := sqlite.NewDB(dsn)
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db.DB, "sqlite"); err != nil {
		t.Fatalf("run SQLite migrations: %v", err)
	}
	return sqliteRepositorySet(sqlite.NewRepository(db))
}

func mariadbFactory(t *testing.T) repositorySet {
	t.Helper()
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if dsn == "" {
		t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB repository matrix")
	}
	validateMariaDBDSN(t, dsn)
	db, err := mariadb.NewDB(dsn)
	if err != nil {
		t.Fatalf("open MariaDB from TEST_MARIADB_DSN: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db.DB, "mariadb"); err != nil {
		t.Fatalf("run MariaDB migrations: %v", err)
	}
	resetMariaDB(t, db.DB)
	return mariadbRepositorySet(mariadb.NewRepository(db))
}

func sqliteRepositorySet(repo *sqlite.Repository) repositorySet {
	return repositorySet{
		users: repo.UserRepo, monitors: repo.MonitorRepo, heartbeats: repo.HeartbeatRepo,
		notifications: repo.NotificationRepo, monitorNotifications: repo.MonitorNotificationRepo,
		maintenance: repo.MaintenanceRepo, statusPages: repo.StatusPageRepo,
		statusPageMonitors: repo.StatusPageMonitorRepo, monitorGroups: repo.MonitorGroupRepo,
		userPermissions: repo.UserPermissionRepo,
		alerts:          repo.AlertRepo, escalationPolicies: repo.EscalationPolicyRepo,
		escalationAssignments: repo.EscalationAssignmentRepo, alertEscalations: repo.AlertEscalationRepo,
	}
}

func mariadbRepositorySet(repo *mariadb.Repository) repositorySet {
	return repositorySet{
		users: repo.UserRepo, monitors: repo.MonitorRepo, heartbeats: repo.HeartbeatRepo,
		notifications: repo.NotificationRepo, monitorNotifications: repo.MonitorNotificationRepo,
		maintenance: repo.MaintenanceRepo, statusPages: repo.StatusPageRepo,
		statusPageMonitors: repo.StatusPageMonitorRepo, monitorGroups: repo.MonitorGroupRepo,
		userPermissions: repo.UserPermissionRepo,
		alerts:          repo.AlertRepo, escalationPolicies: repo.EscalationPolicyRepo,
		escalationAssignments: repo.EscalationAssignmentRepo, alertEscalations: repo.AlertEscalationRepo,
	}
}

// resetMariaDB gives every matrix subtest an empty schema without dropping the
// database named by TEST_MARIADB_DSN. The CI service database is dedicated to
// this test process; the development database must never be used here.
func resetMariaDB(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("reserve MariaDB reset connection: %v", err)
	}
	defer func() { _ = conn.Close() }()
	rows, err := conn.QueryContext(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name <> '_migrations'`)
	if err != nil {
		t.Fatalf("list MariaDB tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			_ = rows.Close()
			t.Fatalf("scan MariaDB table: %v", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close MariaDB table rows: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate MariaDB tables: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("disable MariaDB foreign keys: %v", err)
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
			t.Errorf("re-enable MariaDB foreign keys: %v", err)
		}
	}()
	for _, table := range tables {
		quoted := "`" + strings.ReplaceAll(table, "`", "``") + "`"
		if _, err := conn.ExecContext(ctx, "TRUNCATE TABLE "+quoted); err != nil {
			t.Fatalf("truncate MariaDB table %s: %v", table, err)
		}
	}
}

func TestRepositoryContract_SQLite(t *testing.T) {
	runRepositoryContract(t, sqliteFactory)
}

func TestRepositoryContract_MariaDB(t *testing.T) {
	if os.Getenv("TEST_MARIADB_DSN") == "" {
		t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB repository matrix")
	}
	runRepositoryContract(t, mariadbFactory)
}

func runRepositoryContract(t *testing.T, factory repositoryFactory) {
	t.Helper()
	t.Run("CRUDAndFiltering", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "matrix-user")
		monitor := createMonitor(t, ctx, repos, user.ID, "Matrix HTTP")

		got, err := repos.monitors.GetByID(ctx, monitor.ID)
		if err != nil {
			t.Fatalf("GetByID monitor: %v", err)
		}
		if got.Name != monitor.Name || got.UserID != user.ID || got.Owner != monitor.Owner {
			t.Fatalf("monitor round trip = %+v; want name %q user %d owner %q", got, monitor.Name, user.ID, monitor.Owner)
		}
		active := true
		listed, err := repos.monitors.List(ctx, ports.MonitorFilter{UserID: user.ID, Active: &active, Search: "Matrix"})
		if err != nil {
			t.Fatalf("List monitors: %v", err)
		}
		if len(listed) != 1 || listed[0].ID != monitor.ID {
			t.Fatalf("filtered monitors = %v; want only %d", monitorIDs(listed), monitor.ID)
		}

		monitor.Name = "Matrix HTTP Updated"
		if err := repos.monitors.Update(ctx, monitor); err != nil {
			t.Fatalf("Update monitor: %v", err)
		}
		updated, err := repos.monitors.GetByID(ctx, monitor.ID)
		if err != nil || updated.Name != monitor.Name {
			t.Fatalf("updated monitor = %+v, %v; want name %q", updated, err, monitor.Name)
		}
		if err := repos.monitors.Delete(ctx, monitor.ID); err != nil {
			t.Fatalf("Delete monitor: %v", err)
		}
		if _, err := repos.monitors.GetByID(ctx, monitor.ID); err != ports.ErrNotFound {
			t.Fatalf("GetByID after delete = %v; want ErrNotFound", err)
		}
	})

	t.Run("TestMariaDBRegression_ZeroTimeInsert", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "zero-time-user")
		mw := &domain.MaintenanceWindow{
			UserID: user.ID, Title: "Nightly", Description: "cron without fixed dates",
			Active: true, Strategy: "cron", CronExpr: "0 2 * * *", Duration: 30,
		}
		if err := repos.maintenance.Create(ctx, mw); err != nil {
			t.Fatalf("Create cron maintenance with zero dates: %v", err)
		}
		got, err := repos.maintenance.GetByID(ctx, mw.ID)
		if err != nil {
			t.Fatalf("GetByID maintenance: %v", err)
		}
		if !got.StartDate.IsZero() || !got.EndDate.IsZero() {
			t.Fatalf("cron dates = %v..%v; want database NULLs mapped to zero values", got.StartDate, got.EndDate)
		}
	})

	t.Run("TestMariaDBRegression_HeartbeatLatestTimestampTie", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "heartbeat-tie-user")
		monitor := createMonitor(t, ctx, repos, user.ID, "Heartbeat Tie")
		tie := time.Now().UTC().Truncate(time.Second)
		for _, status := range []domain.Status{domain.StatusPending, domain.StatusDown} {
			if err := repos.heartbeats.Save(ctx, &domain.Heartbeat{MonitorID: monitor.ID, Status: status, Time: tie}); err != nil {
				t.Fatalf("Save heartbeat %s: %v", status, err)
			}
		}
		latest, err := repos.heartbeats.GetLatest(ctx, monitor.ID)
		if err != nil {
			t.Fatalf("GetLatest: %v", err)
		}
		if latest.Status != domain.StatusDown {
			t.Fatalf("latest status = %s; want DOWN from the later row sharing the timestamp", latest.Status)
		}
		ordered, err := repos.heartbeats.ListByMonitor(ctx, monitor.ID, tie.Add(-time.Second), tie.Add(time.Second))
		if err != nil {
			t.Fatalf("ListByMonitor: %v", err)
		}
		if len(ordered) != 2 || ordered[0].Status != domain.StatusPending || ordered[1].Status != domain.StatusDown {
			t.Fatalf("same-second order = %v; want [PENDING DOWN]", heartbeatStatuses(ordered))
		}
	})

	// The batched lookup that replaced the hub's per-monitor N+1 (Sprint D / R3.6)
	// has to answer EXACTLY what GetLatest answers, monitor for monitor. The
	// dangerous half is the tie: heartbeats.time is second-precision on MariaDB, so
	// a retry PENDING and the DOWN confirming it share a timestamp, and a batch
	// query built on MAX(time) or a naive GROUP BY silently returns the PENDING.
	// That would put every tied monitor on the dashboard into the wrong state at
	// once — strictly worse than the original single-row bug.
	//
	// This runs under the MariaDB contract too, which is the only place the tie is
	// real; SQLite stores timestamps precisely enough that it must be constructed.
	t.Run("TestMariaDBRegression_HeartbeatBatchLatestTimestampTie", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		batch, ok := repos.heartbeats.(ports.HeartbeatBatchReader)
		if !ok {
			t.Fatalf("heartbeat repository %T does not implement ports.HeartbeatBatchReader", repos.heartbeats)
		}
		user := createUser(t, ctx, repos, "heartbeat-batch-tie-user")

		// tied: two heartbeats sharing one second, DOWN written last.
		// single: one heartbeat only. bare: no heartbeats at all.
		tied := createMonitor(t, ctx, repos, user.ID, "Batch Tie")
		single := createMonitor(t, ctx, repos, user.ID, "Batch Single")
		bare := createMonitor(t, ctx, repos, user.ID, "Batch Bare")

		tie := time.Now().UTC().Truncate(time.Second)
		for _, status := range []domain.Status{domain.StatusPending, domain.StatusDown} {
			if err := repos.heartbeats.Save(ctx, &domain.Heartbeat{MonitorID: tied.ID, Status: status, Time: tie}); err != nil {
				t.Fatalf("Save tied heartbeat %s: %v", status, err)
			}
		}
		if err := repos.heartbeats.Save(ctx, &domain.Heartbeat{MonitorID: single.ID, Status: domain.StatusUp, Time: tie}); err != nil {
			t.Fatalf("Save single heartbeat: %v", err)
		}

		got, err := batch.GetLatestForMonitors(ctx, []int64{tied.ID, single.ID, bare.ID})
		if err != nil {
			t.Fatalf("GetLatestForMonitors: %v", err)
		}

		if got[tied.ID] == nil || got[tied.ID].Status != domain.StatusDown {
			t.Fatalf("batched latest for tied monitor = %v; want DOWN from the later row sharing the timestamp", got[tied.ID])
		}
		if got[single.ID] == nil || got[single.ID].Status != domain.StatusUp {
			t.Fatalf("batched latest for single-heartbeat monitor = %v; want UP", got[single.ID])
		}
		if _, present := got[bare.ID]; present {
			t.Fatalf("monitor with no heartbeats must be absent from the map, got %v", got[bare.ID])
		}

		// The batch must agree with GetLatest row-for-row, not merely on status.
		for _, id := range []int64{tied.ID, single.ID} {
			want, gErr := repos.heartbeats.GetLatest(ctx, id)
			if gErr != nil {
				t.Fatalf("GetLatest(%d): %v", id, gErr)
			}
			if got[id].ID != want.ID {
				t.Fatalf("batch returned heartbeat id %d for monitor %d; GetLatest returned %d", got[id].ID, id, want.ID)
			}
		}

		// An empty id list must not error or return a nil map.
		empty, err := batch.GetLatestForMonitors(ctx, nil)
		if err != nil {
			t.Fatalf("GetLatestForMonitors(nil): %v", err)
		}
		if empty == nil || len(empty) != 0 {
			t.Fatalf("GetLatestForMonitors(nil) = %v; want empty non-nil map", empty)
		}
	})

	t.Run("TestMariaDBRegression_GetByMonitorIDJoin", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "notification-join-user")
		monitor := createMonitor(t, ctx, repos, user.ID, "Notification Join")
		linked := &domain.Notification{UserID: user.ID, Name: "Linked", Type: "webhook", Active: true, Config: map[string]any{"url": "https://example.test/linked"}}
		unlinked := &domain.Notification{UserID: user.ID, Name: "Unlinked", Type: "webhook", Active: true, Config: map[string]any{"url": "https://example.test/unlinked"}}
		for _, notification := range []*domain.Notification{linked, unlinked} {
			if err := repos.notifications.Create(ctx, notification); err != nil {
				t.Fatalf("Create notification %q: %v", notification.Name, err)
			}
		}
		if err := repos.monitorNotifications.Attach(ctx, monitor.ID, linked.ID); err != nil {
			t.Fatalf("Attach notification: %v", err)
		}
		got, err := repos.notifications.GetByMonitorID(ctx, monitor.ID)
		if err != nil {
			t.Fatalf("GetByMonitorID: %v", err)
		}
		if len(got) != 1 || got[0].ID != linked.ID {
			t.Fatalf("GetByMonitorID = %v; want only linked notification %d", notificationIDs(got), linked.ID)
		}
	})

	t.Run("TestMariaDBRegression_ReorderMonitorsObservableOrder", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "reorder-user")
		slaTarget := 99.95
		sp := &domain.StatusPage{
			Slug: "matrix-status", Title: "Matrix Status", Theme: "auto", Published: true, SLATarget: &slaTarget,
		}
		if err := repos.statusPages.Create(ctx, sp); err != nil {
			t.Fatalf("Create status page: %v", err)
		}
		persisted, err := repos.statusPages.GetByID(ctx, sp.ID)
		if err != nil {
			t.Fatalf("GetByID status page: %v", err)
		}
		if persisted.SLATarget == nil || *persisted.SLATarget != slaTarget {
			t.Fatalf("status page SLA target = %v; want %v", persisted.SLATarget, slaTarget)
		}
		ids := make([]int64, 0, 3)
		for _, name := range []string{"one", "two", "three"} {
			monitor := createMonitor(t, ctx, repos, user.ID, name)
			ids = append(ids, monitor.ID)
			if err := repos.statusPageMonitors.AddMonitor(ctx, sp.ID, monitor.ID, 1000); err != nil {
				t.Fatalf("AddMonitor %q: %v", name, err)
			}
		}
		want := []int64{ids[2], ids[0]}
		if err := repos.statusPageMonitors.ReorderMonitors(ctx, sp.ID, want); err != nil {
			t.Fatalf("ReorderMonitors: %v", err)
		}
		links, err := repos.statusPageMonitors.ListByStatusPage(ctx, sp.ID)
		if err != nil {
			t.Fatalf("ListByStatusPage: %v", err)
		}
		if len(links) != len(want) {
			t.Fatalf("assignments = %d; want %d after replace-set", len(links), len(want))
		}
		for i, link := range links {
			if link.MonitorID != want[i] || link.DisplayOrder != (i+1)*10 {
				t.Errorf("position %d = monitor %d order %d; want monitor %d order %d", i, link.MonitorID, link.DisplayOrder, want[i], (i+1)*10)
			}
		}
	})

	t.Run("GroupDeleteRehomesChildren", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "group-rehome-user")
		root := &domain.MonitorGroup{UserID: user.ID, Name: "Root", Condition: domain.GroupConditionWorstOfChildren}
		child := &domain.MonitorGroup{UserID: user.ID, Name: "Child", Condition: domain.GroupConditionWorstOfChildren}
		if err := repos.monitorGroups.Create(ctx, root); err != nil {
			t.Fatalf("Create root group: %v", err)
		}
		child.ParentID = &root.ID
		if err := repos.monitorGroups.Create(ctx, child); err != nil {
			t.Fatalf("Create child group: %v", err)
		}
		monitor := createMonitor(t, ctx, repos, user.ID, "Grouped")
		monitor.GroupID = &child.ID
		if err := repos.monitors.Update(ctx, monitor); err != nil {
			t.Fatalf("assign monitor to child group: %v", err)
		}
		if err := repos.monitorGroups.Delete(ctx, child.ID); err != nil {
			t.Fatalf("Delete child group: %v", err)
		}
		got, err := repos.monitors.GetByID(ctx, monitor.ID)
		if err != nil {
			t.Fatalf("Get monitor after group delete: %v", err)
		}
		if got.GroupID == nil || *got.GroupID != root.ID {
			t.Fatalf("monitor group after delete = %v; want root %d", got.GroupID, root.ID)
		}
	})

	t.Run("UserPermissionUpsert", func(t *testing.T) {
		repos := factory(t)
		ctx := context.Background()
		user := createUser(t, ctx, repos, "permission-user")
		group := &domain.MonitorGroup{UserID: user.ID, Name: "Permission Group", Condition: domain.GroupConditionWorstOfChildren}
		if err := repos.monitorGroups.Create(ctx, group); err != nil {
			t.Fatalf("Create group: %v", err)
		}
		for _, deep := range []bool{true, false} {
			if err := repos.userPermissions.Grant(ctx, &domain.UserPermission{UserID: user.ID, GroupID: &group.ID, IncludeDescendants: deep}); err != nil {
				t.Fatalf("Grant include_descendants=%v: %v", deep, err)
			}
		}
		grants, err := repos.userPermissions.ListByUser(ctx, user.ID)
		if err != nil {
			t.Fatalf("ListByUser grants: %v", err)
		}
		if len(grants) != 1 || grants[0].IncludeDescendants {
			t.Fatalf("grants = %+v; want one shallow upserted grant", grants)
		}
	})
}

func createUser(t *testing.T, ctx context.Context, repos repositorySet, username string) *domain.User {
	t.Helper()
	user := &domain.User{Username: username, PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repos.users.Create(ctx, user); err != nil {
		t.Fatalf("Create user %q: %v", username, err)
	}
	if user.ID == 0 || user.CreatedAt.IsZero() || user.UpdatedAt.IsZero() {
		t.Fatalf("created user missing generated fields: %+v", user)
	}
	return user
}

func createMonitor(t *testing.T, ctx context.Context, repos repositorySet, userID int64, name string) *domain.Monitor {
	t.Helper()
	monitor := &domain.Monitor{
		UserID: userID, Name: name, Owner: "Platform on-call", Type: "http", Active: true, Interval: 60,
		Timeout: 5, Config: map[string]any{"url": "https://example.test/health"},
		AcceptedStatusCodes: []string{"200-299"},
	}
	if err := repos.monitors.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor %q: %v", name, err)
	}
	if monitor.ID == 0 || monitor.CreatedAt.IsZero() || monitor.UpdatedAt.IsZero() {
		t.Fatalf("created monitor missing generated fields: %+v", monitor)
	}
	return monitor
}

func monitorIDs(monitors []*domain.Monitor) []int64 {
	ids := make([]int64, len(monitors))
	for i, monitor := range monitors {
		ids[i] = monitor.ID
	}
	return ids
}

func heartbeatStatuses(heartbeats []*domain.Heartbeat) []domain.Status {
	statuses := make([]domain.Status, len(heartbeats))
	for i, heartbeat := range heartbeats {
		statuses[i] = heartbeat.Status
	}
	return statuses
}

func notificationIDs(notifications []*domain.Notification) []int64 {
	ids := make([]int64, len(notifications))
	for i, notification := range notifications {
		ids[i] = notification.ID
	}
	return ids
}

func TestMariaDBDSNSafety(t *testing.T) {
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if dsn == "" {
		t.Skip("TEST_MARIADB_DSN is unset; skipping DSN safety check")
	}
	validateMariaDBDSN(t, dsn)
}

func validateMariaDBDSN(t *testing.T, dsn string) {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse TEST_MARIADB_DSN: %v", err)
	}
	if cfg.DBName == "phoenix" {
		t.Fatalf("TEST_MARIADB_DSN points at the development database phoenix; use a dedicated test database")
	}
	if cfg.DBName == "" {
		t.Fatalf("TEST_MARIADB_DSN does not name a database: %q", dsn)
	}
}
