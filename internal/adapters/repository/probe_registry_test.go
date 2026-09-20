package repository_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	probeRegistryID1 = "11111111-1111-4111-8111-111111111111"
	probeRegistryID2 = "22222222-2222-4222-8222-222222222222"
	probeRegistryID3 = "33333333-3333-4333-8333-333333333333"
)

type probeRegistryFixture struct {
	db             *bun.DB
	registry       ports.ProbeRegistryRepository
	assignments    ports.MonitorProbeAssignmentRepository
	commits        ports.RegionalCommitRepository
	localHeartbeat ports.LocalHeartbeatRecorder
	ingest         ports.ProbeIngestRepository
	incidents      ports.ProbeIncidentRepository
	deliveries     ports.ProbeDeliveryRepository
	projections    ports.MonitorHealthProjectionRepository
	engine         string
	dsn            string
}

func newProbeRegistryFixture(t *testing.T, engine string) probeRegistryFixture {
	t.Helper()
	var db *bun.DB
	var err error
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if engine == "sqlite" {
		dsn = "file:" + filepath.Join(t.TempDir(), "probes.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
		db, err = sqlite.NewDB(dsn)
	} else {
		if dsn == "" {
			t.Skip("TEST_MARIADB_DSN is unset; skipping real MariaDB probe registry contract")
		}
		validateMariaDBDSN(t, dsn)
		db, err = mariadb.NewDB(dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db.DB, engine); err != nil {
		t.Fatal(err)
	}
	if engine == "mariadb" {
		resetMariaDB(t, db.DB)
	}
	f := probeRegistryFixture{db: db, engine: engine, dsn: dsn}
	if engine == "sqlite" {
		f.registry = sqlite.NewProbeRegistryRepo(db)
		f.assignments = sqlite.NewProbeAssignmentRepo(db)
		commits := sqlite.NewRegionalCommitRepo(db)
		f.localHeartbeat = commits
		f.commits, f.ingest, f.incidents, f.deliveries, f.projections = commits, commits, commits, commits, commits
	} else {
		f.registry = mariadb.NewProbeRegistryRepo(db)
		f.assignments = mariadb.NewProbeAssignmentRepo(db)
		commits := mariadb.NewRegionalCommitRepo(db)
		f.localHeartbeat = commits
		f.commits, f.ingest, f.incidents, f.deliveries, f.projections = commits, commits, commits, commits, commits
	}
	return f
}

func runProbeRegistryMigration(t *testing.T, db *bun.DB, engine, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(engine, "migrations", "035_probe_registry."+direction+".sql"))
	if err != nil {
		return err
	}
	// These focused scripts contain line comments and ordinary statements only.
	// Strip comments so punctuation in their prose is not treated as SQL.
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			return err
		}
	}
	return nil
}

