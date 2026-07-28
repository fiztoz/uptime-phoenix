// Package uptimekuma converts a stopped Uptime Kuma snapshot (SQLite file or
// MariaDB/MySQL DSN) into a Phoenix BackupDocument JSON file. It never writes
// the source database and never calls a live Phoenix API.
package uptimekuma

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/go-sql-driver/mysql" // MariaDB / MySQL (already in go.mod)
	_ "modernc.org/sqlite"             // CGO-free SQLite (already in go.mod)
)

// Supported source engines.
const (
	EngineSQLite  = "sqlite"
	EngineMariaDB = "mariadb"
	EngineMySQL   = "mysql" // alias of mariadb (same driver/schema)
)

// dialect abstracts engine-specific catalog queries and identifier quoting.
// Conversion SQL is otherwise shared: Kuma's redbean schema uses the same
// table/column names on SQLite and MariaDB.
type dialect interface {
	name() string
	tableExists(db *sql.DB, table string) (bool, error)
	tableColumns(db *sql.DB, table string) (map[string]struct{}, error)
	// quoteIdent returns a safely quoted identifier for reserved names (e.g. group).
	quoteIdent(name string) string
}

// source is an open, read-only Kuma database plus its dialect.
type source struct {
	db     *sql.DB
	d      dialect
	label  string // path or sanitized DSN host/db for reports (no password)
	engine string
}

func (s *source) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *source) tableExists(table string) (bool, error) {
	return s.d.tableExists(s.db, table)
}

func (s *source) tableColumns(table string) (map[string]struct{}, error) {
	return s.d.tableColumns(s.db, table)
}

func (s *source) quote(name string) string {
	return s.d.quoteIdent(name)
}

// normalizeEngine maps user input to a canonical engine name.
func normalizeEngine(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" {
		return EngineSQLite, nil
	}
	switch e {
	case EngineSQLite, "sqlite3":
		return EngineSQLite, nil
	case EngineMariaDB, EngineMySQL, "mariadb/mysql":
		return EngineMariaDB, nil
	default:
		return "", fmt.Errorf("unsupported engine %q (want sqlite or mariadb)", raw)
	}
}

// openSource opens the Kuma source according to engine.
//   - sqlite: opts.Input is a filesystem path (mode=ro)
//   - mariadb: opts.DSN is a go-sql-driver DSN; session is set READ ONLY
func openSource(engine, input, dsn string) (*source, error) {
	eng, err := normalizeEngine(engine)
	if err != nil {
		return nil, err
	}
	switch eng {
	case EngineSQLite:
		if strings.TrimSpace(input) == "" {
			return nil, fmt.Errorf("--input is required for engine=sqlite")
		}
		db, label, err := openSQLiteReadOnly(input)
		if err != nil {
			return nil, err
		}
		return &source{db: db, d: sqliteDialect{}, label: label, engine: EngineSQLite}, nil
	case EngineMariaDB:
		if strings.TrimSpace(dsn) == "" {
			return nil, fmt.Errorf("--dsn is required for engine=mariadb (e.g. user:pass@tcp(host:3306)/kuma?parseTime=true)")
		}
		db, label, err := openMariaDBReadOnly(dsn)
		if err != nil {
			return nil, err
		}
		return &source{db: db, d: mariaDialect{}, label: label, engine: EngineMariaDB}, nil
	default:
		return nil, fmt.Errorf("unsupported engine %q", eng)
	}
}

// openSQLiteReadOnly opens path as SQLite in read-only mode.
func openSQLiteReadOnly(path string) (*sql.DB, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve input path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, "", fmt.Errorf("stat input: %w", err)
	}
	if info.IsDir() {
		return nil, "", fmt.Errorf("input is a directory, expected a SQLite file")
	}

	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	fileDSN := fmt.Sprintf("file:%s?mode=ro", uriPath)

	db, err := sql.Open("sqlite", fileDSN)
	if err != nil {
		return nil, "", fmt.Errorf("open sqlite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	var n int
	if err := db.QueryRow(`SELECT 1`).Scan(&n); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("ping sqlite read-only: %w", err)
	}
	return db, abs, nil
}

