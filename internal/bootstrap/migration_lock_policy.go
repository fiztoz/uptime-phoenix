package bootstrap

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

// migrationLockPolicy maps DB_MIGRATION_LOCK_* onto the startup migration
// runner's lock-wait cap and retry budget.
func migrationLockPolicy(cfg Config) repository.MigrationLockPolicy {
	p := repository.DefaultMigrationLockPolicy()
	p.LockWait = time.Duration(cfg.DBMigrationLockWaitSeconds) * time.Second
	p.Attempts = cfg.DBMigrationLockAttempts
	return p
}