func (f probeRegistryFixture) user(t *testing.T) int64 {
	t.Helper()
	result, err := f.db.ExecContext(context.Background(),
		"INSERT INTO users (username, password_hash) VALUES (?, ?)",
		fmt.Sprintf("owner-%d", time.Now().UnixNano()), "x")
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f probeRegistryFixture) monitor(t *testing.T) int64 {
	t.Helper()
	result, err := f.db.ExecContext(context.Background(), "INSERT INTO monitors (name, type, config) VALUES (?, ?, ?)", "Legacy monitor", "http", `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f probeRegistryFixture) notification(t *testing.T, userID int64) int64 {
	t.Helper()
	result, err := f.db.ExecContext(context.Background(),
		"INSERT INTO notifications (user_id, name, type, active, is_default, config) VALUES (?, ?, ?, ?, ?, ?)",
		userID, "Test Notif", "webhook", true, false, `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f probeRegistryFixture) escalationPolicy(t *testing.T, userID int64) int64 {
	t.Helper()
	result, err := f.db.ExecContext(context.Background(),
		"INSERT INTO escalation_policies (user_id, name, description, enabled) VALUES (?, ?, ?, ?)",
		userID, "Test Policy", "Desc", true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f probeRegistryFixture) remote(t *testing.T, id, key string) domain.Probe {
	t.Helper()
	probe := domain.Probe{ID: id, Key: key, Kind: domain.ProbeKindRemote, Name: key, Enabled: true}
	if err := f.registry.Create(context.Background(), &probe); err != nil {
		t.Fatal(err)
	}
	return probe
}

func TestProbeRegistryContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("RegistryAndIdentity", func(t *testing.T) { testProbeRegistryIdentity(t, newProbeRegistryFixture(t, engine)) })
			t.Run("ReplacementAndTombstones", func(t *testing.T) { testProbeRegistryReplacement(t, newProbeRegistryFixture(t, engine)) })
			t.Run("ConcurrentReplacement", func(t *testing.T) { testProbeRegistryConcurrency(t, newProbeRegistryFixture(t, engine)) })
			t.Run("AtomicRollbackAndOverflow", func(t *testing.T) { testProbeRegistryRollback(t, newProbeRegistryFixture(t, engine)) })
			t.Run("LegacyBackfillAndDown", func(t *testing.T) { testProbeRegistryMigration(t, newProbeRegistryFixture(t, engine)) })
			t.Run("SchemaConstraints", func(t *testing.T) { testProbeRegistryConstraints(t, newProbeRegistryFixture(t, engine)) })
		})
	}
}

func testProbeRegistryIdentity(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	local, err := f.registry.GetByID(ctx, domain.LocalProbeID)
	if err != nil || local.ID != "local" || local.Key != "local" || local.Kind != domain.ProbeKindLocal || !local.Enabled || local.Revision != 1 {
		t.Fatalf("local registration: %+v, %v", local, err)
	}
	if err := f.registry.Update(ctx, local, 1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("local mutation: %v", err)
	}
	remote := f.remote(t, probeRegistryID1, "asia-bangkok")
	if remote.Revision != 1 || remote.CreatedAt.IsZero() || remote.CreatedAt.Location() != time.UTC {
		t.Fatalf("new registration: %+v", remote)
	}
	duplicate := remote
	duplicate.ID = probeRegistryID2
	if err := f.registry.Create(ctx, &duplicate); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("duplicate key: %v", err)
	}
	for name, invalid := range map[string]domain.Probe{
		"reserved key":   {ID: probeRegistryID2, Key: "local", Kind: domain.ProbeKindRemote, Name: "reserved"},
		"bad UUID":       {ID: "not-a-uuid", Key: "other", Kind: domain.ProbeKindRemote, Name: "invalid"},
		"uppercase UUID": {ID: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", Key: "other", Kind: domain.ProbeKindRemote, Name: "invalid"},
		"nil UUID":       {ID: "00000000-0000-0000-0000-000000000000", Key: "other", Kind: domain.ProbeKindRemote, Name: "invalid"},
		"bad key":        {ID: probeRegistryID2, Key: "Mixed_Case", Kind: domain.ProbeKindRemote, Name: "invalid"},
		"empty name":     {ID: probeRegistryID2, Key: "other", Kind: domain.ProbeKindRemote},
	} {
		if err := f.registry.Create(ctx, &invalid); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	remote.Name, remote.Location = "Bangkok", "TH"
	if err := f.registry.Update(ctx, &remote, 1); err != nil || remote.Revision != 2 {
		t.Fatalf("update registration: %+v, %v", remote, err)
	}
	if err := f.registry.Update(ctx, &remote, 1); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale registration update: %v", err)
	}
	remote.Key = "renamed"
	if err := f.registry.Update(ctx, &remote, 2); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("identity mutation: %v", err)
	}
	all, err := f.registry.List(ctx)
	if err != nil || len(all) != 2 || all[0].Key != "asia-bangkok" || all[1].ID != "local" {
		t.Fatalf("deterministic registration list: %+v, %v", all, err)
	}
}

