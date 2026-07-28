package uptimekuma

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// buildClassicFixture creates a Kuma-like SQLite DB without parent/timeout/description.
func buildClassicFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeIgnoringError(db)

	stmts := []string{
		`CREATE TABLE user (id INTEGER PRIMARY KEY, username TEXT, password TEXT)`,
		`INSERT INTO user (id, username, password) VALUES (1, 'admin', '$2a$10$notarealhash')`,

		`CREATE TABLE proxy (
			id INTEGER PRIMARY KEY, user_id INTEGER, protocol TEXT, host TEXT, port INTEGER,
			auth INTEGER, username TEXT, password TEXT, active INTEGER, "default" INTEGER
		)`,
		`INSERT INTO proxy VALUES (1, 1, 'http', 'proxy.example.com', 8080, 1, 'u', 'secret-proxy', 1, 1)`,

		`CREATE TABLE tag (id INTEGER PRIMARY KEY, name TEXT, color TEXT, created_date TEXT)`,
		`INSERT INTO tag VALUES (1, 'prod', '#ff0000', '2024-01-01')`,
		`INSERT INTO tag VALUES (2, 'edge', '#00ff00', '2024-01-01')`,

		`CREATE TABLE notification (
			id INTEGER PRIMARY KEY, name TEXT, active INTEGER, user_id INTEGER, is_default INTEGER, config TEXT
		)`,
		`INSERT INTO notification VALUES (1, 'ops-telegram', 1, 1, 1,
			'{"type":"telegram","telegramBotToken":"123:ABC","telegramChatID":"-1001"}')`,
		`INSERT INTO notification VALUES (2, 'pagerduty-skip', 1, 1, 0,
			'{"type":"pagerduty","pagerdutyIntegrationKey":"secret-pd"}')`,

		`CREATE TABLE monitor (
			id INTEGER PRIMARY KEY,
			name TEXT, active INTEGER, user_id INTEGER, interval INTEGER, url TEXT, type TEXT,
			weight INTEGER, hostname TEXT, port INTEGER, keyword TEXT, maxretries INTEGER,
			ignore_tls INTEGER, upside_down INTEGER, maxredirects INTEGER,
			accepted_statuscodes_json TEXT, dns_resolve_type TEXT, dns_resolve_server TEXT,
			retry_interval INTEGER, push_token TEXT, method TEXT, body TEXT, headers TEXT,
			docker_container TEXT, proxy_id INTEGER, mqtt_topic TEXT, mqtt_success_message TEXT,
			mqtt_username TEXT, mqtt_password TEXT, database_connection_string TEXT,
			database_query TEXT, grpc_url TEXT, grpc_service_name TEXT, grpc_enable_tls INTEGER,
			resend_interval INTEGER, game TEXT
		)`,
		// http
		`INSERT INTO monitor (id, name, active, user_id, interval, url, type, weight, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval,
			method, resend_interval)
		 VALUES (1, 'Homepage', 1, 1, 60, 'https://example.com', 'http', 100, 2,
			0, 0, 10, '["200-299"]', 30, 'GET', 0)`,
		// keyword → http
		`INSERT INTO monitor (id, name, active, user_id, interval, url, type, weight, keyword, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval, method, resend_interval)
		 VALUES (2, 'Keyword check', 1, 1, 60, 'https://example.com/health', 'keyword', 200, 'OK', 0,
			1, 0, 5, '["200"]', 0, 'GET', 0)`,
		// port → tcp
		`INSERT INTO monitor (id, name, active, user_id, interval, type, weight, hostname, port, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval, resend_interval)
		 VALUES (3, 'DB port', 1, 1, 30, 'port', 300, 'db.example.com', 5432, 1,
			0, 0, 0, '[]', 10, 0)`,
		// radius — unsupported
		`INSERT INTO monitor (id, name, active, user_id, interval, type, weight, hostname, port, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval, resend_interval)
		 VALUES (4, 'RADIUS', 1, 1, 60, 'radius', 400, 'radius.example.com', 1812, 0,
			0, 0, 0, '[]', 0, 0)`,
		// push
		`INSERT INTO monitor (id, name, active, user_id, interval, type, weight, push_token, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval, resend_interval)
		 VALUES (5, 'Push agent', 1, 1, 60, 'push', 500, 'push-token-abc', 0,
			0, 0, 0, '[]', 0, 0)`,

		`CREATE TABLE monitor_tag (id INTEGER PRIMARY KEY, monitor_id INTEGER, tag_id INTEGER, value TEXT)`,
		`INSERT INTO monitor_tag VALUES (1, 1, 1, 'primary')`,
		`INSERT INTO monitor_tag VALUES (2, 3, 2, '')`,

		`CREATE TABLE monitor_notification (id INTEGER, monitor_id INTEGER, notification_id INTEGER)`,
		`INSERT INTO monitor_notification VALUES (1, 1, 1)`,
		`INSERT INTO monitor_notification VALUES (2, 1, 2)`, // notif 2 will be skipped

		`CREATE TABLE status_page (
			id INTEGER PRIMARY KEY, slug TEXT, title TEXT, description TEXT, icon TEXT, theme TEXT,
			published INTEGER, show_tags INTEGER, password TEXT, footer_text TEXT, custom_css TEXT
		)`,
		`INSERT INTO status_page VALUES (1, 'public', 'Public Status', 'desc', '/icon.png', 'dark', 1, 1, '', 'footer', '')`,

		`CREATE TABLE status_page_cname (id INTEGER PRIMARY KEY, status_page_id INTEGER, domain TEXT)`,
		`INSERT INTO status_page_cname VALUES (1, 1, 'status.example.com')`,

		`CREATE TABLE "group" (
			id INTEGER PRIMARY KEY, name TEXT, created_date TEXT, public INTEGER, active INTEGER,
			weight INTEGER, status_page_id INTEGER
		)`,
		`INSERT INTO "group" VALUES (1, 'Main', '2024-01-01', 1, 1, 1000, 1)`,

		`CREATE TABLE monitor_group (
			id INTEGER PRIMARY KEY, monitor_id INTEGER, group_id INTEGER, weight INTEGER, send_url INTEGER
		)`,
		`INSERT INTO monitor_group VALUES (1, 1, 1, 10, 0)`,
		`INSERT INTO monitor_group VALUES (2, 3, 1, 20, 0)`,

		// Heartbeats / sessions must be ignored even if present.
		`CREATE TABLE heartbeat (id INTEGER PRIMARY KEY, monitor_id INTEGER, status INTEGER, msg TEXT, time TEXT)`,
		`INSERT INTO heartbeat VALUES (1, 1, 1, 'ok', '2024-01-01 00:00:00')`,
		`CREATE TABLE api_key (id INTEGER PRIMARY KEY, key TEXT, name TEXT, user_id INTEGER)`,
		`INSERT INTO api_key VALUES (1, 'kuma-secret-key', 'ci', 1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// buildParentFixture creates a Kuma-like DB with parent folders, description, timeout.
func buildParentFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeIgnoringError(db)

	stmts := []string{
		`CREATE TABLE monitor (
			id INTEGER PRIMARY KEY,
			name TEXT, description TEXT, active INTEGER, user_id INTEGER, interval INTEGER,
			url TEXT, type TEXT, weight INTEGER, hostname TEXT, port INTEGER, keyword TEXT,
			maxretries INTEGER, ignore_tls INTEGER, upside_down INTEGER, maxredirects INTEGER,
			accepted_statuscodes_json TEXT, dns_resolve_type TEXT, dns_resolve_server TEXT,
			retry_interval INTEGER, push_token TEXT, method TEXT, body TEXT, headers TEXT,
			docker_container TEXT, proxy_id INTEGER, expiry_notification INTEGER,
			mqtt_topic TEXT, mqtt_success_message TEXT, mqtt_username TEXT, mqtt_password TEXT,
			database_connection_string TEXT, database_query TEXT, grpc_url TEXT,
			grpc_service_name TEXT, grpc_enable_tls INTEGER, resend_interval INTEGER, game TEXT,
			parent INTEGER, timeout REAL, json_path TEXT, expected_value TEXT
		)`,
		// folder
		`INSERT INTO monitor (id, name, description, active, type, weight, maxretries, ignore_tls,
			upside_down, maxredirects, accepted_statuscodes_json, retry_interval, resend_interval, timeout)
		 VALUES (10, 'Production', 'prod folder', 1, 'group', 1, 0, 0, 0, 0, '[]', 0, 0, 0)`,
		// nested folder
		`INSERT INTO monitor (id, name, description, active, type, weight, maxretries, ignore_tls,
			upside_down, maxredirects, accepted_statuscodes_json, retry_interval, resend_interval, parent, timeout)
		 VALUES (11, 'APIs', 'api subfolder', 1, 'group', 2, 0, 0, 0, 0, '[]', 0, 0, 10, 0)`,
		// http under nested folder
		`INSERT INTO monitor (id, name, description, active, type, weight, url, interval, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval,
			method, resend_interval, parent, timeout, expiry_notification)
		 VALUES (12, 'API health', 'checks /health', 1, 'http', 3, 'https://api.example.com/health', 30, 1,
			0, 0, 10, '["200-299","301"]', 15, 'GET', 5, 11, 10.5, 1)`,
		// dns
		`INSERT INTO monitor (id, name, active, type, weight, hostname, interval, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval,
			resend_interval, parent, timeout, dns_resolve_type, dns_resolve_server)
		 VALUES (13, 'DNS example', 1, 'dns', 4, 'example.com', 60, 0,
			0, 0, 0, '[]', 0, 0, 10, 5, 'A', '1.1.1.1')`,
		// postgres → database
		`INSERT INTO monitor (id, name, active, type, weight, interval, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval,
			resend_interval, timeout, database_connection_string, database_query)
		 VALUES (14, 'PG primary', 1, 'postgres', 5, 60, 0,
			0, 0, 0, '[]', 0, 0, 0, 'postgres://user:pass@db:5432/app', 'SELECT 1')`,
		// kafka — skip
		`INSERT INTO monitor (id, name, active, type, weight, interval, maxretries,
			ignore_tls, upside_down, maxredirects, accepted_statuscodes_json, retry_interval, resend_interval, timeout)
		 VALUES (15, 'Kafka prod', 1, 'kafka-producer', 6, 60, 0, 0, 0, 0, '[]', 0, 0, 0)`,

		`CREATE TABLE notification (
			id INTEGER PRIMARY KEY, name TEXT, active INTEGER, user_id INTEGER, is_default INTEGER, config TEXT
		)`,
		`INSERT INTO notification VALUES (1, 'discord-ops', 1, 1, 0,
			'{"type":"discord","discordWebhookUrl":"https://discord.com/api/webhooks/1/abc"}')`,
		`INSERT INTO notification VALUES (2, 'smtp-ops', 1, 1, 0,
			'{"type":"smtp","smtpHost":"smtp.example.com","smtpPort":587,"smtpUsername":"a","smtpPassword":"s","smtpFrom":"a@x.com","smtpTo":"b@x.com"}')`,

		`CREATE TABLE monitor_notification (id INTEGER, monitor_id INTEGER, notification_id INTEGER)`,
		`INSERT INTO monitor_notification VALUES (1, 12, 1)`,
		`INSERT INTO monitor_notification VALUES (2, 12, 2)`,

		`CREATE TABLE maintenance (
			id INTEGER PRIMARY KEY, title TEXT, description TEXT, user_id INTEGER, active INTEGER,
			strategy TEXT, start_date TEXT, end_date TEXT, cron TEXT, timezone TEXT, duration INTEGER
		)`,
		`INSERT INTO maintenance VALUES (1, 'Nightly', 'window', 1, 1, 'single',
			'2024-06-01 02:00:00', '2024-06-01 04:00:00', '', 'Asia/Bangkok', 0)`,
		`INSERT INTO maintenance VALUES (2, 'Cron maint', 'weekly', 1, 1, 'cron',
			'', '', '0 2 * * 0', 'UTC', 120)`,
		`CREATE TABLE monitor_maintenance (id INTEGER PRIMARY KEY, monitor_id INTEGER, maintenance_id INTEGER)`,
		`INSERT INTO monitor_maintenance VALUES (1, 12, 1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func TestConvertClassicSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kuma-classic.db")
	buildClassicFixture(t, dbPath)

	out := filepath.Join(dir, "backup.json")
	reportPath := filepath.Join(dir, "report.json")
	result, err := Convert(Options{Input: dbPath, Output: out, ReportPath: reportPath})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	// Users / API keys / heartbeats never appear.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// "admin" might appear as a name — check for password hash and api key specifically.
	if containsAny(string(raw), "kuma-secret-key", "$2a$10$notarealhash") {
		t.Fatalf("backup leaked user/api-key material")
	}

	var doc services.BackupDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal backup: %v", err)
	}
	if doc.Version != services.BackupDocumentVersion {
		t.Fatalf("version: got %d", doc.Version)
	}
	if len(doc.Monitors) != 4 { // http, keyword→http, port→tcp, push; radius skipped
		t.Fatalf("monitors: got %d want 4: %+v", len(doc.Monitors), names(doc))
	}
	if len(doc.Notifications) != 1 {
		t.Fatalf("notifications: got %d want 1", len(doc.Notifications))
	}
	if doc.Notifications[0].Type != "telegram" {
		t.Fatalf("notif type: %s", doc.Notifications[0].Type)
	}
	if doc.Notifications[0].Config["bot_token"] != "123:ABC" {
		t.Fatalf("telegram bot_token mapping failed: %#v", doc.Notifications[0].Config)
	}
	// Secrets present in backup (deliberate) but not in report.
	repRaw, _ := os.ReadFile(reportPath)
	if containsAny(string(repRaw), "123:ABC", "secret-proxy", "secret-pd") {
		t.Fatalf("report leaked secrets: %s", repRaw)
	}
	if result.Report.SkipCount < 2 { // radius + pagerduty at minimum
		t.Fatalf("expected skips, got %d: %+v", result.Report.SkipCount, result.Report.Skipped)
	}

	// Proxy password preserved for restore.
	if len(doc.Proxies) != 1 || doc.Proxies[0].Password != "secret-proxy" {
		t.Fatalf("proxy: %+v", doc.Proxies)
	}

	// Status page + order + cname.
	if len(doc.StatusPages) != 1 || doc.StatusPages[0].Slug != "public" {
		t.Fatalf("status pages: %+v", doc.StatusPages)
	}
	if len(doc.StatusPageCNAMEs) != 1 || doc.StatusPageCNAMEs[0].Domain != "status.example.com" {
		t.Fatalf("cnames: %+v", doc.StatusPageCNAMEs)
	}
	if len(doc.StatusPageMonitors) != 2 {
		t.Fatalf("status page monitors: %+v", doc.StatusPageMonitors)
	}
	// Deterministic display order (by weight 10 then 20).
	if doc.StatusPageMonitors[0].MonitorID != 1 || doc.StatusPageMonitors[1].MonitorID != 3 {
		t.Fatalf("order: %+v", doc.StatusPageMonitors)
	}

	// Tags + links.
	if len(doc.Tags) != 2 || len(doc.MonitorTags) != 2 {
		t.Fatalf("tags=%d links=%d", len(doc.Tags), len(doc.MonitorTags))
	}

	// Monitor notification: only telegram (notif 1) linked; pagerduty link skipped.
	if len(doc.MonitorNotifications) != 1 {
		t.Fatalf("monitor notifs: %+v", doc.MonitorNotifications)
	}

	// File mode 0600.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("output mode too open: %o", info.Mode().Perm())
	}
}

func TestConvertParentSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kuma-parent.db")
	buildParentFixture(t, dbPath)

	out := filepath.Join(dir, "backup.json")
	result, err := Convert(Options{Input: dbPath, Output: out})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	raw, _ := os.ReadFile(out)
	var doc services.BackupDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(doc.MonitorGroups) != 2 {
		t.Fatalf("groups: got %d want 2", len(doc.MonitorGroups))
	}
	// Nested parent link: APIs (11) → Production (10).
	var apis *services.BackupMonitorGroup
	for i := range doc.MonitorGroups {
		if doc.MonitorGroups[i].ID == 11 {
			apis = &doc.MonitorGroups[i]
		}
	}
	if apis == nil || apis.ParentID == nil || *apis.ParentID != 10 {
		t.Fatalf("nested group parent: %+v", doc.MonitorGroups)
	}

	// Monitors: http, dns, postgres → database; kafka skipped. = 3
	if len(doc.Monitors) != 3 {
		t.Fatalf("monitors: got %d: %+v", len(doc.Monitors), names(doc))
	}
	var httpMon *services.BackupMonitor
	for i := range doc.Monitors {
		if doc.Monitors[i].ID == 12 {
			httpMon = &doc.Monitors[i]
		}
	}
	if httpMon == nil {
		t.Fatal("missing http monitor 12")
	}
	if httpMon.GroupID == nil || *httpMon.GroupID != 11 {
		t.Fatalf("http group_id: %+v", httpMon.GroupID)
	}
	if httpMon.Timeout != 10.5 {
		t.Fatalf("timeout: %v", httpMon.Timeout)
	}
	if httpMon.Config["url"] != "https://api.example.com/health" {
		t.Fatalf("url: %#v", httpMon.Config)
	}
	if httpMon.Description != "checks /health" {
		t.Fatalf("description: %q", httpMon.Description)
	}

	var dbMon *services.BackupMonitor
	for i := range doc.Monitors {
		if doc.Monitors[i].ID == 14 {
			dbMon = &doc.Monitors[i]
		}
	}
	if dbMon == nil || dbMon.Type != "database" {
		t.Fatalf("postgres mapping: %+v", dbMon)
	}
	if dbMon.Config["engine"] != "postgres" {
		t.Fatalf("engine: %#v", dbMon.Config)
	}

	// Kafka skip recorded.
	foundKafka := false
	for _, s := range result.Report.Skipped {
		if s.ID == 15 {
			foundKafka = true
		}
	}
	if !foundKafka {
		t.Fatalf("kafka skip missing: %+v", result.Report.Skipped)
	}

	// Maintenance windows converted.
	if len(doc.MaintenanceWindows) != 2 {
		t.Fatalf("maintenance: %+v", doc.MaintenanceWindows)
	}
	if len(doc.MaintenanceMonitors) != 1 {
		t.Fatalf("maintenance monitors: %+v", doc.MaintenanceMonitors)
	}
}

