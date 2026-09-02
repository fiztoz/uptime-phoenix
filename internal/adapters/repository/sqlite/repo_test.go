package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func setupTestDB(t *testing.T) *Repository {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	sqldb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })

	if _, err := sqldb.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}

	if err := repository.RunMigrations(sqldb, "sqlite"); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	bunDB := bun.NewDB(sqldb, sqlitedialect.New())
	return NewRepository(bunDB)
}

func TestUserRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	// Create
	user := &domain.User{
		Username:     "testuser",
		PasswordHash: "hashed_password",
		Active:       true,
		IsAdmin:      true,
		Timezone:     "UTC",
	}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if user.ID == 0 {
		t.Fatal("expected user ID to be set")
	}

	// A second, non-admin user so List can be checked for ordering.
	second := &domain.User{
		Username:     "testuser2",
		PasswordHash: "hashed_password",
		Active:       true,
		Timezone:     "UTC",
	}
	if err := repo.UserRepo.Create(ctx, second); err != nil {
		t.Fatalf("Create (second user): %v", err)
	}

	// GetByID
	fetched, err := repo.UserRepo.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.Username != user.Username {
		t.Errorf("expected username %q, got %q", user.Username, fetched.Username)
	}
	if !fetched.IsAdmin {
		t.Errorf("expected IsAdmin true, got false")
	}
	if second.IsAdmin {
		t.Errorf("expected second user IsAdmin false (default), got true")
	}

	// List — ordered by id ascending.
	all, err := repo.UserRepo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("List: expected 2 users, got %d", len(all))
	}
	if all[0].ID != user.ID || all[1].ID != second.ID {
		t.Errorf("List not ordered by id ascending: got ids %d, %d; want %d, %d",
			all[0].ID, all[1].ID, user.ID, second.ID)
	}
	if !all[0].IsAdmin || all[1].IsAdmin {
		t.Errorf("List did not preserve IsAdmin: got [%v, %v]; want [true, false]", all[0].IsAdmin, all[1].IsAdmin)
	}

	// GetByUsername
	fetched2, err := repo.UserRepo.GetByUsername(ctx, user.Username)
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if fetched2.ID != user.ID {
		t.Errorf("expected ID %d, got %d", user.ID, fetched2.ID)
	}

	// Update
	user.Timezone = "America/New_York"
	if updateErr := repo.UserRepo.Update(ctx, user); updateErr != nil {
		t.Fatalf("Update: %v", updateErr)
	}
	fetched3, err := repo.UserRepo.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if fetched3.Timezone != "America/New_York" {
		t.Errorf("expected timezone America/New_York, got %q", fetched3.Timezone)
	}

	// Delete
	if delErr := repo.UserRepo.Delete(ctx, user.ID); delErr != nil {
		t.Fatalf("Delete: %v", delErr)
	}
	_, err = repo.UserRepo.GetByID(ctx, user.ID)
	if err != ports.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestMonitorRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	// Create user first (for foreign key)
	user := &domain.User{
		Username:     "monitoruser",
		PasswordHash: "hash",
		Active:       true,
	}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}

	// Create monitor
	monitor := &domain.Monitor{
		UserID:              user.ID,
		Name:                "Test Monitor",
		Type:                "http",
		Active:              true,
		Interval:            60,
		RetryInterval:       5,
		MaxRetries:          3,
		Timeout:             30.0,
		Config:              map[string]any{"url": "https://example.com"},
		AcceptedStatusCodes: []string{"200-299"},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}
	if monitor.ID == 0 {
		t.Fatal("expected monitor ID to be set")
	}

	// GetByID
	fetched, err := repo.MonitorRepo.GetByID(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.Name != monitor.Name {
		t.Errorf("expected name %q, got %q", monitor.Name, fetched.Name)
	}

	// List
	monitors, err := repo.MonitorRepo.List(ctx, ports.MonitorFilter{UserID: user.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(monitors) != 1 {
		t.Errorf("expected 1 monitor, got %d", len(monitors))
	}

	// List Active
	active := true
	activeMonitors, err := repo.MonitorRepo.List(ctx, ports.MonitorFilter{Active: &active})
	if err != nil {
		t.Fatalf("List (active): %v", err)
	}
	if len(activeMonitors) != 1 {
		t.Errorf("expected 1 active monitor, got %d", len(activeMonitors))
	}

	// Update
	monitor.Name = "Updated Monitor"
	if updateErr := repo.MonitorRepo.Update(ctx, monitor); updateErr != nil {
		t.Fatalf("Update: %v", updateErr)
	}
	fetched2, err := repo.MonitorRepo.GetByID(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if fetched2.Name != "Updated Monitor" {
		t.Errorf("expected name 'Updated Monitor', got %q", fetched2.Name)
	}

	// Delete
	if delErr := repo.MonitorRepo.Delete(ctx, monitor.ID); delErr != nil {
		t.Fatalf("Delete: %v", delErr)
	}
	_, err = repo.MonitorRepo.GetByID(ctx, monitor.ID)
	if err != ports.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestHeartbeatRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	// Create user and monitor
	user := &domain.User{Username: "hbuser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}

	monitor := &domain.Monitor{
		UserID:   user.ID,
		Name:     "HB Monitor",
		Type:     "ping",
		Active:   true,
		Interval: 60,
		Timeout:  5.0,
		Config:   map[string]any{},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}

	// Save heartbeats
	now := time.Now()
	hb1 := &domain.Heartbeat{
		MonitorID: monitor.ID,
		Status:    domain.StatusUp,
		Time:      now.Add(-2 * time.Minute),
		Msg:       "OK",
		Ping:      50,
		Duration:  100,
	}
	hb2 := &domain.Heartbeat{
		MonitorID: monitor.ID,
		Status:    domain.StatusDown,
		Time:      now.Add(-1 * time.Minute),
		Msg:       "Timeout",
		Ping:      0,
		Duration:  5000,
	}
	hb3 := &domain.Heartbeat{
		MonitorID: monitor.ID,
		Status:    domain.StatusUp,
		Time:      now,
		Msg:       "Recovered",
		Ping:      60,
		Duration:  120,
	}

	for _, hb := range []*domain.Heartbeat{hb1, hb2, hb3} {
		if err := repo.HeartbeatRepo.Save(ctx, hb); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	// GetLatest
	latest, err := repo.HeartbeatRepo.GetLatest(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if latest.Status != domain.StatusUp {
		t.Errorf("expected status UP, got %v", latest.Status)
	}
	if latest.Msg != "Recovered" {
		t.Errorf("expected msg 'Recovered', got %q", latest.Msg)
	}

	// ListByMonitor
	from := now.Add(-3 * time.Minute)
	to := now.Add(1 * time.Minute)
	heartbeats, err := repo.HeartbeatRepo.ListByMonitor(ctx, monitor.ID, from, to)
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(heartbeats) != 3 {
		t.Errorf("expected 3 heartbeats, got %d", len(heartbeats))
	}
}

// GetLatest must break a timestamp tie by id, or it returns a stale status.
//
// SQLite's timestamps are precise enough that they do not produce the tie on
// their own, so the tie is CONSTRUCTED: two heartbeats written with an
// identical Time. The assertion pins the ordering contract both adapters
// implement (`time DESC, id DESC`).
func TestHeartbeatGetLatest_BreaksTimestampTieByID(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "tieuser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}
	monitor := &domain.Monitor{
		UserID: user.ID, Name: "Tie Monitor", Type: "push", Active: true,
		Interval: 60, Timeout: 5.0, Config: map[string]any{},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}

	// Same instant, written in order: the retry PENDING, then the confirmed DOWN.
	tie := time.Now().UTC().Truncate(time.Second)
	for _, status := range []domain.Status{domain.StatusPending, domain.StatusDown} {
		if err := repo.HeartbeatRepo.Save(ctx, &domain.Heartbeat{
			MonitorID: monitor.ID, Status: status, Time: tie,
		}); err != nil {
			t.Fatalf("Save heartbeat %v: %v", status, err)
		}
	}

	latest, err := repo.HeartbeatRepo.GetLatest(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if latest.Status != domain.StatusDown {
		t.Errorf("GetLatest returned %v for two heartbeats sharing a timestamp, want DOWN (the later row). "+
			"Without the id tie-break the engine picks arbitrarily, and a DOWN monitor reads back as PENDING.",
			latest.Status)
	}
}

// ListByMonitor must also break a timestamp tie by id, or the chart and the
// recent-checks strip render same-second beats out of chronological order.
// The tie is constructed, as in TestHeartbeatGetLatest_BreaksTimestampTieByID.
func TestHeartbeatListByMonitor_BreaksTimestampTieByID(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "orderuser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}
	monitor := &domain.Monitor{
		UserID: user.ID, Name: "Order Monitor", Type: "push", Active: true,
		Interval: 60, Timeout: 5.0, Config: map[string]any{},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}

	tie := time.Now().UTC().Truncate(time.Second)
	want := []domain.Status{domain.StatusUp, domain.StatusPending, domain.StatusDown}
	for _, status := range want {
		if err := repo.HeartbeatRepo.Save(ctx, &domain.Heartbeat{
			MonitorID: monitor.ID, Status: status, Time: tie,
		}); err != nil {
			t.Fatalf("Save heartbeat %v: %v", status, err)
		}
	}

	got, err := repo.HeartbeatRepo.ListByMonitor(ctx, monitor.ID,
		tie.Add(-time.Minute), tie.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d heartbeats, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Status != w {
			t.Errorf("heartbeat %d = %v, want %v — beats sharing a timestamp must come back "+
				"in insertion order, not whatever the engine picks", i, got[i].Status, w)
		}
	}
}

func TestHeartbeatListRecentByMonitor_CapsNewestWithIDTieBreak(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "recentuser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}
	monitor := &domain.Monitor{
		UserID: user.ID, Name: "Recent Monitor", Type: "push", Active: true,
		Interval: 60, Timeout: 5.0, Config: map[string]any{},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Second)
	for i, status := range []domain.Status{
		domain.StatusUp, domain.StatusUp, domain.StatusPending, domain.StatusDown,
	} {
		if err := repo.HeartbeatRepo.Save(ctx, &domain.Heartbeat{
			MonitorID: monitor.ID, Status: status, Time: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("Save heartbeat %v: %v", status, err)
		}
	}
	// Same-second tie: PENDING then DOWN. Newest-by-id must win.
	tie := base.Add(10 * time.Second)
	if err := repo.HeartbeatRepo.Save(ctx, &domain.Heartbeat{
		MonitorID: monitor.ID, Status: domain.StatusPending, Time: tie,
	}); err != nil {
		t.Fatalf("Save pending: %v", err)
	}
	if err := repo.HeartbeatRepo.Save(ctx, &domain.Heartbeat{
		MonitorID: monitor.ID, Status: domain.StatusDown, Time: tie,
	}); err != nil {
		t.Fatalf("Save down: %v", err)
	}

	got, err := repo.HeartbeatRepo.ListRecentByMonitor(ctx, monitor.ID,
		base.Add(-time.Minute), base.Add(time.Minute), 2)
	if err != nil {
		t.Fatalf("ListRecentByMonitor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d heartbeats, want 2", len(got))
	}
	if got[0].Status != domain.StatusDown {
		t.Errorf("newest = %v, want DOWN (later insert at the tied timestamp)", got[0].Status)
	}
	if got[1].Status != domain.StatusPending {
		t.Errorf("second = %v, want PENDING", got[1].Status)
	}
}

func TestHeartbeatAggregateQueries(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "agguser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}

	monitor := &domain.Monitor{
		UserID:   user.ID,
		Name:     "agg-test",
		Type:     "http",
		Active:   true,
		Interval: 60,
		Config:   map[string]any{},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}

	bucket := time.Now().UTC().Truncate(24 * time.Hour)
	agg := &ports.Aggregate1d{
		MonitorID:   monitor.ID,
		Bucket:      bucket,
		UpCount:     10,
		DownCount:   1,
		TotalChecks: 11,
		AvgPing:     42.5,
		MinPing:     30,
		MaxPing:     80,
	}
	if err := repo.HeartbeatRepo.SaveAggregate1d(ctx, agg); err != nil {
		t.Fatalf("SaveAggregate1d: %v", err)
	}

	got, err := repo.HeartbeatRepo.GetAggregate1d(ctx, monitor.ID, bucket.Add(-time.Hour))
	if err != nil {
		t.Fatalf("GetAggregate1d: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 aggregate, got %d", len(got))
	}
	if got[0].UpCount != 10 || got[0].AvgPing != 42.5 {
		t.Errorf("unexpected aggregate: %+v", got[0])
	}
}

func TestTLSInfoRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "tlsuser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}

	monitor := &domain.Monitor{
		UserID:   user.ID,
		Name:     "tls-test",
		Type:     "http",
		Active:   true,
		Interval: 60,
		Config:   map[string]any{},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}

	now := time.Now().UTC()
	info := &ports.TLSInfo{
		MonitorID:     monitor.ID,
		DaysRemaining: 90,
		NotAfter:      now.AddDate(0, 0, 90),
		Issuer:        "Test CA",
		CheckedAt:     now,
	}
	if err := repo.TLSInfoRepo.Upsert(ctx, info); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.TLSInfoRepo.GetByMonitorID(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("GetByMonitorID: %v", err)
	}
	if got.DaysRemaining != 90 || got.Issuer != "Test CA" {
		t.Errorf("unexpected tls info: %+v", got)
	}
}

func TestSettingRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	// Get (not found)
	_, err := repo.SettingRepo.Get(ctx, "nonexistent")
	if err != ports.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// Set
	setErr := repo.SettingRepo.Set(ctx, "test_key", "test_value")
	if setErr != nil {
		t.Fatalf("Set: %v", setErr)
	}

	// Get (found)
	val, err := repo.SettingRepo.Get(ctx, "test_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "test_value" {
		t.Errorf("expected 'test_value', got %q", val)
	}

	// Update
	setErr2 := repo.SettingRepo.Set(ctx, "test_key", "updated_value")
	if setErr2 != nil {
		t.Fatalf("Set (update): %v", setErr2)
	}
	val2, err := repo.SettingRepo.Get(ctx, "test_key")
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if val2 != "updated_value" {
		t.Errorf("expected 'updated_value', got %q", val2)
	}

	// Delete
	delErr := repo.SettingRepo.Delete(ctx, "test_key")
	if delErr != nil {
		t.Fatalf("Delete: %v", delErr)
	}
	_, err = repo.SettingRepo.Get(ctx, "test_key")
	if err != ports.ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestNotificationRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	// Create user
	user := &domain.User{Username: "notifuser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}

	// Create notification
	notif := &domain.Notification{
		UserID:        user.ID,
		Name:          "Test Slack",
		Type:          "slack",
		Active:        true,
		IsDefault:     false,
		IncludeAckURL: true,
		Config:        map[string]any{"webhook_url": "https://hooks.slack.com/test"},
	}
	if err := repo.NotificationRepo.Create(ctx, notif); err != nil {
		t.Fatalf("Create notification: %v", err)
	}
	if notif.ID == 0 {
		t.Fatal("expected notification ID to be set")
	}

	// GetByID
	fetched, err := repo.NotificationRepo.GetByID(ctx, notif.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.Name != notif.Name {
		t.Errorf("expected name %q, got %q", notif.Name, fetched.Name)
	}
	if !fetched.IncludeAckURL {
		t.Errorf("IncludeAckURL = false; want true")
	}

	fetched.IncludeAckURL = false
	if err := repo.NotificationRepo.Update(ctx, fetched); err != nil {
		t.Fatalf("Update IncludeAckURL: %v", err)
	}
	toggled, err := repo.NotificationRepo.GetByID(ctx, notif.ID)
	if err != nil {
		t.Fatalf("GetByID after ack-url toggle: %v", err)
	}
	if toggled.IncludeAckURL {
		t.Errorf("IncludeAckURL after update = true; want false")
	}

	// List
	notifs, err := repo.NotificationRepo.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(notifs) != 1 {
		t.Errorf("expected 1 notification, got %d", len(notifs))
	}

	// Delete
	delErr := repo.NotificationRepo.Delete(ctx, notif.ID)
	if delErr != nil {
		t.Fatalf("Delete: %v", delErr)
	}
	_, err = repo.NotificationRepo.GetByID(ctx, notif.ID)
	if err != ports.ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// bun must persist an explicit false on INSERT. A `default:true` tag would
	// marshal the zero value as SQL DEFAULT and silently store true.
	off := &domain.Notification{
		UserID:        user.ID,
		Name:          "No Ack",
		Type:          "webhook",
		Active:        true,
		IncludeAckURL: false,
		Config:        map[string]any{"url": "https://example.test/no-ack"},
	}
	if err := repo.NotificationRepo.Create(ctx, off); err != nil {
		t.Fatalf("Create IncludeAckURL=false: %v", err)
	}
	gotOff, err := repo.NotificationRepo.GetByID(ctx, off.ID)
	if err != nil {
		t.Fatalf("GetByID IncludeAckURL=false: %v", err)
	}
	if gotOff.IncludeAckURL {
		t.Fatal("Create IncludeAckURL=false persisted as true")
	}
}

// TestMonitorNotificationIncludeTarget guards the link-level include_target
// column: it defaults true on attach and SetIncludeTarget persists an explicit
// false (and back).
func TestMonitorNotificationIncludeTarget(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "linkuser", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}
	monitor := &domain.Monitor{ID: 0, UserID: user.ID, Name: "A", Type: "http", Config: map[string]any{"url": "https://a.example"}}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}
	notif := &domain.Notification{UserID: user.ID, Name: "pager", Type: "webhook", Active: true, Config: map[string]any{"url": "https://hooks.example"}}
	if err := repo.NotificationRepo.Create(ctx, notif); err != nil {
		t.Fatalf("Create notification: %v", err)
	}

	// Attach with the default (true).
	if err := repo.MonitorNotificationRepo.Attach(ctx, monitor.ID, notif.ID, true); err != nil {
		t.Fatalf("Attach include_target=true: %v", err)
	}
	links, err := repo.MonitorNotificationRepo.ListByMonitor(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(links) != 1 || !links[0].IncludeTarget {
		t.Fatalf("after attach = %#v; want one link with IncludeTarget=true", links)
	}

	// Flip to false and back, verifying persistence.
	if err := repo.MonitorNotificationRepo.SetIncludeTarget(ctx, monitor.ID, notif.ID, false); err != nil {
		t.Fatalf("SetIncludeTarget false: %v", err)
	}
	links, _ = repo.MonitorNotificationRepo.ListByMonitor(ctx, monitor.ID)
	if len(links) != 1 || links[0].IncludeTarget {
		t.Fatalf("after SetIncludeTarget(false) = %#v; want IncludeTarget=false", links)
	}
	if err := repo.MonitorNotificationRepo.SetIncludeTarget(ctx, monitor.ID, notif.ID, true); err != nil {
		t.Fatalf("SetIncludeTarget true: %v", err)
	}
	links, _ = repo.MonitorNotificationRepo.ListByMonitor(ctx, monitor.ID)
	if len(links) != 1 || !links[0].IncludeTarget {
		t.Fatalf("after SetIncludeTarget(true) = %#v; want IncludeTarget=true", links)
	}
}

// TestMaintenanceRepository guards two MariaDB contracts:
//
//  1. FK integrity: the window is created with a real UserID and must
//     round-trip.
//  2. Nullable dates: a cron-strategy window legitimately has no start/end
//     date — it relies on CronExpr + Duration instead — so StartDate/EndDate
//     must map through `nullzero`. MariaDB rejects Go's zero time
//     (0001-01-01 00:00:00) outright; SQLite is lenient, so this test
//     exercises the exact "cron strategy, no dates" shape, asserting Create
//     succeeds and the dates round-trip as zero (i.e. as if NULL).
func TestMaintenanceRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "maintuser", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	mw := &domain.MaintenanceWindow{
		UserID:      user.ID,
		Title:       "Nightly DB Maintenance",
		Description: "Recurring maintenance window with no fixed start/end date.",
		Active:      true,
		Strategy:    "cron",
		CronExpr:    "0 2 * * *",
		Duration:    30,
	}
	if err := repo.MaintenanceRepo.Create(ctx, mw); err != nil {
		t.Fatalf("Create (cron strategy, no start/end dates): %v", err)
	}
	if mw.ID == 0 {
		t.Fatal("expected maintenance window ID to be set")
	}

	fetched, err := repo.MaintenanceRepo.GetByID(ctx, mw.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.UserID != user.ID {
		t.Errorf("UserID = %d; want %d", fetched.UserID, user.ID)
	}
	if fetched.Strategy != "cron" {
		t.Errorf("Strategy = %q; want cron", fetched.Strategy)
	}
	if fetched.CronExpr != "0 2 * * *" {
		t.Errorf("CronExpr = %q; want '0 2 * * *'", fetched.CronExpr)
	}
	if fetched.Duration != 30 {
		t.Errorf("Duration = %d; want 30", fetched.Duration)
	}
	if !fetched.StartDate.IsZero() {
		t.Errorf("StartDate = %v; want zero (no start date for cron strategy)", fetched.StartDate)
	}
	if !fetched.EndDate.IsZero() {
		t.Errorf("EndDate = %v; want zero (no end date for cron strategy)", fetched.EndDate)
	}

	// List scoped to the owning user must include the new window -- this
	// is the query path MaintenanceHandlers.List uses once it reads the
	// authenticated user ID instead of a hardcoded placeholder.
	windows, err := repo.MaintenanceRepo.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, w := range windows {
		if w.ID == mw.ID {
			found = true
		}
	}
	if !found {
		t.Error("List(userID) did not include the created maintenance window")
	}

	// A "single" strategy window with real start/end dates still works.
	single := &domain.MaintenanceWindow{
		UserID:      user.ID,
		Title:       "One-off Upgrade Window",
		Description: "Single maintenance window with explicit dates.",
		Active:      true,
		Strategy:    "single",
		StartDate:   time.Now().UTC().Truncate(time.Second),
		EndDate:     time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second),
	}
	if err := repo.MaintenanceRepo.Create(ctx, single); err != nil {
		t.Fatalf("Create (single strategy, with dates): %v", err)
	}
	fetchedSingle, err := repo.MaintenanceRepo.GetByID(ctx, single.ID)
	if err != nil {
		t.Fatalf("GetByID (single): %v", err)
	}
	if fetchedSingle.StartDate.IsZero() || fetchedSingle.EndDate.IsZero() {
		t.Error("single-strategy window lost its start/end dates on round trip")
	}
}

func TestWebAuthnCredentialRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	// A credential needs an owning user (FK).
	user := &domain.User{Username: "passkeyuser", PasswordHash: "h", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	cred := &domain.WebAuthnCredential{
		UserID:       user.ID,
		CredentialID: []byte{0x01, 0x02, 0x03, 0x04},
		PublicKey:    []byte{0xaa, 0xbb, 0xcc},
		SignCount:    1,
		Transports:   []string{"internal", "hybrid"},
		Flags:        0x1d,
		FlagsKnown:   true,
		CloneWarning: true,
		Attachment:   "platform",
		Name:         "Pixel",
	}
	if err := repo.WebAuthnCredentialRepo.Create(ctx, cred); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cred.ID == 0 {
		t.Fatal("expected credential ID to be set")
	}

	// GetByCredentialID round-trips the raw bytes through base64 storage.
	got, err := repo.WebAuthnCredentialRepo.GetByCredentialID(ctx, []byte{0x01, 0x02, 0x03, 0x04})
	if err != nil {
		t.Fatalf("GetByCredentialID: %v", err)
	}
	if string(got.PublicKey) != string(cred.PublicKey) {
		t.Errorf("public key mismatch: %v", got.PublicKey)
	}
	if len(got.Transports) != 2 || got.Transports[0] != "internal" {
		t.Errorf("transports = %v; want [internal hybrid]", got.Transports)
	}
	if !got.FlagsKnown || got.Flags != cred.Flags || !got.CloneWarning || got.Attachment != "platform" {
		t.Errorf("authenticator state did not round-trip: %+v", got)
	}

	// ListByUser
	list, err := repo.WebAuthnCredentialRepo.ListByUser(ctx, user.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListByUser = %v, len %d; want 1", err, len(list))
	}

	// UpdateUsage
	now := time.Now().UTC()
	if err := repo.WebAuthnCredentialRepo.UpdateUsage(ctx, cred.ID, 7, 0x0d, false, "cross-platform", now); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}
	got2, _ := repo.WebAuthnCredentialRepo.GetByCredentialID(ctx, cred.CredentialID)
	if got2.SignCount != 7 {
		t.Errorf("sign count = %d; want 7", got2.SignCount)
	}
	if got2.LastUsedAt == nil {
		t.Error("last_used_at not set")
	}
	if got2.Flags != 0x0d || got2.CloneWarning || got2.Attachment != "cross-platform" {
		t.Errorf("updated authenticator state = %+v", got2)
	}

	// Delete scoped to the wrong user is a no-op (not found).
	if err := repo.WebAuthnCredentialRepo.Delete(ctx, cred.ID, user.ID+999); err != ports.ErrNotFound {
		t.Errorf("cross-user delete = %v; want ErrNotFound", err)
	}
	// Delete by the owner succeeds.
	if err := repo.WebAuthnCredentialRepo.Delete(ctx, cred.ID, user.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.WebAuthnCredentialRepo.GetByCredentialID(ctx, cred.CredentialID); err != ports.ErrNotFound {
		t.Errorf("after delete = %v; want ErrNotFound", err)
	}
}

// assertNonZeroTime fails the test if tm is the zero time.Time, i.e. exactly
// what Bun sends MariaDB when a repo forgets to populate a `notnull` created_at
// / updated_at column — MariaDB rejects it ("Incorrect datetime value"),
// SQLite silently accepts it, which is why this guard exists.
func assertNonZeroTime(t *testing.T, label string, tm time.Time) {
	t.Helper()
	if tm.IsZero() {
		t.Errorf("%s is zero after Create", label)
	}
}

// assertIDSet fails the test if id is the zero value, i.e. Create did not
// run at all.
func assertIDSet(t *testing.T, label string, id int64) {
	t.Helper()
	if id == 0 {
		t.Errorf("expected %s ID to be set", label)
	}
}

// assertTimeAdvanced fails the test if after is not strictly later than
// before, i.e. Update did not bump the timestamp.
func assertTimeAdvanced(t *testing.T, label string, before, after time.Time) {
	t.Helper()
	if !after.After(before) {
		t.Errorf("%s did not advance: before=%v after=%v", label, before, after)
	}
}

// TestCreateSetsTimestamps asserts that every repo Create populates
// CreatedAt/UpdatedAt before insert: the model fields are tagged `notnull`
// with no `nullzero`, so an unset zero time.Time is rejected by MariaDB while
// SQLite silently accepts it.
//
// Each step below asserts the entity whose table actually has a created_at
// and/or updated_at column comes back from Create with non-zero timestamps.
// Tag, StatusPageCNAME, and MaintenanceWindow are intentionally NOT asserted
// on timestamps: their tables have no created_at/updated_at columns at all
// (confirmed against both mariadb/migrations/001_init.up.sql and
// sqlite/migrations/001_init.up.sql), so there is nothing to set — they are
// still exercised to confirm Create succeeds.
func TestCreateSetsTimestamps(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := createTimestampTestUser(t, repo, ctx)
	createTimestampTestMonitor(t, repo, ctx, user.ID)
	createTimestampTestAPIKey(t, repo, ctx, user.ID)
	createTimestampTestTag(t, repo, ctx)
	createTimestampTestNotification(t, repo, ctx, user.ID)
	sp := createTimestampTestStatusPage(t, repo, ctx, "ts-status-page")
	createTimestampTestCname(t, repo, ctx, sp.ID)
	createTimestampTestSubscriber(t, repo, ctx, sp.ID)
	createTimestampTestIncident(t, repo, ctx, sp.ID)
	createTimestampTestMaintenance(t, repo, ctx, user.ID)
}

func createTimestampTestUser(t *testing.T, repo *Repository, ctx context.Context) *domain.User {
	t.Helper()
	user := &domain.User{Username: "ts-user", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("User Create: %v", err)
	}
	assertNonZeroTime(t, "User.CreatedAt", user.CreatedAt)
	assertNonZeroTime(t, "User.UpdatedAt", user.UpdatedAt)
	return user
}

func createTimestampTestMonitor(t *testing.T, repo *Repository, ctx context.Context, userID int64) {
	t.Helper()
	monitor := &domain.Monitor{
		UserID:   userID,
		Name:     "ts-monitor",
		Type:     "http",
		Active:   true,
		Interval: 60,
		Timeout:  30.0,
		Config:   map[string]any{"url": "https://example.com"},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Monitor Create: %v", err)
	}
	assertNonZeroTime(t, "Monitor.CreatedAt", monitor.CreatedAt)
	assertNonZeroTime(t, "Monitor.UpdatedAt", monitor.UpdatedAt)
}

// createTimestampTestAPIKey covers the zero-guard requirement: the repo must
// only default CreatedAt when it is unset, since AuthService.CreateAPIKey
// pre-populates it at the service layer.
func createTimestampTestAPIKey(t *testing.T, repo *Repository, ctx context.Context, userID int64) {
	t.Helper()
	apiKey := &domain.APIKey{
		UserID:  userID,
		Name:    "ts-key",
		KeyHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Active:  true,
		Scopes:  []string{"read"},
	}
	if err := repo.APIKeyRepo.Create(ctx, apiKey); err != nil {
		t.Fatalf("APIKey Create: %v", err)
	}
	assertNonZeroTime(t, "APIKey.CreatedAt", apiKey.CreatedAt)
}

func createTimestampTestTag(t *testing.T, repo *Repository, ctx context.Context) {
	t.Helper()
	tag := &domain.Tag{Name: "ts-tag", Color: "#123456"}
	if err := repo.TagRepo.Create(ctx, tag); err != nil {
		t.Fatalf("Tag Create: %v", err)
	}
	assertIDSet(t, "Tag", tag.ID)
}

func createTimestampTestNotification(t *testing.T, repo *Repository, ctx context.Context, userID int64) {
	t.Helper()
	notif := &domain.Notification{
		UserID: userID,
		Name:   "ts-notif",
		Type:   "webhook",
		Active: true,
		Config: map[string]any{"url": "https://example.com/hook"},
	}
	if err := repo.NotificationRepo.Create(ctx, notif); err != nil {
		t.Fatalf("Notification Create: %v", err)
	}
	assertNonZeroTime(t, "Notification.CreatedAt", notif.CreatedAt)
	assertNonZeroTime(t, "Notification.UpdatedAt", notif.UpdatedAt)
}

// createTimestampTestStatusPage asserts StatusPage Create populates
// CreatedAt/UpdatedAt.
func createTimestampTestStatusPage(t *testing.T, repo *Repository, ctx context.Context, slug string) *domain.StatusPage {
	t.Helper()
	sp := &domain.StatusPage{
		Slug:      slug,
		Title:     "Test Status Page",
		Theme:     "light",
		Published: true,
	}
	if err := repo.StatusPageRepo.Create(ctx, sp); err != nil {
		t.Fatalf("StatusPage Create: %v", err)
	}
	assertNonZeroTime(t, "StatusPage.CreatedAt", sp.CreatedAt)
	assertNonZeroTime(t, "StatusPage.UpdatedAt", sp.UpdatedAt)
	return sp
}

func createTimestampTestCname(t *testing.T, repo *Repository, ctx context.Context, statusPageID int64) {
	t.Helper()
	cname := &domain.StatusPageCNAME{StatusPageID: statusPageID, Domain: "status.example.com"}
	if err := repo.StatusPageCnameRepo.Create(ctx, cname); err != nil {
		t.Fatalf("StatusPageCNAME Create: %v", err)
	}
	assertIDSet(t, "StatusPageCNAME", cname.ID)
}

func createTimestampTestSubscriber(t *testing.T, repo *Repository, ctx context.Context, statusPageID int64) {
	t.Helper()
	sub := &domain.StatusPageSubscriber{
		StatusPageID: statusPageID,
		Email:        "subscriber@example.com",
		Active:       false,
	}
	if err := repo.StatusPageSubscriberRepo.Create(ctx, sub); err != nil {
		t.Fatalf("StatusPageSubscriber Create: %v", err)
	}
	assertNonZeroTime(t, "StatusPageSubscriber.CreatedAt", sub.CreatedAt)
	assertNonZeroTime(t, "StatusPageSubscriber.UpdatedAt", sub.UpdatedAt)
	assertIDSet(t, "StatusPageSubscriber", sub.ID)
}

// createTimestampTestIncident asserts Incident Create populates
// CreatedAt/UpdatedAt.
func createTimestampTestIncident(t *testing.T, repo *Repository, ctx context.Context, statusPageID int64) {
	t.Helper()
	inc := &domain.Incident{
		StatusPageID: statusPageID,
		Title:        "ts-incident",
		Content:      "Investigating an outage.",
		Style:        "danger",
		Pinned:       true,
		Active:       true,
	}
	if err := repo.IncidentRepo.Create(ctx, inc); err != nil {
		t.Fatalf("Incident Create: %v", err)
	}
	assertNonZeroTime(t, "Incident.CreatedAt", inc.CreatedAt)
}

func TestIncidentRepository_BreaksCreatedAtTieByID(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	sp := &domain.StatusPage{Slug: "incident-order", Title: "Incident order", Published: true}
	if err := repo.StatusPageRepo.Create(ctx, sp); err != nil {
		t.Fatalf("create status page: %v", err)
	}

	createdAt := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	first := &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "First",
		Content:      "First incident.",
		Style:        "warning",
		Active:       false,
		CreatedAt:    createdAt,
	}
	second := &domain.Incident{
		StatusPageID: sp.ID,
		Title:        "Second",
		Content:      "Second incident.",
		Style:        "danger",
		Active:       false,
		CreatedAt:    createdAt,
	}
	if err := repo.IncidentRepo.Create(ctx, first); err != nil {
		t.Fatalf("create first incident: %v", err)
	}
	if err := repo.IncidentRepo.Create(ctx, second); err != nil {
		t.Fatalf("create second incident: %v", err)
	}

	for name, list := range map[string]func(context.Context) ([]*domain.Incident, error){
		"status page": func(ctx context.Context) ([]*domain.Incident, error) {
			return repo.IncidentRepo.ListByStatusPage(ctx, sp.ID)
		},
		"all incidents": repo.IncidentRepo.ListAll,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := list(ctx)
			if err != nil {
				t.Fatalf("list incidents: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("incident count = %d, want 2", len(got))
			}
			if got[0].ID != second.ID || got[1].ID != first.ID {
				t.Fatalf("incident IDs = [%d, %d], want [%d, %d]", got[0].ID, got[1].ID, second.ID, first.ID)
			}
		})
	}
}

func createTimestampTestMaintenance(t *testing.T, repo *Repository, ctx context.Context, userID int64) {
	t.Helper()
	mw := &domain.MaintenanceWindow{
		UserID:      userID,
		Title:       "ts-maintenance",
		Description: "Planned maintenance.",
		Active:      true,
		Strategy:    "single",
	}
	if err := repo.MaintenanceRepo.Create(ctx, mw); err != nil {
		t.Fatalf("MaintenanceWindow Create: %v", err)
	}
	assertIDSet(t, "MaintenanceWindow", mw.ID)
}

// TestUpdateBumpsUpdatedAt asserts that Update always advances UpdatedAt for
// every entity whose table has that column. Update methods build a fresh
// model from the domain struct and never copy the new UpdatedAt back onto
// the caller's struct, so each check re-fetches via GetByID to observe the
// persisted value.
func TestUpdateBumpsUpdatedAt(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := assertUserUpdateBumpsUpdatedAt(t, repo, ctx)
	assertMonitorUpdateBumpsUpdatedAt(t, repo, ctx, user.ID)
	assertNotificationUpdateBumpsUpdatedAt(t, repo, ctx, user.ID)
	assertStatusPageUpdateBumpsUpdatedAt(t, repo, ctx)
}

func assertUserUpdateBumpsUpdatedAt(t *testing.T, repo *Repository, ctx context.Context) *domain.User {
	t.Helper()
	user := &domain.User{Username: "bump-user", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}
	before, err := repo.UserRepo.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByID user (before): %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	user.Timezone = "America/New_York"
	if err := repo.UserRepo.Update(ctx, user); err != nil {
		t.Fatalf("Update user: %v", err)
	}
	after, err := repo.UserRepo.GetByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetByID user (after): %v", err)
	}
	assertTimeAdvanced(t, "User.UpdatedAt", before.UpdatedAt, after.UpdatedAt)
	return user
}

func assertMonitorUpdateBumpsUpdatedAt(t *testing.T, repo *Repository, ctx context.Context, userID int64) {
	t.Helper()
	monitor := &domain.Monitor{
		UserID:   userID,
		Name:     "bump-monitor",
		Type:     "http",
		Active:   true,
		Interval: 60,
		Timeout:  30.0,
		Config:   map[string]any{"url": "https://example.com"},
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("Create monitor: %v", err)
	}
	before, err := repo.MonitorRepo.GetByID(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("GetByID monitor (before): %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	monitor.Name = "bump-monitor-2"
	if err := repo.MonitorRepo.Update(ctx, monitor); err != nil {
		t.Fatalf("Update monitor: %v", err)
	}
	after, err := repo.MonitorRepo.GetByID(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("GetByID monitor (after): %v", err)
	}
	assertTimeAdvanced(t, "Monitor.UpdatedAt", before.UpdatedAt, after.UpdatedAt)
}

func assertNotificationUpdateBumpsUpdatedAt(t *testing.T, repo *Repository, ctx context.Context, userID int64) {
	t.Helper()
	notif := &domain.Notification{
		UserID: userID,
		Name:   "bump-notif",
		Type:   "webhook",
		Active: true,
		Config: map[string]any{"url": "https://example.com/hook"},
	}
	if err := repo.NotificationRepo.Create(ctx, notif); err != nil {
		t.Fatalf("Create notification: %v", err)
	}
	before, err := repo.NotificationRepo.GetByID(ctx, notif.ID)
	if err != nil {
		t.Fatalf("GetByID notification (before): %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	notif.Name = "bump-notif-2"
	if err := repo.NotificationRepo.Update(ctx, notif); err != nil {
		t.Fatalf("Update notification: %v", err)
	}
	after, err := repo.NotificationRepo.GetByID(ctx, notif.ID)
	if err != nil {
		t.Fatalf("GetByID notification (after): %v", err)
	}
	assertTimeAdvanced(t, "Notification.UpdatedAt", before.UpdatedAt, after.UpdatedAt)
}

func assertStatusPageUpdateBumpsUpdatedAt(t *testing.T, repo *Repository, ctx context.Context) {
	t.Helper()
	sp := createTimestampTestStatusPage(t, repo, ctx, "bump-status-page")
	before, err := repo.StatusPageRepo.GetByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("GetByID status page (before): %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	sp.Title = "Bump Status Page 2"
	if err := repo.StatusPageRepo.Update(ctx, sp); err != nil {
		t.Fatalf("Update status page: %v", err)
	}
	after, err := repo.StatusPageRepo.GetByID(ctx, sp.ID)
	if err != nil {
		t.Fatalf("GetByID status page (after): %v", err)
	}
	assertTimeAdvanced(t, "StatusPage.UpdatedAt", before.UpdatedAt, after.UpdatedAt)
}

// TestMonitorListOrdersByWeight proves List returns monitors by weight ASC,
// then name ASC (not only by id). Matches MonitorGroup.List ordering semantics.
func TestMonitorListOrdersByWeight(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	user := &domain.User{Username: "weight-user", PasswordHash: "hash", Active: true}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}
	// Insert with weights deliberately out of id order: high weight first, then low.
	seeds := []struct {
		name   string
		weight int
	}{
		{"zebra", 3000},
		{"alpha", 1000},
		{"middle", 2000},
	}
	for _, s := range seeds {
		m := &domain.Monitor{
			UserID: user.ID, Name: s.name, Type: "http", Active: true,
			Interval: 60, Timeout: 30, Weight: s.weight,
			Config: map[string]any{"url": "https://example.com"},
		}
		if err := repo.MonitorRepo.Create(ctx, m); err != nil {
			t.Fatalf("Create %q: %v", s.name, err)
		}
	}
	list, err := repo.MonitorRepo.List(ctx, ports.MonitorFilter{UserID: user.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List len = %d; want 3", len(list))
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, name := range want {
		if list[i].Name != name {
			t.Errorf("list[%d] = %q; want %q (weight order)", i, list[i].Name, name)
		}
	}
}

// TestMonitorGroupRepository covers basic CRUD: Create, GetByID, List
// (ordered by weight then name), and Update.
func TestMonitorGroupRepository(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "groupuser", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Three groups whose weights are deliberately out of alphabetical order,
	// so the List assertion below proves sorting is by weight first and only
	// falls back to name as a tiebreaker.
	middle := &domain.MonitorGroup{UserID: user.ID, Name: "Middle", Condition: domain.GroupConditionWorstOfChildren, Weight: 10}
	first := &domain.MonitorGroup{UserID: user.ID, Name: "First", Condition: domain.GroupConditionAllDown, Weight: 5}
	last := &domain.MonitorGroup{
		UserID: user.ID, Name: "Last", Condition: domain.GroupConditionThreshold,
		Threshold: 3, ThresholdIsPercent: true, Weight: 20, Collapsed: true,
	}

	for _, g := range []*domain.MonitorGroup{middle, first, last} {
		if err := repo.MonitorGroupRepo.Create(ctx, g); err != nil {
			t.Fatalf("Create group %q: %v", g.Name, err)
		}
		assertIDSet(t, "MonitorGroup "+g.Name, g.ID)
		assertNonZeroTime(t, "MonitorGroup.CreatedAt "+g.Name, g.CreatedAt)
		assertNonZeroTime(t, "MonitorGroup.UpdatedAt "+g.Name, g.UpdatedAt)
	}

	// GetByID round-trips every field, including the threshold/percent pair.
	fetched, err := repo.MonitorGroupRepo.GetByID(ctx, last.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.Name != "Last" || fetched.Condition != domain.GroupConditionThreshold {
		t.Errorf("GetByID: got name=%q condition=%q", fetched.Name, fetched.Condition)
	}
	if fetched.Threshold != 3 || !fetched.ThresholdIsPercent {
		t.Errorf("GetByID: threshold=%d percent=%v; want 3, true", fetched.Threshold, fetched.ThresholdIsPercent)
	}
	if !fetched.Collapsed {
		t.Error("GetByID: expected Collapsed true")
	}

	// List — ordered by weight ASC then name ASC.
	all, err := repo.MonitorGroupRepo.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List: expected 3 groups, got %d", len(all))
	}
	wantOrder := []string{"First", "Middle", "Last"}
	for i, name := range wantOrder {
		if all[i].Name != name {
			t.Errorf("List order = [%s, %s, %s]; want %v", all[0].Name, all[1].Name, all[2].Name, wantOrder)
			break
		}
	}

	// Update.
	before, err := repo.MonitorGroupRepo.GetByID(ctx, middle.ID)
	if err != nil {
		t.Fatalf("GetByID before update: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	middle.Name = "Middle Renamed"
	middle.Collapsed = true
	if err := repo.MonitorGroupRepo.Update(ctx, middle); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := repo.MonitorGroupRepo.GetByID(ctx, middle.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if after.Name != "Middle Renamed" || !after.Collapsed {
		t.Errorf("Update did not persist: got name=%q collapsed=%v", after.Name, after.Collapsed)
	}
	assertTimeAdvanced(t, "MonitorGroup.UpdatedAt", before.UpdatedAt, after.UpdatedAt)
}

// TestMonitorGroupRepositoryDeleteRehomesChildren is a regression guard for
// the core Delete contract: removing a group must re-home its child monitors
// and child subgroups to the deleted group's own parent, never cascade-delete
// them. It asserts the effect — the monitor and subgroup still exist and now
// point at the new parent — rather than merely that Delete returns no error.
func TestMonitorGroupRepositoryDeleteRehomesChildren(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "rehomeuser", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	root := &domain.MonitorGroup{UserID: user.ID, Name: "Root", Condition: domain.GroupConditionWorstOfChildren}
	if err := repo.MonitorGroupRepo.Create(ctx, root); err != nil {
		t.Fatalf("create root group: %v", err)
	}

	child := &domain.MonitorGroup{UserID: user.ID, Name: "Child", ParentID: &root.ID, Condition: domain.GroupConditionWorstOfChildren}
	if err := repo.MonitorGroupRepo.Create(ctx, child); err != nil {
		t.Fatalf("create child group: %v", err)
	}

	grandchild := &domain.MonitorGroup{UserID: user.ID, Name: "Grandchild", ParentID: &child.ID, Condition: domain.GroupConditionWorstOfChildren}
	if err := repo.MonitorGroupRepo.Create(ctx, grandchild); err != nil {
		t.Fatalf("create grandchild group: %v", err)
	}

	monitor := &domain.Monitor{
		UserID:   user.ID,
		Name:     "Child Monitor",
		Type:     "http",
		Active:   true,
		Interval: 60,
		Timeout:  30.0,
		Config:   map[string]any{"url": "https://example.com"},
		GroupID:  &child.ID,
	}
	if err := repo.MonitorRepo.Create(ctx, monitor); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	// Delete the middle group. Its child monitor and its child subgroup must
	// both be re-homed to Root (child.ParentID), not deleted or orphaned.
	if err := repo.MonitorGroupRepo.Delete(ctx, child.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The deleted group itself is gone.
	if _, err := repo.MonitorGroupRepo.GetByID(ctx, child.ID); err != ports.ErrNotFound {
		t.Errorf("GetByID(child) after delete = %v; want ErrNotFound", err)
	}

	// The monitor still exists (delete-group must never delete-monitor) and
	// now points at Root instead of the deleted group or nil.
	fetchedMonitor, err := repo.MonitorRepo.GetByID(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("monitor should still exist after group delete: %v", err)
	}
	if fetchedMonitor.GroupID == nil || *fetchedMonitor.GroupID != root.ID {
		t.Errorf("monitor.GroupID = %v; want %d (re-homed to root)", fetchedMonitor.GroupID, root.ID)
	}

	// The grandchild subgroup still exists and now points at Root, not the
	// deleted group and not left dangling.
	fetchedGrandchild, err := repo.MonitorGroupRepo.GetByID(ctx, grandchild.ID)
	if err != nil {
		t.Fatalf("grandchild group should still exist after parent delete: %v", err)
	}
	if fetchedGrandchild.ParentID == nil || *fetchedGrandchild.ParentID != root.ID {
		t.Errorf("grandchild.ParentID = %v; want %d (re-homed to root)", fetchedGrandchild.ParentID, root.ID)
	}

	// Deleting a top-level group (ParentID nil) re-homes its remaining
	// children to nil (top level), not to some stale non-nil value.
	if err := repo.MonitorGroupRepo.Delete(ctx, root.ID); err != nil {
		t.Fatalf("Delete root: %v", err)
	}
	fetchedMonitor2, err := repo.MonitorRepo.GetByID(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("monitor should still exist after root delete: %v", err)
	}
	if fetchedMonitor2.GroupID != nil {
		t.Errorf("monitor.GroupID = %v; want nil (re-homed to top level)", *fetchedMonitor2.GroupID)
	}
	fetchedGrandchild2, err := repo.MonitorGroupRepo.GetByID(ctx, grandchild.ID)
	if err != nil {
		t.Fatalf("grandchild group should still exist after root delete: %v", err)
	}
	if fetchedGrandchild2.ParentID != nil {
		t.Errorf("grandchild.ParentID = %v; want nil (re-homed to top level)", *fetchedGrandchild2.ParentID)
	}
}

// TestMonitorFilterByGroup covers ports.MonitorFilter's GroupID and
// GroupIDIsNull fields, added by the monitor group feature.
func TestMonitorFilterByGroup(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()

	user := &domain.User{Username: "filteruser", PasswordHash: "hash", Active: true, Timezone: "UTC"}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	group := &domain.MonitorGroup{UserID: user.ID, Name: "Group A", Condition: domain.GroupConditionWorstOfChildren}
	if err := repo.MonitorGroupRepo.Create(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}
	otherGroup := &domain.MonitorGroup{UserID: user.ID, Name: "Group B", Condition: domain.GroupConditionWorstOfChildren}
	if err := repo.MonitorGroupRepo.Create(ctx, otherGroup); err != nil {
		t.Fatalf("create other group: %v", err)
	}

	inGroup := &domain.Monitor{
		UserID: user.ID, Name: "In Group", Type: "http", Active: true, Interval: 60, Timeout: 30.0,
		Config: map[string]any{"url": "https://a.example.com"}, GroupID: &group.ID,
	}
	inOtherGroup := &domain.Monitor{
		UserID: user.ID, Name: "In Other Group", Type: "http", Active: true, Interval: 60, Timeout: 30.0,
		Config: map[string]any{"url": "https://b.example.com"}, GroupID: &otherGroup.ID,
	}
	topLevel := &domain.Monitor{
		UserID: user.ID, Name: "Top Level", Type: "http", Active: true, Interval: 60, Timeout: 30.0,
		Config: map[string]any{"url": "https://c.example.com"},
	}

	for _, m := range []*domain.Monitor{inGroup, inOtherGroup, topLevel} {
		if err := repo.MonitorRepo.Create(ctx, m); err != nil {
			t.Fatalf("create monitor %q: %v", m.Name, err)
		}
	}

	// GroupID filters to exactly the monitors filed under that one group.
	byGroup, err := repo.MonitorRepo.List(ctx, ports.MonitorFilter{UserID: user.ID, GroupID: &group.ID})
	if err != nil {
		t.Fatalf("List(GroupID): %v", err)
	}
	if len(byGroup) != 1 || byGroup[0].ID != inGroup.ID {
		t.Errorf("List(GroupID=%d) = %v; want exactly [%d]", group.ID, monitorIDs(byGroup), inGroup.ID)
	}

	// GroupIDIsNull filters to top-level monitors only.
	topLevelOnly, err := repo.MonitorRepo.List(ctx, ports.MonitorFilter{UserID: user.ID, GroupIDIsNull: true})
	if err != nil {
		t.Fatalf("List(GroupIDIsNull): %v", err)
	}
	if len(topLevelOnly) != 1 || topLevelOnly[0].ID != topLevel.ID {
		t.Errorf("List(GroupIDIsNull) = %v; want exactly [%d]", monitorIDs(topLevelOnly), topLevel.ID)
	}

	// No group filter at all returns every monitor owned by the user.
	all, err := repo.MonitorRepo.List(ctx, ports.MonitorFilter{UserID: user.ID})
	if err != nil {
		t.Fatalf("List(no group filter): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List(no group filter) = %d monitors; want 3", len(all))
	}
}

func monitorIDs(monitors []*domain.Monitor) []int64 {
	ids := make([]int64, len(monitors))
	for i, m := range monitors {
		ids[i] = m.ID
	}
	return ids
}