func testProbeRegistryReplacement(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitorID := f.monitor(t)
	if _, err := f.assignments.GetByMonitorID(ctx, monitorID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unwired creation must not silently initialize: %v", err)
	}
	initial, err := f.assignments.InitializeLocal(ctx, monitorID)
	if err != nil || initial.Revision != 1 || len(initial.Assignments) != 1 || initial.Assignments[0].ProbeID != "local" {
		t.Fatalf("initialize: %+v, %v", initial, err)
	}
	remote := f.remote(t, probeRegistryID1, "asia")
	updated, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local", remote.ID}, domain.HealthPolicyAllDown)
	if err != nil || updated.Revision != 2 || len(updated.Assignments) != 2 {
		t.Fatalf("replace: %+v, %v", updated, err)
	}
	unchanged, err := f.assignments.Replace(ctx, monitorID, 2, []string{remote.ID, "local"}, domain.HealthPolicyAllDown)
	if err != nil || !reflect.DeepEqual(updated, unchanged) {
		t.Fatalf("no-op replacement changed desired state: %+v / %+v, %v", updated, unchanged, err)
	}
	again, err := f.assignments.InitializeLocal(ctx, monitorID)
	if err != nil || !reflect.DeepEqual(updated, again) {
		t.Fatalf("reinitialization rewrote desired state: %+v, %v", again, err)
	}
	removed, err := f.assignments.Replace(ctx, monitorID, 2, []string{"local"}, domain.HealthPolicyAnyDown)
	if err != nil || removed.Revision != 3 {
		t.Fatalf("remove: %+v, %v", removed, err)
	}
	readded, err := f.assignments.Replace(ctx, monitorID, 3, []string{remote.ID, "local"}, domain.HealthPolicyAnyDown)
	if err != nil || readded.Revision != 4 || readded.Assignments[0].Generation != 2 || readded.Assignments[1].Generation != 1 {
		t.Fatalf("generation after re-add: %+v, %v", readded, err)
	}
	for _, ids := range [][]string{nil, {"local", "local"}, {probeRegistryID3}} {
		if _, err := f.assignments.Replace(ctx, monitorID, 4, ids, domain.HealthPolicyAnyDown); err == nil {
			t.Fatalf("invalid set accepted: %v", ids)
		}
	}
	if _, err := f.assignments.Replace(ctx, monitorID, 3, []string{"local"}, domain.HealthPolicyAnyDown); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale replacement: %v", err)
	}
	remote.Enabled = false
	if err := f.registry.Update(ctx, &remote, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(ctx, monitorID, 4, []string{remote.ID}, domain.HealthPolicyAnyDown); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("disabled probe accepted: %v", err)
	}
	after, err := f.assignments.GetByMonitorID(ctx, monitorID)
	if err != nil || !reflect.DeepEqual(readded, after) {
		t.Fatalf("failed validation changed set: %+v, %v", after, err)
	}
	if _, err := f.assignments.InitializeLocal(ctx, monitorID+1000); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("nonexistent monitor initialized: %v", err)
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM monitors WHERE id = ?", monitorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.GetByMonitorID(ctx, monitorID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("deleted monitor retained assignment: %v", err)
	}
}

