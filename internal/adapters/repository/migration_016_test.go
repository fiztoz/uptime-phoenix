package repository_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
)

// Migration 016 must apply and reverse cleanly on BOTH engines. A down file that
// does not actually drop everything leaves an operator unable to roll back and
// then unable to roll forward again — the failure only shows up during an
// incident, which is the worst possible time to discover it.

var escalationTables = []string{
	"alert_escalations",
	"escalation_policy_groups",
	"escalation_policy_monitors",
	"escalation_step_notifications",
	"escalation_steps",
	"escalation_policies",
}

func TestMigration016_UpDownUp_SQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "mig016.db") + "?cache=shared"
	db, err := sqlite.NewDB(dsn)
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db.DB, "sqlite"); err != nil {
		t.Fatalf("run SQLite migrations: %v", err)
	}
	assertEscalationTables(t, db.DB, "sqlite", true)

	execMigrationFile(t, db.DB, "sqlite/migrations/016_escalation_policies.down.sql")
	assertEscalationTables(t, db.DB, "sqlite", false)

	execMigrationFile(t, db.DB, "sqlite/migrations/016_escalation_policies.up.sql")
	assertEscalationTables(t, db.DB, "sqlite", true)
}

func TestMigration016_UpDownUp_MariaDB(t *testing.T) {
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if dsn == "" {
		t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB migration 016 cycle")
	}
	validateMariaDBDSN(t, dsn)
	db, err := mariadb.NewDB(dsn)
	if err != nil {
		t.Fatalf("open MariaDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db.DB, "mariadb"); err != nil {
		t.Fatalf("run MariaDB migrations: %v", err)
	}
	// This test drops real tables. If it fails between the down and the re-up,
	// _migrations still records 016 as applied, so RunMigrations will skip it
	// forever and every sibling test in the same database starts failing for an
	// unrelated-looking reason. Put the schema back whatever happens.
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), "SELECT 1")
		if tablesPresent(db.DB, "mariadb") {
			return
		}
		execMigrationFile(t, db.DB, "mariadb/migrations/016_escalation_policies.up.sql")
	})
	assertEscalationTables(t, db.DB, "mariadb", true)

	execMigrationFile(t, db.DB, "mariadb/migrations/016_escalation_policies.down.sql")
	assertEscalationTables(t, db.DB, "mariadb", false)

	execMigrationFile(t, db.DB, "mariadb/migrations/016_escalation_policies.up.sql")
	assertEscalationTables(t, db.DB, "mariadb", true)
}

// execMigrationFile reads a migration from the repo tree (not the embedded FS,
// which only exposes .up.sql through RunMigrations) and runs its statements.
func execMigrationFile(t *testing.T, db *sql.DB, relPath string) {
	t.Helper()
	body, err := os.ReadFile(filepath.FromSlash(relPath))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	// Strip `--` line comments before splitting. Neither migration contains a
	// semicolon inside parentheses (no PARTITION clause), so a plain split is
	// sufficient once the prose is gone.
	var sql strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		sql.WriteString(line)
		sql.WriteByte('\n')
	}
	for _, stmt := range strings.Split(sql.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("exec %s statement %q: %v", relPath, firstLine(stmt), err)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func tableCountQuery(engine string) string {
	if engine == "sqlite" {
		return "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?"
	}
	return "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?"
}

func tablesPresent(db *sql.DB, engine string) bool {
	for _, table := range escalationTables {
		var n int
		if err := db.QueryRowContext(context.Background(), tableCountQuery(engine), table).Scan(&n); err != nil || n != 1 {
			return false
		}
	}
	return true
}

func assertEscalationTables(t *testing.T, db *sql.DB, engine string, want bool) {
	t.Helper()
	for _, table := range escalationTables {
		var n int
		if err := db.QueryRowContext(context.Background(), tableCountQuery(engine), table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if want && n != 1 {
			t.Fatalf("%s: table %s missing after up", engine, table)
		}
		if !want && n != 0 {
			t.Fatalf("%s: table %s survived the down migration", engine, table)
		}
	}
}
