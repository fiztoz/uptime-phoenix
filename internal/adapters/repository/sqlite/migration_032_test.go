package sqlite

import (
	"database/sql"
	"os"
	"testing"
)

// Migration 032 adds a new privilege. Existing users must stay least-privileged
// (default 0); there is no backfill because this is not preserving old behavior.
func TestMigration032_DefaultsExistingAndNewUsersOff(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		can_view_extensions INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatalf("create pre-032 users table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, is_admin) VALUES
		(1, 'existing-member', 0),
		(2, 'existing-admin', 1)`); err != nil {
		t.Fatalf("seed pre-032 users: %v", err)
	}

	up, err := os.ReadFile("migrations/032_user_view_all_monitors.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	if _, err := db.Exec(string(up)); err != nil {
		t.Fatalf("execute up migration: %v", err)
	}
	if !tableColumns(t, db, "users")["can_view_all_monitors"] {
		t.Fatal("users.can_view_all_monitors missing after migration 032")
	}

	var existingMember, existingAdmin int
	if err := db.QueryRow(`SELECT
		MAX(CASE WHEN id = 1 THEN can_view_all_monitors END),
		MAX(CASE WHEN id = 2 THEN can_view_all_monitors END)
		FROM users`).Scan(&existingMember, &existingAdmin); err != nil {
		t.Fatalf("read existing users: %v", err)
	}
	if existingMember != 0 {
		t.Errorf("existing non-admin can_view_all_monitors = %d; want 0", existingMember)
	}
	if existingAdmin != 0 {
		t.Errorf("existing admin raw can_view_all_monitors = %d; want 0 because admin access is implicit", existingAdmin)
	}

	if _, err := db.Exec(`INSERT INTO users (id, username, is_admin) VALUES (3, 'new-member', 0)`); err != nil {
		t.Fatalf("insert post-032 user: %v", err)
	}
	var newMember int
	if err := db.QueryRow(`SELECT can_view_all_monitors FROM users WHERE id = 3`).Scan(&newMember); err != nil {
		t.Fatalf("read new user default: %v", err)
	}
	if newMember != 0 {
		t.Errorf("new non-admin can_view_all_monitors default = %d; want 0", newMember)
	}
}
