package domain

import (
	"strings"
	"testing"
)

// Monitor.Target() reaches every viewer of a monitor (WebSocket cards) and
// every notification channel, so a database DSN must be reduced to
// host[:port][/db]. RedactURLPassword alone missed both non-URL forms.
func TestDatabaseDisplayTarget_NeverLeaksCredentials(t *testing.T) {
	t.Parallel()
	const secret = "S3cr3t-DO-NOT-LEAK"
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"go mysql dsn", map[string]any{"connection_string": "monitor:" + secret + "@tcp(db.internal:3306)/app?parseTime=true&tls=skip-verify"}, "db.internal:3306/app"},
		{"go mysql dsn, @ and / in password", map[string]any{"connection_string": "monitor:p@ss/" + secret + "@tcp(db:3306)/app"}, "db:3306/app"},
		{"go mysql dsn, no net", map[string]any{"dsn": "root:" + secret + "@/app"}, ""},
		{"postgres keyword dsn", map[string]any{"connection_string": "host=pg.internal port=5432 user=monitor password=" + secret + " dbname=app sslmode=require"}, "pg.internal:5432/app"},
		{"postgres keyword dsn, quoted password", map[string]any{"connection_string": "host=pg dbname=app password='a b " + secret + "' user=m"}, "pg/app"},
		{"sql server ado dsn", map[string]any{"connection_string": "Server=tcp:sql.internal,1433;Database=app;User ID=m;Password=" + secret + ";Encrypt=true"}, "sql.internal,1433/app"},
		{"postgres url", map[string]any{"connection_string": "postgres://monitor:" + secret + "@pg.internal:5432/app?sslmode=require"}, "postgres://pg.internal:5432/app"},
		{"url with password in query", map[string]any{"connection_string": "postgres://pg.internal/app?user=m&password=" + secret}, "postgres://pg.internal/app"},
		{"url with unescaped @ in password", map[string]any{"connection_string": "mongodb://m:a@b" + secret + "@mongo:27017/admin"}, "mongodb://mongo:27017/admin"},
		{"redis url, password only", map[string]any{"connection_string": "redis://:" + secret + "@cache:6379/0"}, "redis://cache:6379/0"},
		{"sqlserver url", map[string]any{"connection_string": "sqlserver://m:" + secret + "@sql:1433?database=app"}, "sqlserver://sql:1433"},
		{"legacy camelCase key", map[string]any{"connectionString": "u:" + secret + "@tcp(h:3306)/d"}, "h:3306/d"},
		{"unparseable", map[string]any{"connection_string": secret}, ""},
		{"hostname only", map[string]any{"hostname": "db.internal"}, "db.internal"},
	}
	for _, tc := range cases {
		m := &Monitor{Type: "database", Config: tc.cfg}
		got := m.Target()
		if strings.Contains(got, secret) || strings.Contains(got, "monitor:") {
			t.Errorf("%s: Target() = %q leaks credentials", tc.name, got)
		}
		if got != tc.want {
			t.Errorf("%s: Target() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMonitorDisplayTarget_StripsURLUserinfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ  string
		cfg  map[string]any
		want string
	}{
		{"http", map[string]any{"url": "https://svc:DO-NOT-LEAK@example.com/health"}, "https://example.com/health"},
		{"rabbitmq", map[string]any{"url": "amqp://monitor:DO-NOT-LEAK@rabbitmq.internal:5672/%2F"}, "amqp://rabbitmq.internal:5672/%2F"},
		{"mqtt", map[string]any{"broker": "tcp://u:DO-NOT-LEAK@broker:1883"}, "tcp://broker:1883"},
		{"http", map[string]any{"url": "https://u:p%zzDO-NOT-LEAK@example.com/x"}, "https://example.com/x"},
		{"tcp", map[string]any{"hostname": "db.internal"}, "db.internal"},
		{"http", map[string]any{"url": "https://example.com/a@b"}, "https://example.com/a@b"},
	}
	for _, tc := range cases {
		got := MonitorDisplayTarget(tc.typ, tc.cfg)
		if strings.Contains(got, "DO-NOT-LEAK") || got != tc.want {
			t.Errorf("%s %v: target = %q, want %q", tc.typ, tc.cfg, got, tc.want)
		}
	}
}