func testProbeRegistryConcurrency(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitorID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
		t.Fatal(err)
	}
	f.remote(t, probeRegistryID1, "asia")
	other := f.assignments
	if f.engine == "sqlite" {
		// Two independent handles exercise SQLite's file locking, not just the
		// production pool's single-connection serialization within one process.
		peer, err := sqlite.NewDB(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = peer.Close() })
		other = sqlite.NewProbeAssignmentRepo(peer)
	}
	const writers = 8
	results := make(chan error, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range writers {
		store := f.assignments
		if i%2 == 1 {
			store = other
		}
		wg.Go(func() {
			<-start
			_, err := store.Replace(ctx, monitorID, 1, []string{"local", probeRegistryID1}, domain.HealthPolicyAnyDown)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ports.ErrConflict) {
			t.Errorf("concurrent replace: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful writers = %d, want 1", successes)
	}
	current, err := f.assignments.GetByMonitorID(ctx, monitorID)
	if err != nil || current.Revision != 2 || len(current.Assignments) != 2 {
		t.Fatalf("concurrent result: %+v, %v", current, err)
	}
	history := assignmentHistory(t, f, monitorID)
	if len(history) != 3 || history[0].To.IsZero() || history[1].Revision != 2 || history[2].Revision != 2 ||
		!history[0].To.Equal(history[1].From) || !history[1].From.Equal(history[2].From) {
		t.Fatalf("concurrent writers split/duplicated history: %+v", history)
	}
}

func testProbeRegistryRollback(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitorID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
		t.Fatal(err)
	}
	f.remote(t, probeRegistryID1, "asia")
	f.remote(t, probeRegistryID2, "europe")
	if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local", probeRegistryID2}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	before, err := f.assignments.Replace(ctx, monitorID, 2, []string{"local"}, domain.HealthPolicyAnyDown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE monitor_probe_assignments SET generation = ? WHERE monitor_id = ? AND probe_id = ?", math.MaxInt64, monitorID, probeRegistryID2); err != nil {
		t.Fatal(err)
	}
	// The sorted replacement inserts ID1, then encounters exhausted ID2.
	// Transaction rollback must undo the already inserted row and all metadata.
	if _, err := f.assignments.Replace(ctx, monitorID, 3, []string{probeRegistryID1, probeRegistryID2}, domain.HealthPolicyAllDown); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("generation overflow: %v", err)
	}
	after, err := f.assignments.GetByMonitorID(ctx, monitorID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("partial replacement leaked: %+v, %v", after, err)
	}
	var count int
	if err := f.db.NewSelect().Table("monitor_probe_assignments").ColumnExpr("COUNT(*)").Where("monitor_id = ? AND probe_id = ?", monitorID, probeRegistryID1).Scan(ctx, &count); err != nil || count != 0 {
		t.Fatalf("rolled-back insertion count = %d, %v", count, err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision = ? WHERE monitor_id = ?", math.MaxInt64, monitorID); err != nil {
		t.Fatal(err)
	}
	if same, err := f.assignments.Replace(ctx, monitorID, math.MaxInt64, []string{"local"}, domain.HealthPolicyAnyDown); err != nil || same.Revision != math.MaxInt64 {
		t.Fatalf("maximum revision no-op: %+v, %v", same, err)
	}
	if _, err := f.assignments.Replace(ctx, monitorID, math.MaxInt64, []string{"local"}, domain.HealthPolicyAllDown); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("revision overflow: %v", err)
	}
}

