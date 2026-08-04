package uptimekuma

import (
	"strings"
	"testing"
)

func TestNormalizeEngine(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", EngineSQLite, false},
		{"sqlite", EngineSQLite, false},
		{"SQLite3", EngineSQLite, false},
		{"mariadb", EngineMariaDB, false},
		{"mysql", EngineMariaDB, false},
		{"MariaDB", EngineMariaDB, false},
		{"postgres", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeEngine(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeEngine(%q) want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeEngine(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeEngine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeDSNLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"user:secret@tcp(db:3306)/kuma", "user@tcp(db:3306)/kuma"},
		{"user:secret@tcp(db:3306)/kuma?parseTime=true", "user@tcp(db:3306)/kuma"},
		{"user@tcp(db:3306)/kuma", "user@tcp(db:3306)/kuma"},
		{"tcp(db:3306)/kuma", "tcp(db:3306)/kuma"},
	}
	for _, tc := range cases {
		if got := sanitizeDSNLabel(tc.in); got != tc.want {
			t.Errorf("sanitizeDSNLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(sanitizeDSNLabel(tc.in), "secret") {
			t.Errorf("sanitized label still contains password material: %s", sanitizeDSNLabel(tc.in))
		}
	}
}

func TestEnsureParseTime(t *testing.T) {
	if got := ensureParseTime("u:p@tcp(h:3306)/db"); got != "u:p@tcp(h:3306)/db?parseTime=true" {
		t.Errorf("no query: %s", got)
	}
	if got := ensureParseTime("u:p@tcp(h:3306)/db?charset=utf8mb4"); got != "u:p@tcp(h:3306)/db?charset=utf8mb4&parseTime=true" {
		t.Errorf("with query: %s", got)
	}
	if got := ensureParseTime("u:p@tcp(h:3306)/db?parseTime=true"); got != "u:p@tcp(h:3306)/db?parseTime=true" {
		t.Errorf("already set: %s", got)
	}
}

func TestDialectQuoteIdent(t *testing.T) {
	if q := (sqliteDialect{}).quoteIdent("group"); q != `"group"` {
		t.Errorf("sqlite quote = %s", q)
	}
	if q := (mariaDialect{}).quoteIdent("group"); q != "`group`" {
		t.Errorf("maria quote = %s", q)
	}
}

// MariaDB reserves INTERVAL; unquoted COALESCE(interval, 60) is Error 1064.
// The monitor SELECT must quote the column per dialect.
func TestBuildMonitorSelectColsQuotesInterval(t *testing.T) {
	cols := map[string]struct{}{
		"description": {},
		"timeout":     {},
		"parent":      {},
	}
	mariaCols, _, _, _, _, _, _, _, _, _ := buildMonitorSelectCols((mariaDialect{}).quoteIdent, cols)
	sql := strings.Join(mariaCols, ", ")
	if !strings.Contains(sql, "COALESCE(`interval`, 60)") {
		t.Fatalf("maria monitor select missing backtick-quoted interval:\n%s", sql)
	}
	if strings.Contains(sql, "COALESCE(interval, 60)") {
		t.Fatalf("maria monitor select still has unquoted interval:\n%s", sql)
	}

	sqliteCols, _, _, _, _, _, _, _, _, _ := buildMonitorSelectCols((sqliteDialect{}).quoteIdent, cols)
	sql = strings.Join(sqliteCols, ", ")
	if !strings.Contains(sql, `COALESCE("interval", 60)`) {
		t.Fatalf("sqlite monitor select missing double-quoted interval:\n%s", sql)
	}
}

func TestConvertRequiresEngineInputs(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/out.json"
	if _, err := Convert(Options{Engine: EngineSQLite, Output: out}); err == nil {
		t.Fatal("sqlite without input should fail")
	}
	if _, err := Convert(Options{Engine: EngineMariaDB, Output: out}); err == nil {
		t.Fatal("mariadb without dsn should fail")
	}
	// DSN-only infers mariadb and still needs a reachable server — expect open error, not flag error.
	if _, err := Convert(Options{DSN: "bad:bad@tcp(127.0.0.1:1)/none", Output: out}); err == nil {
		t.Fatal("unreachable dsn should fail")
	}
}

func TestMariaDBOpenMissingDSN(t *testing.T) {
	_, err := openSource(EngineMariaDB, "", "")
	if err == nil {
		t.Fatal("expected error for empty DSN")
	}
}