func TestStrictMode(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kuma.db")
	buildClassicFixture(t, dbPath)
	out := filepath.Join(dir, "backup.json")
	_, err := Convert(Options{Input: dbPath, Output: out, Strict: true})
	if err == nil {
		t.Fatal("expected strict error for skipped entities")
	}
	// Output still written.
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output should still be written: %v", err)
	}
}

func TestOverwriteRefusal(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kuma.db")
	buildClassicFixture(t, dbPath)
	out := filepath.Join(dir, "backup.json")
	if err := os.WriteFile(out, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Convert(Options{Input: dbPath, Output: out, Force: false})
	if err == nil {
		t.Fatal("expected overwrite refusal")
	}
	// With force, succeeds.
	if _, err := Convert(Options{Input: dbPath, Output: out, Force: true}); err != nil {
		t.Fatalf("force: %v", err)
	}
}

func TestReadOnlySource(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kuma.db")
	buildClassicFixture(t, dbPath)

	// Open via converter and prove a write fails.
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeIgnoringError(db)
	if _, err := db.Exec(`INSERT INTO tag (id, name, color) VALUES (99, 'x', '#000')`); err == nil {
		t.Fatal("expected write against read-only DB to fail")
	}
}

func TestDeterminism(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kuma.db")
	buildClassicFixture(t, dbPath)

	out1 := filepath.Join(dir, "a.json")
	out2 := filepath.Join(dir, "b.json")
	r1, err := Convert(Options{Input: dbPath, Output: out1})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Convert(Options{Input: dbPath, Output: out2})
	if err != nil {
		t.Fatal(err)
	}
	// Compare after normalizing exported_at (wall-clock).
	r1.Document.ExportedAt = time.Time{}
	r2.Document.ExportedAt = time.Time{}
	b1, _ := json.Marshal(r1.Document)
	b2, _ := json.Marshal(r2.Document)
	if string(b1) != string(b2) {
		t.Fatalf("non-deterministic output\nA=%s\nB=%s", b1, b2)
	}
}

func TestJSONAcceptsAsBackupDocument(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kuma.db")
	buildParentFixture(t, dbPath)
	out := filepath.Join(dir, "backup.json")
	if _, err := Convert(Options{Input: dbPath, Output: out}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc services.BackupDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("BackupDocument unmarshal: %v", err)
	}
	if doc.Version != 1 {
		t.Fatalf("version %d", doc.Version)
	}
	// Required slices non-nil after marshal round-trip from our writer.
	if doc.Monitors == nil {
		t.Fatal("monitors nil")
	}
	// Structural checks that Import would not immediately reject:
	for _, m := range doc.Monitors {
		if m.Name == "" || m.Type == "" {
			t.Fatalf("invalid monitor: %+v", m)
		}
		if _, ok := phoenixMonitorTypes[m.Type]; !ok {
			t.Fatalf("non-phoenix type in output: %s", m.Type)
		}
	}
	for _, n := range doc.Notifications {
		if _, ok := phoenixProviders[n.Type]; !ok {
			t.Fatalf("non-phoenix provider: %s", n.Type)
		}
	}
}

func TestMapMonitorTypeNoSilentCoerce(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"http", "http", false},
		{"keyword", "http", false},
		{"json-query", "http", false},
		{"port", "tcp", false},
		{"postgres", "database", false},
		{"rabbitmq", "rabbitmq", false},
		{"radius", "", true},
		{"kafka-producer", "", true},
		{"real-browser", "", true},
		{"group", "", true},
	}
	for _, tc := range cases {
		got, reason := mapMonitorType(tc.in)
		if tc.wantErr {
			if got != "" || reason == "" {
				t.Fatalf("%s: expected skip, got %q %q", tc.in, got, reason)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("%s: got %q want %q (%s)", tc.in, got, tc.want, reason)
		}
	}
}

func TestRequiredFlags(t *testing.T) {
	if _, err := Convert(Options{}); err == nil {
		t.Fatal("expected error for missing input/output")
	}
	if _, err := Convert(Options{Input: "x"}); err == nil {
		t.Fatal("expected error for missing output")
	}
}

func names(doc services.BackupDocument) []string {
	out := make([]string, 0, len(doc.Monitors))
	for _, m := range doc.Monitors {
		out = append(out, m.Name+"/"+m.Type)
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || len(s) > 0 && stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