func testProbeRegistryMigration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	// Test 035 at its own schema boundary, not underneath later FK dependents.
	// Restore the latest schema for the shared MariaDB test database afterward.
	later := []string{"036_probe_regional", "037_probe_heartbeat", "038_probe_incidents", "039_probe_health_projection", "040_probe_assignment_history", "041_probe_auxiliary_state", "042_local_stream_sequence", "043_notification_throttles", "045_probe_delivery_outbox", "047_probe_config_snapshots", "048_probe_installation", "049_probe_activation", "050_delivery_cancellation", "051_escalation_delivery_context", "052_probe_connector_leases", "053_probe_connections"}
	for i := len(later) - 1; i >= 0; i-- {
		if err := runEngineMigration(t, f.db, f.engine, later[i], "down"); err != nil {
			t.Fatalf("downgrade dependency %s: %v", later[i], err)
		}
	}
	t.Cleanup(func() {
		for _, name := range later {
			if err := runEngineMigration(t, f.db, f.engine, name, "up"); err != nil {
				t.Errorf("restore dependency %s: %v", name, err)
				return
			}
		}
	})
	if err := runProbeRegistryMigration(t, f.db, f.engine, "down"); err != nil {
		t.Fatalf("safe empty downgrade: %v", err)
	}
	monitorID := f.monitor(t)
	// A baseline heartbeat survives additive migration and safe downgrade.
	if _, err := f.db.ExecContext(ctx, "INSERT INTO heartbeats (monitor_id, status, time, msg, ping, duration) VALUES (?, ?, ?, ?, ?, ?)", monitorID, 1, time.Now().UTC(), "legacy", 12, 12); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := runProbeRegistryMigration(t, f.db, f.engine, "up"); err != nil {
			t.Fatal(err)
		}
	}
	set, err := f.assignments.GetByMonitorID(ctx, monitorID)
	if err != nil || set.Revision != 1 || set.HealthPolicy != domain.HealthPolicyAnyDown || len(set.Assignments) != 1 || set.Assignments[0].Generation != 1 {
		t.Fatalf("legacy backfill: %+v, %v", set, err)
	}
	if err := runProbeRegistryMigration(t, f.db, f.engine, "down"); err != nil {
		t.Fatalf("safe local-only downgrade: %v", err)
	}
	var count int
	if err := f.db.NewSelect().Table("heartbeats").ColumnExpr("COUNT(*)").Where("monitor_id = ?", monitorID).Scan(ctx, &count); err != nil || count != 1 {
		t.Fatalf("legacy heartbeat lost: count=%d, %v", count, err)
	}
	if err := runProbeRegistryMigration(t, f.db, f.engine, "up"); err != nil {
		t.Fatal(err)
	}
	f.remote(t, probeRegistryID1, "asia")
	if err := runProbeRegistryMigration(t, f.db, f.engine, "down"); err == nil {
		t.Fatal("downgrade discarded remote registration")
	}
	if _, err := f.registry.GetByID(ctx, probeRegistryID1); err != nil {
		t.Fatalf("guard did not preserve registration: %v", err)
	}
	if _, err := f.assignments.GetByMonitorID(ctx, monitorID); err != nil {
		t.Fatalf("guard did not preserve assignment tables: %v", err)
	}
}

func testProbeRegistryConstraints(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	monitorID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"null probe id":         "INSERT INTO probes (id, probe_key, name, location, kind, enabled, revision, created_at, updated_at) VALUES (NULL, 'null-id', 'Bad', '', 'remote', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)",
		"reserved local alias":  fmt.Sprintf("INSERT INTO probes (id, probe_key, name, location, kind, enabled, revision, created_at, updated_at) VALUES ('%s', 'local', 'Bad', '', 'remote', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)", probeRegistryID1),
		"missing probe FK":      fmt.Sprintf("INSERT INTO monitor_probe_assignments (monitor_id, probe_id, generation, active, created_at, updated_at) VALUES (%d, '%s', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)", monitorID, probeRegistryID3),
		"zero generation":       fmt.Sprintf("UPDATE monitor_probe_assignments SET generation = 0 WHERE monitor_id = %d", monitorID),
		"zero revision":         fmt.Sprintf("UPDATE monitor_probe_assignment_sets SET revision = 0 WHERE monitor_id = %d", monitorID),
		"invalid policy":        fmt.Sprintf("UPDATE monitor_probe_assignment_sets SET health_policy = 'invalid' WHERE monitor_id = %d", monitorID),
		"out of range revision": fmt.Sprintf("UPDATE monitor_probe_assignment_sets SET revision = 9223372036854775808 WHERE monitor_id = %d", monitorID),
	} {
		if _, err := f.db.ExecContext(ctx, statement); err == nil {
			t.Errorf("schema accepted %s", name)
		}
	}
	if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local"}, domain.HealthPolicyAllDown); err != nil {
		t.Fatal(err)
	}
	if err := runProbeRegistryMigration(t, f.db, f.engine, "down"); err == nil {
		t.Fatal("downgrade discarded changed local policy")
	}
}
