package mariadb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

// ConnectRetry bounds how long startup waits for MariaDB to accept
// connections (#163). After a host reboot the database container can still
// be starting when Phoenix boots; refusing to start on the first refused ping
// turns a few seconds of DB startup into a crash loop.
type ConnectRetry struct {
	// Timeout is the total wait budget. Zero or negative means one attempt.
	Timeout time.Duration
	// InitialBackoff is the first wait; it doubles up to MaxBackoff.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// OnRetry, if set, is called once per failed attempt that will be retried.
	OnRetry func(attempt int, wait time.Duration, err error)
}

// DefaultConnectRetry waits up to timeout with 500ms..5s backoff.
func DefaultConnectRetry(timeout time.Duration) ConnectRetry {
	return ConnectRetry{Timeout: timeout, InitialBackoff: 500 * time.Millisecond, MaxBackoff: 5 * time.Second}
}

type retryClock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NewDBWithRetry opens a MariaDB pool and pings it, retrying connection
// failures with backoff for up to retry.Timeout. Errors the server itself
// returned (bad credentials, unknown database) fail immediately.
func NewDBWithRetry(ctx context.Context, dsn string, pool PoolSettings, retry ConnectRetry) (*bun.DB, error) {
	sqldb, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mariadb: %w", err)
	}
	applyPool(sqldb, pool)
	db := bun.NewDB(sqldb, mysqldialect.New())
	if err := pingWithRetry(ctx, db.PingContext, retry, realClock{}); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("ping mariadb: %w", err)
	}
	return db, nil
}

func pingWithRetry(ctx context.Context, ping func(context.Context) error, r ConnectRetry, clock retryClock) error {
	deadline := clock.Now().Add(r.Timeout)
	wait := r.InitialBackoff
	if wait <= 0 {
		wait = 500 * time.Millisecond
	}
	maxWait := r.MaxBackoff
	if maxWait < wait {
		maxWait = wait
	}
	for attempt := 1; ; attempt++ {
		err := ping(ctx)
		if err == nil {
			return nil
		}
		if !retryableConnectError(err) || ctx.Err() != nil {
			return err
		}
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			return err
		}
		if wait > remaining {
			wait = remaining
		}
		if r.OnRetry != nil {
			r.OnRetry(attempt, wait, err)
		}
		if sleepErr := clock.Sleep(ctx, wait); sleepErr != nil {
			return errors.Join(err, sleepErr)
		}
		wait *= 2
		if wait > maxWait {
			wait = maxWait
		}
	}
}

// retryableConnectError is true for failures to reach a server. An error the
// server sent back (*mysql.MySQLError: access denied, unknown database) will
// not change by waiting.
func retryableConnectError(err error) bool {
	var serverErr *mysql.MySQLError
	return !errors.As(err, &serverErr)
}
