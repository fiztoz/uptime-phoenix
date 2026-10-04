package repository

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSplitMigrationStatements_partitions(t *testing.T) {
	sql := `
CREATE TABLE a (id INT);
CREATE TABLE heartbeats (
    id BIGINT,
    time TIMESTAMP,
    PRIMARY KEY (id, time)
) PARTITION BY RANGE (UNIX_TIMESTAMP(time)) (
    PARTITION p1 VALUES LESS THAN (100),
    PARTITION pmax VALUES LESS THAN MAXVALUE
);
CREATE TABLE b (id INT);
`
	stmts := splitMigrationStatements(sql)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3: %v", len(stmts), stmts)
	}
	if !strings.HasPrefix(stmts[0], "CREATE TABLE a") {
		t.Errorf("stmt0: %q", stmts[0])
	}
	if !strings.Contains(stmts[1], "PARTITION pmax") {
		t.Errorf("stmt1 missing partition: %q", stmts[1])
	}
	if !strings.HasPrefix(stmts[2], "CREATE TABLE b") {
		t.Errorf("stmt2: %q", stmts[2])
	}
}

func TestSQLiteMigrationRebuildRollsBackWithChildren(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "migration.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RunMigrations(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"INSERT INTO users (id, username, password_hash) VALUES (1, 'owner', 'test')",
		"INSERT INTO monitors (id, name, type, config) VALUES (1, 'test', 'http', '{}')",
		"INSERT INTO alerts (id, source_alert_id, monitor_id, status, message, fired_at, ack_token, open_monitor_id) VALUES (1, '11111111-1111-4111-8111-111111111111', 1, 'firing', 'down', CURRENT_TIMESTAMP, 'opaque-test-token', 1)",
		"INSERT INTO escalation_policies (id, user_id, name, description) VALUES (1, 1, 'test', '')",
		"INSERT INTO alert_escalations (id, alert_id, monitor_id, policy_id, next_step, next_run_at, status) VALUES (1, 1, 1, 1, 1, CURRENT_TIMESTAMP, 'pending')",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	for _, name := range []string{"044_probe_alert_scope.down.sql", "046_alert_source_identity.up.sql"} {
		source, err := sqliteMigrations.ReadFile("sqlite/migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		// Fail after rebuilding both parent and child, before recording the migration.
		err = applyMigration(ctx, db, "sqlite", "injected_failure.up.sql", string(source)+"; INSERT INTO nonexistent_failure_table VALUES (1);")
		if err == nil {
			t.Fatal("expected injected failure")
		}
		var probe, token, sourceID string
		if err := db.QueryRowContext(ctx, "SELECT probe_id, ack_token, source_alert_id FROM alerts WHERE id = 1").Scan(&probe, &token, &sourceID); err != nil || probe != "local" || token != "opaque-test-token" || sourceID != "11111111-1111-4111-8111-111111111111" {
			t.Fatalf("parent rollback failed: %v", err)
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM alert_escalations WHERE id = 1 AND alert_id = 1 AND status = 'pending'").Scan(&n); err != nil || n != 1 {
			t.Fatalf("child rollback failed: %v", err)
		}
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM _migrations WHERE filename = 'injected_failure.up.sql'").Scan(&n); err != nil || n != 0 {
			t.Fatal("failed migration was recorded")
		}
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM alerts WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM alert_escalations").Scan(&n); err != nil || n != 0 {
		t.Fatal("rollback lost foreign key")
	}
}
