package sqlite

import (
	"database/sql"
	"os"
	"testing"
)

// Migration 031 changes an existing behavior, not just a schema shape: users
// who could see extensions before the migration must keep that visibility,
// while accounts created after it must remain least-privileged by default.
func TestMigration031_BackfillsExistingMembersAndDefaultsNewUsersOff(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create pre-031 users table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, is_admin) VALUES
		(1, 'existing-member', 0),
		(2, 'existing-admin', 1)`); err != nil {
		t.Fatalf("seed pre-031 users: %v", err)
	}

	up, err := os.ReadFile("migrations/031_user_extension_visibility.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	if _, err := db.Exec(string(up)); err != nil {
		t.Fatalf("execute up migration: %v", err)
	}
	if !tableColumns(t, db, "users")["can_view_extensions"] {
		t.Fatal("users.can_view_extensions missing after migration 031")
	}

	var existingMember, existingAdmin int
	if err := db.QueryRow(`SELECT
		MAX(CASE WHEN id = 1 THEN can_view_extensions END),
		MAX(CASE WHEN id = 2 THEN can_view_extensions END)
		FROM users`).Scan(&existingMember, &existingAdmin); err != nil {
		t.Fatalf("read backfilled users: %v", err)
	}
	if existingMember != 1 {
		t.Errorf("existing non-admin can_view_extensions = %d; want 1 to preserve pre-031 visibility", existingMember)
	}
	if existingAdmin != 0 {
		t.Errorf("existing admin raw can_view_extensions = %d; want 0 because admin access is implicit", existingAdmin)
	}

	if _, err := db.Exec(`INSERT INTO users (id, username, is_admin) VALUES (3, 'new-member', 0)`); err != nil {
		t.Fatalf("insert post-031 user: %v", err)
	}
	var newMember int
	if err := db.QueryRow(`SELECT can_view_extensions FROM users WHERE id = 3`).Scan(&newMember); err != nil {
		t.Fatalf("read new user default: %v", err)
	}
	if newMember != 0 {
		t.Errorf("new non-admin can_view_extensions default = %d; want 0", newMember)
	}
}
