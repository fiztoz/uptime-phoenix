package mariadb

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"

	// Register MariaDB driver.
	_ "github.com/go-sql-driver/mysql"
)

// PoolSettings bounds the database/sql connection pool.
//
// The previous defaults (25 open / 25 idle, no idle timeout) left every
// borrowed connection sitting in Sleep until ConnMaxLifetime. Split API+worker
// therefore showed ~50 sessions in SHOW PROCESSLIST even when the app was
// idle, and a slow heartbeat lookup filled the rest.
type PoolSettings struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
}

// DefaultPool is sized for one Phoenix process against a dedicated MariaDB.
// Operators raise it via DB_MAX_OPEN_CONNS / DB_MAX_IDLE_CONNS when a worker
// shard has more concurrent checkers than this.
func DefaultPool() PoolSettings {
	return PoolSettings{
		MaxOpenConns:    10,
		MaxIdleConns:    2,
		ConnMaxIdleTime: 30 * time.Second,
		ConnMaxLifetime: 5 * time.Minute,
	}
}

func applyPool(sqldb *sql.DB, pool PoolSettings) {
	if pool.MaxOpenConns <= 0 {
		pool.MaxOpenConns = 10
	}
	if pool.MaxIdleConns < 0 {
		pool.MaxIdleConns = 2
	}
	if pool.MaxIdleConns > pool.MaxOpenConns {
		pool.MaxIdleConns = pool.MaxOpenConns
	}
	sqldb.SetMaxOpenConns(pool.MaxOpenConns)
	sqldb.SetMaxIdleConns(pool.MaxIdleConns)
	if pool.ConnMaxIdleTime > 0 {
		sqldb.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
	}
	if pool.ConnMaxLifetime > 0 {
		sqldb.SetConnMaxLifetime(pool.ConnMaxLifetime)
	}
}

// NewDB opens a MariaDB connection with DefaultPool.
// The DSN should include multiStatements=true for migration support:
//
//	user:password@tcp(host:3306)/dbname?multiStatements=true&parseTime=true
func NewDB(dsn string) (*bun.DB, error) {
	return NewDBWithPool(dsn, DefaultPool())
}

// NewDBWithPool opens a MariaDB connection using the given pool bounds.
func NewDBWithPool(dsn string, pool PoolSettings) (*bun.DB, error) {
	sqldb, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mariadb: %w", err)
	}

	applyPool(sqldb, pool)

	db := bun.NewDB(sqldb, mysqldialect.New())
	if err := db.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping mariadb: %w", err)
	}

	return db, nil
}
