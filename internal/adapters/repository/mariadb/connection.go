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

// NewDB opens a MariaDB connection and returns a Bun DB handle.
// The DSN should include multiStatements=true for migration support:
//
//	user:password@tcp(host:3306)/dbname?multiStatements=true&parseTime=true
func NewDB(dsn string) (*bun.DB, error) {
	sqldb, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mariadb: %w", err)
	}

	sqldb.SetMaxOpenConns(25)
	sqldb.SetMaxIdleConns(25)
	sqldb.SetConnMaxLifetime(5 * time.Minute)

	db := bun.NewDB(sqldb, mysqldialect.New())
	if err := db.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping mariadb: %w", err)
	}

	return db, nil
}
