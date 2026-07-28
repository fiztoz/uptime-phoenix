package sqlite

// Migration 011 against a real SQLite database.
//
// The rest of the suite exercises the repositories through Bun, which means it
// only ever sees a schema that migrations already built successfully — it cannot
// tell you that the migration SQL itself is right. These two tests run the real
// files and assert the two properties an upgrade depends on, both of which are
// invisible to every other test in the repo:
//
//   - the column DEFAULTS. include_descendants must default to 1, or every grant
//     written before 011 silently becomes shallow the moment someone upgrades and
//     users quietly lose sight of monitors. Nothing else asserts this: the Go code
//     always writes the column explicitly, so a wrong default would only ever
//     surface on real, pre-existing rows.
//   - the DOWN migration actually executing. It rebuilds two tables by hand
//     (SQLite's DROP COLUMN is too new to rely on), which is the kind of SQL that
//     is either exactly right or silently destructive.

import (
	"database/sql"
	"os"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db, "sqlite"); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return db
}

func tableColumns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[n] = true
	}
	return out
}

// The up migration adds what the Bun models expect, and defaults it the way an
// in-place upgrade needs.
func TestMigration011_UpAddsColumnsWithUpgradeSafeDefaults(t *testing.T) {
	db := migratedDB(t)

	userCols := tableColumns(t, db, "users")
	for _, c := range []string{"can_create_monitors", "can_create_groups"} {
		if !userCols[c] {
			t.Errorf("users.%s missing after migrations", c)
		}
	}
	if !tableColumns(t, db, "user_permissions")["include_descendants"] {
		t.Error("user_permissions.include_descendants missing after migrations")
	}

	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash) VALUES (1, 'u', 'h')`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO monitor_groups (id, user_id, name) VALUES (1, 1, 'g')`); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	// Insert a grant WITHOUT naming include_descendants — the shape a row written
	// before 011 has. It must read back deep.
	if _, err := db.Exec(`INSERT INTO user_permissions (user_id, group_id) VALUES (1, 1)`); err != nil {
		t.Fatalf("legacy-shaped grant insert: %v", err)
	}
	var deep int
	if err := db.QueryRow(`SELECT include_descendants FROM user_permissions WHERE user_id = 1`).Scan(&deep); err != nil {
		t.Fatalf("read back grant: %v", err)
	}
	if deep != 1 {
		t.Error("include_descendants defaulted to 0; every group grant predating this migration would silently stop covering subfolders")
	}

	// The capabilities default to off: migrating grants nobody anything.
	var canMonitors, canGroups int
	if err := db.QueryRow(`SELECT can_create_monitors, can_create_groups FROM users WHERE id = 1`).
		Scan(&canMonitors, &canGroups); err != nil {
		t.Fatalf("read capabilities: %v", err)
	}
	if canMonitors != 0 || canGroups != 0 {
		t.Errorf("create capabilities defaulted to (%d, %d); want (0, 0) — migrating must not hand out powers", canMonitors, canGroups)
	}
}

// The down migration runs, drops only what 011 added, and carries the existing
// rows and indexes through its table rebuilds.
func TestMigration011_DownRebuildsWithoutLosingData(t *testing.T) {
	db := migratedDB(t)

	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, can_manage_maintenance, can_create_monitors)
		VALUES (1, 'keeper', 'h', 1, 1, 1)`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO monitor_groups (id, user_id, name) VALUES (1, 1, 'g')`); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO user_permissions (id, user_id, group_id, include_descendants) VALUES (7, 1, 1, 0)`); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	down, err := os.ReadFile("migrations/011_create_capabilities.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	if _, err := db.Exec(string(down)); err != nil {
		t.Fatalf("the down migration does not execute: %v", err)
	}

	cols := tableColumns(t, db, "users")
	for _, c := range []string{"can_create_monitors", "can_create_groups"} {
		if cols[c] {
			t.Errorf("users.%s survived the down migration", c)
		}
	}
	// The rebuild must not take earlier migrations' columns with it.
	for _, c := range []string{"is_admin", "can_manage_notifications", "can_manage_maintenance"} {
		if !cols[c] {
			t.Errorf("the down migration dropped users.%s, which belongs to migration 006/008", c)
		}
	}
	if tableColumns(t, db, "user_permissions")["include_descendants"] {
		t.Error("user_permissions.include_descendants survived the down migration")
	}

	var username string
	var isAdmin, canMaint int
	if err := db.QueryRow(`SELECT username, is_admin, can_manage_maintenance FROM users WHERE id = 1`).
		Scan(&username, &isAdmin, &canMaint); err != nil {
		t.Fatalf("the user did not survive the table rebuild: %v", err)
	}
	if username != "keeper" || isAdmin != 1 || canMaint != 1 {
		t.Errorf("user came back as (%q, %d, %d); want (keeper, 1, 1)", username, isAdmin, canMaint)
	}

	var grantUser, grantGroup int64
	if err := db.QueryRow(`SELECT user_id, group_id FROM user_permissions WHERE id = 7`).
		Scan(&grantUser, &grantGroup); err != nil {
		t.Fatalf("the grant did not survive the table rebuild (id included): %v", err)
	}
	if grantUser != 1 || grantGroup != 1 {
		t.Errorf("grant came back as (%d, %d); want (1, 1)", grantUser, grantGroup)
	}

	// Rebuilding a table drops its indexes with it. If the down forgets to
	// recreate them, Grant stops being idempotent on a downgraded install — a
	// duplicate would be accepted instead of collapsing into the existing row.
	if _, err := db.Exec(`INSERT INTO user_permissions (user_id, group_id) VALUES (1, 1)`); err == nil {
		t.Error("a duplicate grant was accepted after the down migration; the UNIQUE index was not restored")
	}
}
