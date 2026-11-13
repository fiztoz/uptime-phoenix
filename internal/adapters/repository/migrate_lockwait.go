package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	sqlite "modernc.org/sqlite"
)

// ErrMigrationLockWaitExhausted reports that a migration statement kept
// losing its lock wait to another session for every allowed attempt.
var ErrMigrationLockWaitExhausted = errors.New("migration lock wait exhausted")

// MigrationLockPolicy bounds how long a schema migration may wait for a lock
// another session holds, and how often it retries.
//
// Without a cap, MariaDB waits lock_wait_timeout (server default 86,400 s)
// for a metadata lock. While an ALTER TABLE waits, every new reader and writer
// of that table queues behind it, so one long transaction on heartbeats stalls
// all heartbeat traffic for as long as it is held.
type MigrationLockPolicy struct {
	// LockWait caps one attempt's lock wait. On MariaDB it sets the migration
	// session's lock_wait_timeout and innodb_lock_wait_timeout (whole seconds,
	// minimum 1). SQLite uses the connection's busy_timeout from DB_DSN.
	LockWait time.Duration
	// Attempts is the total number of tries per statement (MariaDB) or per
	// migration transaction (SQLite). Minimum 1.
	Attempts int
	// BaseBackoff is the pause after the first lock timeout; it doubles after
	// each further timeout, up to MaxBackoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// Logger receives retry warnings. Nil uses slog.Default().
	Logger *slog.Logger
}

// DefaultMigrationLockPolicy caps each lock wait at 10 s and tries 6 times
// with 2 s → 30 s backoff: at most about 2 minutes before startup fails.
func DefaultMigrationLockPolicy() MigrationLockPolicy {
	return MigrationLockPolicy{LockWait: 10 * time.Second, Attempts: 6, BaseBackoff: 2 * time.Second, MaxBackoff: 30 * time.Second}
}

func (p MigrationLockPolicy) normalized() MigrationLockPolicy {
	d := DefaultMigrationLockPolicy()
	if p.LockWait <= 0 {
		p.LockWait = d.LockWait
	}
	if p.Attempts < 1 {
		p.Attempts = 1
	}
	if p.BaseBackoff <= 0 {
		p.BaseBackoff = d.BaseBackoff
	}
	if p.MaxBackoff < p.BaseBackoff {
		p.MaxBackoff = p.BaseBackoff
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	return p
}

func (p MigrationLockPolicy) lockWaitSeconds() int {
	s := int(p.LockWait / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// RunMigrationsWithPolicy is RunMigrations with an explicit lock-wait cap and
// retry policy.
func RunMigrationsWithPolicy(db *sql.DB, engine string, policy MigrationLockPolicy) error {
	ctx, cancel := context.WithTimeout(context.Background(), migrationStartupTimeout)
	defer cancel()
	return runMigrations(ctx, db, engine, policy.normalized())
}

// isMigrationLockTimeout reports a lock wait that gave up while another
// session held the lock. The failed statement changed nothing, so it is safe
// to run again: MariaDB 1205 (lock_wait_timeout / innodb_lock_wait_timeout),
// MariaDB 1213 (deadlock victim), SQLite BUSY / LOCKED.
func isMigrationLockTimeout(err error) bool {
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		return my.Number == 1205 || my.Number == 1213
	}
	var lite *sqlite.Error
	if errors.As(err, &lite) {
		switch lite.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED (and their extended codes)
			return true
		}
	}
	return false
}

// withMigrationLockRetry runs fn until it succeeds, fails for another reason,
// or exhausts the policy's attempts on lock timeouts.
func withMigrationLockRetry(ctx context.Context, p MigrationLockPolicy, migration, step string, fn func() error) error {
	backoff := p.BaseBackoff
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !isMigrationLockTimeout(err) {
			return err
		}
		if attempt >= p.Attempts {
			return fmt.Errorf("%w: migration %s (%s) lost its lock wait %d attempts in a row (cap %ds each); another session holds a lock on the table: stop the old API/worker pods (scale to 0) and restart: %w",
				ErrMigrationLockWaitExhausted, migration, step, p.Attempts, p.lockWaitSeconds(), err)
		}
		p.Logger.Warn("migration lock wait exceeded; retrying",
			"migration", migration, "step", step, "attempt", attempt, "max_attempts", p.Attempts,
			"lock_wait_s", p.lockWaitSeconds(), "backoff", backoff.String(), "error", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("migration %s: %w (after lock wait: %v)", migration, ctx.Err(), err)
		case <-timer.C:
		}
		backoff *= 2
		if backoff > p.MaxBackoff {
			backoff = p.MaxBackoff
		}
	}
}

// lockRetryExecutor retries each MariaDB migration statement on a lock
// timeout. MariaDB DDL is not transactional, so the retry unit is one
// statement: earlier statements of the migration stay applied.
type lockRetryExecutor struct {
	mariaDBMigrationCatalog
	policy    MigrationLockPolicy
	migration string
}

func (e *lockRetryExecutor) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := withMigrationLockRetry(ctx, e.policy, e.migration, statementLabel(query), func() error {
		var execErr error
		res, execErr = e.mariaDBMigrationCatalog.ExecContext(ctx, query, args...)
		return execErr
	})
	return res, err
}

func statementLabel(query string) string {
	s := strings.Join(strings.Fields(query), " ")
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// setMariaDBSessionLockWait applies the cap to the migration session.
func setMariaDBSessionLockWait(ctx context.Context, conn *sql.Conn, p MigrationLockPolicy) error {
	s := p.lockWaitSeconds()
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET SESSION lock_wait_timeout = %d, innodb_lock_wait_timeout = %d", s, s)); err != nil {
		return fmt.Errorf("cap MariaDB migration lock wait: %w", err)
	}
	return nil
}

// restoreMariaDBSessionLockWait returns the session to the server defaults
// before the connection goes back to the pool.
func restoreMariaDBSessionLockWait(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "SET SESSION lock_wait_timeout = DEFAULT, innodb_lock_wait_timeout = DEFAULT")
	return err
}
