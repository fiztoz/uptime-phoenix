package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	modernsqlite "modernc.org/sqlite"
)

// DefaultDSN is the recommended local SQLite DSN with WAL and busy_timeout.
const DefaultDSN = "file:phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"

// initializedConnector applies connection-local invariants before a physical
// connection enters the pool. A canceled query can retire a file connection;
// one-time pool Exec calls do not configure its replacement.
type initializedConnector struct{ driver.Connector }

func (c initializedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	exec, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("sqlite connection does not support context execution")
	}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 10000",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := exec.ExecContext(ctx, pragma, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("sqlite pragma %q: %w", pragma, err)
		}
	}
	return conn, nil
}

// NewDB opens a SQLite connection and returns a Bun DB handle.
// The DSN is typically a file path like "file:phoenix.db" or ":memory:".
func NewDB(dsn string) (*bun.DB, error) {
	connector, err := modernsqlite.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb := sql.OpenDB(initializedConnector{Connector: connector})
	// SQLite allows one writer at a time. A single connection avoids "database is
	// locked" / SQLITE_BUSY when the scheduler runs many checks concurrently.
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	db := bun.NewDB(sqldb, sqlitedialect.New())
	if err := db.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}
