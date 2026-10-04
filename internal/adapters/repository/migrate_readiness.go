package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"hash/fnv"
	"time"
)

const migrationStartupTimeout = 30 * time.Minute

// runMariaDBMigrations keeps ownership and every migration statement on one session.
func runMariaDBMigrations(ctx context.Context, db *sql.DB, conn *sql.Conn) (runErr error) {
	lockName, err := mariaDBMigrationLockName(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	lockAcquired := false
	defer func() {
		// A dedicated connection must never return to the pool while it might still
		// own the session lock. Poison it when release cannot be confirmed.
		if lockAcquired {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var released sql.NullInt64
			err := conn.QueryRowContext(cleanupCtx, "SELECT RELEASE_LOCK(?)", lockName).Scan(&released)
			cancel()
			if err != nil || !released.Valid || released.Int64 != 1 {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				if runErr == nil {
					releaseErr := err
					if err == nil {
						releaseErr = fmt.Errorf("unexpected result %v", released)
					}
					runErr = fmt.Errorf("release MariaDB migration lock: %w", releaseErr)
				}
			}
		}
		if closeErr := conn.Close(); runErr == nil && closeErr != nil {
			runErr = fmt.Errorf("close MariaDB migration connection: %w", closeErr)
		}
	}()
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", lockName, int(migrationStartupTimeout.Seconds())).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire MariaDB migration lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return fmt.Errorf("acquire MariaDB migration lock: lock wait timed out")
	}
	lockAcquired = true
	return runMigrationsWithExecutor(ctx, db, conn, "mariadb")
}

func mariaDBMigrationLockName(ctx context.Context, conn *sql.Conn) (string, error) {
	var schema string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil {
		return "", fmt.Errorf("identify MariaDB migration database: %w", err)
	}
	if schema == "" {
		return "", fmt.Errorf("identify MariaDB migration database: no database selected")
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(schema))
	return fmt.Sprintf("upx-migrate-%016x", h.Sum64()), nil
}