// openMariaDBReadOnly opens a MariaDB/MySQL DSN and forces a read-only session.
// Prefer a dedicated read-only DB user; the session flag is defense in depth.
//
// DSN format (go-sql-driver/mysql):
//
//	user:password@tcp(host:3306)/kuma?parseTime=true&charset=utf8mb4
//
// parseTime is forced on when missing so TIMESTAMP columns scan into time.Time
// and RFC3339 strings consistently with SQLite fixtures.
func openMariaDBReadOnly(dsn string) (*sql.DB, string, error) {
	dsn = ensureParseTime(dsn)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("open mariadb: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("ping mariadb: %w", err)
	}
	// Best-effort read-only session (requires privilege; ignore failure if denied).
	if _, err := db.Exec(`SET SESSION TRANSACTION READ ONLY`); err != nil {
		// Continue — conversion is SELECT-only; operators should still use RO users.
		_ = err
	}
	if _, err := db.Exec(`SET SESSION sql_mode = CONCAT(@@sql_mode, ',STRICT_TRANS_TABLES')`); err != nil {
		_ = err
	}

	label := sanitizeDSNLabel(dsn)
	return db, label, nil
}

func ensureParseTime(dsn string) string {
	lower := strings.ToLower(dsn)
	if strings.Contains(lower, "parsetime=") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&parseTime=true"
	}
	return dsn + "?parseTime=true"
}

// sanitizeDSNLabel strips password material for report/log labels.
// Examples:
//
//	user:secret@tcp(db:3306)/kuma → user@tcp(db:3306)/kuma
//	user@tcp(db:3306)/kuma → unchanged
func sanitizeDSNLabel(dsn string) string {
	// Cut query string (may contain tokens in rare setups).
	base := dsn
	if i := strings.Index(base, "?"); i >= 0 {
		base = base[:i]
	}
	// user:pass@tcp(...) → user@tcp(...)
	at := strings.LastIndex(base, "@")
	if at < 0 {
		return base
	}
	userPart := base[:at]
	rest := base[at:] // includes @
	if colon := strings.Index(userPart, ":"); colon >= 0 {
		userPart = userPart[:colon]
	}
	return userPart + rest
}

// ── SQLite dialect ──────────────────────────────────────────────────────────

type sqliteDialect struct{}

func (sqliteDialect) name() string { return EngineSQLite }

func (sqliteDialect) quoteIdent(name string) string {
	// Double-quotes are the SQLite identifier delimiter.
	return `"` + name + `"`
}

func (sqliteDialect) tableColumns(db *sql.DB, table string) (map[string]struct{}, error) {
	if !safeIdent(table) {
		return nil, fmt.Errorf("unsafe table name %q", table)
	}
	rows, err := db.Query(`PRAGMA table_info("` + table + `")`)
	if err != nil {
		return nil, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer closeIgnoringError(rows)

	cols := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info: %w", err)
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
}

func (sqliteDialect) tableExists(db *sql.DB, table string) (bool, error) {
	if !safeIdent(table) {
		return false, fmt.Errorf("unsafe table name %q", table)
	}
	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`,
		table,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ── MariaDB dialect ─────────────────────────────────────────────────────────

type mariaDialect struct{}

func (mariaDialect) name() string { return EngineMariaDB }

func (mariaDialect) quoteIdent(name string) string {
	// Backticks are the MariaDB/MySQL identifier delimiter.
	return "`" + name + "`"
}

func (mariaDialect) tableColumns(db *sql.DB, table string) (map[string]struct{}, error) {
	if !safeIdent(table) {
		return nil, fmt.Errorf("unsafe table name %q", table)
	}
	// DATABASE() is the currently selected schema from the DSN path.
	rows, err := db.Query(`
		SELECT COLUMN_NAME
		FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, fmt.Errorf("information_schema columns(%s): %w", table, err)
	}
	defer closeIgnoringError(rows)

	cols := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan column name: %w", err)
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
}

func (mariaDialect) tableExists(db *sql.DB, table string) (bool, error) {
	if !safeIdent(table) {
		return false, fmt.Errorf("unsafe table name %q", table)
	}
	var name string
	err := db.QueryRow(`
		SELECT TABLE_NAME
		FROM INFORMATION_SCHEMA.TABLES
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME = ?
		  AND TABLE_TYPE = 'BASE TABLE'
		LIMIT 1`, table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

// hasCol is a helper for optional-column selection.
func hasCol(cols map[string]struct{}, name string) bool {
	_, ok := cols[name]
	return ok
}

// Legacy helpers kept for tests that open SQLite directly.
func openReadOnly(path string) (*sql.DB, error) {
	db, _, err := openSQLiteReadOnly(path)
	return db, err
}
