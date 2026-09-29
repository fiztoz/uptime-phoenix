// A constrained helper for rehearse_probe_heartbeat_migration.py. This is not
// an operator migration CLI: it runs only inside that script's isolated Docker
// container, against its fixed local Unix socket and throwaway schema.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"

	_ "github.com/go-sql-driver/mysql"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

func main() {
	if err := run(); err != nil {
		slog.Error("rehearsal migration failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if os.Getenv("PHOENIX_REHEARSAL_CONTAINER") != "1" {
		return errors.New("only the isolated rehearsal container may run this helper")
	}
	const dsn = "root@unix(/run/mysqld/mysqld.sock)/phoenix_m6_rehearsal?parseTime=true&loc=UTC&multiStatements=true"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("open isolated rehearsal database: %w", err)
	}
	defer func() { _ = db.Close() }()
	var schema string
	if err := db.QueryRowContext(context.Background(), "SELECT DATABASE()").Scan(&schema); err != nil {
		return fmt.Errorf("check rehearsal schema: %w", err)
	}
	if schema != "phoenix_m6_rehearsal" {
		return errors.New("refusing unexpected database")
	}
	if err := repository.RunMigrations(db, "mariadb"); err != nil {
		return fmt.Errorf("run real MariaDB migrations: %w", err)
	}
	return nil
}
