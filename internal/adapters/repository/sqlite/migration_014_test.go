package sqlite

// Migration 014 against a real SQLite database.
//
// Asserts the load-bearing upgrade properties:
//   - dormant webhook rows survive under the explicit legacy table name
//   - the new email subscriber + channel tables accept the Sprint C shape
//   - the DOWN path restores the original webhook table with those legacy rows

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestMigration014_PreservesLegacyWebhookAndCreatesEmailTables(t *testing.T) {
	// Build a pre-014 schema by applying all migrations then replaying down
	// is awkward; instead: apply full chain (legacy table is empty), insert a
	// synthetic legacy webhook row, prove email table + channel work, then
	// run the real down SQL and prove the webhook table+row are restored.

	dbPath := filepath.Join(t.TempDir(), "m014.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if err := repository.RunMigrations(db, "sqlite"); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	mustExec(t, db, `
		INSERT INTO users (username, password_hash, is_admin, active, created_at, updated_at)
		VALUES ('admin', 'x', 1, 1, datetime('now'), datetime('now'))
	`)
	mustExec(t, db, `
		INSERT INTO status_pages (slug, title, theme, published, created_at, updated_at)
		VALUES ('m014', 'M014', 'light', 1, datetime('now'), datetime('now'))
	`)
	var spID int64
	if err := db.QueryRow(`SELECT id FROM status_pages WHERE slug = 'm014'`).Scan(&spID); err != nil {
		t.Fatalf("status page id: %v", err)
	}

	// Simulate an upgraded install that already had webhook subscribers:
	// insert into the legacy table created by 014's RENAME.
	mustExec(t, db, `
		INSERT INTO status_page_subscribers_legacy_webhook (status_page_id, url, active, secret, created_at)
		VALUES (?, 'https://hooks.example.com/x', 1, 'hmac-secret', datetime('now'))
	`, spID)

	var legacyURL, legacySecret string
	err = db.QueryRow(`
		SELECT url, secret FROM status_page_subscribers_legacy_webhook WHERE status_page_id = ?
	`, spID).Scan(&legacyURL, &legacySecret)
	if err != nil {
		t.Fatalf("legacy row missing: %v", err)
	}
	if legacyURL != "https://hooks.example.com/x" || legacySecret != "hmac-secret" {
		t.Fatalf("legacy row corrupted: url=%q secret=%q", legacyURL, legacySecret)
	}

	// Email table is independent of the legacy webhook table.
	mustExec(t, db, `
		INSERT INTO status_page_subscribers (status_page_id, email, active, created_at, updated_at)
		VALUES (?, 'ops@example.com', 0, datetime('now'), datetime('now'))
	`, spID)
	_, err = db.Exec(`
		INSERT INTO status_page_subscribers (status_page_id, email, active, created_at, updated_at)
		VALUES (?, 'ops@example.com', 0, datetime('now'), datetime('now'))
	`, spID)
	if err == nil {
		t.Fatal("duplicate page+email should fail unique constraint")
	}

	mustExec(t, db, `
		INSERT INTO notifications (user_id, name, type, active, is_default, config, created_at, updated_at)
		VALUES (1, 'SMTP', 'smtp', 1, 0, '{}', datetime('now'), datetime('now'))
	`)
	var notifID int64
	if err := db.QueryRow(`SELECT id FROM notifications WHERE name = 'SMTP'`).Scan(&notifID); err != nil {
		t.Fatalf("notif id: %v", err)
	}
	mustExec(t, db, `
		INSERT INTO status_page_subscription_channels (status_page_id, notification_id)
		VALUES (?, ?)
	`, spID, notifID)

	// Down path: drop email tables, restore webhook table name, keep legacy rows.
	// Note: email subscriber rows created after 014 are intentionally discarded.
	down, err := os.ReadFile("migrations/014_status_page_email_subscriptions.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	execStatements(t, db, string(down))

	var restoredURL string
	err = db.QueryRow(`
		SELECT url FROM status_page_subscribers WHERE status_page_id = ?
	`, spID).Scan(&restoredURL)
	if err != nil {
		t.Fatalf("webhook table not restored: %v", err)
	}
	if restoredURL != "https://hooks.example.com/x" {
		t.Fatalf("restored url = %q", restoredURL)
	}

	if tableExists(t, db, "status_page_subscription_channels") {
		t.Fatal("status_page_subscription_channels should be dropped by down")
	}
	if tableExists(t, db, "status_page_subscribers_legacy_webhook") {
		t.Fatal("legacy table name should be gone after rename-back")
	}
	cols := tableColumns(t, db, "status_page_subscribers")
	if !cols["url"] {
		t.Fatal("restored status_page_subscribers missing url column")
	}
	if cols["email"] {
		t.Fatal("restored status_page_subscribers must not have email column")
	}
}

func TestMigration014_EmailSubscriberRepoRoundTrip(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "sub-user", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	sp := &domain.StatusPage{Slug: "email-rt", Title: "Email RT", Theme: "light", Published: true}
	if err := repo.StatusPageRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create sp: %v", err)
	}

	sub := &domain.StatusPageSubscriber{
		StatusPageID: sp.ID,
		Email:        "a@b.co",
		Active:       false,
	}
	if err := repo.StatusPageSubscriberRepo.Create(ctx, sub); err != nil {
		t.Fatalf("create sub: %v", err)
	}
	if sub.ID == 0 || sub.CreatedAt.IsZero() || sub.UpdatedAt.IsZero() {
		t.Fatalf("id/timestamps not set: %+v", sub)
	}

	got, err := repo.StatusPageSubscriberRepo.GetByPageAndEmail(ctx, sp.ID, "a@b.co")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Active {
		t.Fatal("pending sub should not be active")
	}

	now := time.Now().UTC()
	got.Active = true
	got.ConfirmedAt = &now
	if err := repo.StatusPageSubscriberRepo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}

	confirmed, err := repo.StatusPageSubscriberRepo.ListConfirmedByStatusPage(ctx, sp.ID)
	if err != nil || len(confirmed) != 1 {
		t.Fatalf("list confirmed: %v len=%d", err, len(confirmed))
	}

	mon := &domain.Monitor{
		UserID: user.ID, Name: "m", Type: "http", Active: true, Interval: 60, Timeout: 30,
		Config: map[string]any{"url": "https://example.com"},
	}
	if err := repo.MonitorRepo.Create(ctx, mon); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	if err := repo.StatusPageMonitorRepo.AddMonitor(ctx, sp.ID, mon.ID, 10); err != nil {
		t.Fatalf("link monitor: %v", err)
	}
	ids, err := repo.StatusPageSubscriberRepo.ListStatusPageIDsForMonitors(ctx, []int64{mon.ID})
	if err != nil || len(ids) != 1 || ids[0] != sp.ID {
		t.Fatalf("list page ids: %v %v", err, ids)
	}
	empty, err := repo.StatusPageSubscriberRepo.ListStatusPageIDsForMonitors(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty monitors: %v %v", err, empty)
	}

	// Channel upsert.
	n := &domain.Notification{
		UserID: user.ID, Name: "SMTP", Type: "smtp", Active: true,
		Config: map[string]any{"host": "smtp.example.com", "from": "a@b.c", "port": float64(587)},
	}
	if err := repo.NotificationRepo.Create(ctx, n); err != nil {
		t.Fatalf("create notif: %v", err)
	}
	ch := &domain.StatusPageSubscriptionChannel{StatusPageID: sp.ID, NotificationID: n.ID}
	if err := repo.StatusPageSubscriberRepo.SetChannel(ctx, ch); err != nil {
		t.Fatalf("set channel: %v", err)
	}
	gotCh, err := repo.StatusPageSubscriberRepo.GetChannel(ctx, sp.ID)
	if err != nil || gotCh.NotificationID != n.ID {
		t.Fatalf("get channel: %+v err=%v", gotCh, err)
	}
	if err := repo.StatusPageSubscriberRepo.DeleteChannel(ctx, sp.ID); err != nil {
		t.Fatalf("delete channel: %v", err)
	}

	if err := repo.StatusPageSubscriberRepo.Delete(ctx, got.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.StatusPageSubscriberRepo.GetByID(ctx, got.ID); err == nil {
		t.Fatal("expected not found after delete")
	}
}

// --- helpers ---------------------------------------------------------------

func execStatements(t *testing.T, db *sql.DB, sqlText string) {
	t.Helper()
	parts := strings.Split(sqlText, ";")
	for _, p := range parts {
		stmt := strings.TrimSpace(p)
		if stmt == "" {
			continue
		}
		lines := strings.Split(stmt, "\n")
		var keep []string
		for _, line := range lines {
			trim := strings.TrimSpace(line)
			if trim == "" || strings.HasPrefix(trim, "--") {
				continue
			}
			keep = append(keep, line)
		}
		stmt = strings.TrimSpace(strings.Join(keep, "\n"))
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			preview := stmt
			if len(preview) > 80 {
				preview = preview[:80]
			}
			t.Fatalf("exec %q: %v", preview, err)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec: %v\nSQL: %s", err, query)
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return err == nil
}
