package mariadb

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/go-sql-driver/mysql"
)

// retryLeaseDeadlock replays only complete, database-only lease operations.
// Monitor deletion retries its own deadlock victims, but InnoDB can choose a
// concurrent lease operation as the victim instead. Retrying a statement inside
// that transaction is unsafe: error 1213 rolls the entire transaction back.
// Callers must restart their locking reads and sample authority clocks again.
func retryLeaseDeadlock(ctx context.Context, operation func() error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := operation()
		var conflict *mysql.MySQLError
		if attempt >= 3 || !errors.As(err, &conflict) || conflict.Number != 1213 {
			return err
		}
		// A bounded, jittered pause avoids immediately rejoining the same
		// contention cycle during replay drains. Never sleep past cancellation.
		delay := time.Duration((1<<attempt)*5+rand.IntN(5)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
