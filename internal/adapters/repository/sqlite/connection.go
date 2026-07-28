package sqlite

import (
	"database/sql"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	// Register SQLite driver (CGO-free).
	_ "modernc.org/sqlite"
)

// DefaultDSN is the recommended local SQLite DSN with WAL and busy_timeout.
const DefaultDSN = "file:phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"

// NewDB opens a SQLite connection and returns a Bun DB handle.
// The DSN is typically a file path like "file:phoenix.db" or ":memory:".
func NewDB(dsn string) (*bun.DB, error) {
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite allows one writer at a time. A single connection avoids "database is
	// locked" / SQLITE_BUSY when the scheduler runs many checks concurrently.
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)

	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 10000",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := sqldb.Exec(pragma); err != nil {
			_ = sqldb.Close()
			return nil, fmt.Errorf("sqlite pragma %q: %w", pragma, err)
		}
	}

	db := bun.NewDB(sqldb, sqlitedialect.New())
	if err := db.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	return db, nil
}
