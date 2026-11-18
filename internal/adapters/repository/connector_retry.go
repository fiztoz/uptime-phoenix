package repository

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/go-sql-driver/mysql"
)

// retryConnectorDeadlock retries only complete database-only operations. Error
// 1213 rolls back the entire InnoDB transaction, so each attempt must reacquire
// registration/reset locks, resample DB time and revalidate all ownership fences.
// Callbacks must not publish events or mutate inputs between attempts. Lease
// results are returned only after commit; every error returns an empty lease.
// This mirrors mariadb.retryLeaseDeadlock without broadening its package API.
func retryConnectorDeadlock(ctx context.Context, operation func() error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := operation()
		var deadlock *mysql.MySQLError
		if attempt >= 3 || !errors.As(err, &deadlock) || deadlock.Number != 1213 {
			return err
		}
		timer := time.NewTimer(time.Duration((1<<attempt)*5+rand.IntN(5)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
