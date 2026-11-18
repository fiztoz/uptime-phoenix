package mariadb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// DB-free contract for retryLeaseDeadlock (#256): only a 1213 deadlock is
// retried, also when wrapped; 1205 and 1020 are returned at once; a run makes
// at most four attempts (the first try plus three retries); and a canceled
// context stops the loop.
func TestRetryLeaseDeadlock(t *testing.T) {
	wrapped := func(n uint16) error { return fmt.Errorf("lock lease rows: %w", &mysql.MySQLError{Number: n}) }
	run := func(ctx context.Context, errs ...error) (int, error) {
		attempts := 0
		err := retryLeaseDeadlock(ctx, func() error {
			attempts++
			if attempts <= len(errs) {
				return errs[attempts-1]
			}
			return errs[len(errs)-1]
		})
		return attempts, err
	}

	t.Run("wrapped 1213 is retried", func(t *testing.T) {
		attempts, err := run(context.Background(), wrapped(1213), nil)
		if err != nil || attempts != 2 {
			t.Fatalf("attempts=%d err=%v, want 2 attempts and success", attempts, err)
		}
	})
	for _, n := range []uint16{1205, 1020} {
		t.Run(fmt.Sprintf("%d is not retried", n), func(t *testing.T) {
			attempts, err := run(context.Background(), wrapped(n), nil)
			var engine *mysql.MySQLError
			if attempts != 1 || !errors.As(err, &engine) || engine.Number != n {
				t.Fatalf("attempts=%d err=%v, want 1 attempt returning %d", attempts, err, n)
			}
		})
	}
	t.Run("non-engine error is not retried", func(t *testing.T) {
		boom := errors.New("boom")
		if attempts, err := run(context.Background(), boom, nil); attempts != 1 || !errors.Is(err, boom) {
			t.Fatalf("attempts=%d err=%v", attempts, err)
		}
	})
	t.Run("attempts are capped at four", func(t *testing.T) {
		attempts, err := run(context.Background(), wrapped(1213))
		var engine *mysql.MySQLError
		if attempts != 4 || !errors.As(err, &engine) || engine.Number != 1213 {
			t.Fatalf("attempts=%d err=%v, want 4 attempts returning 1213", attempts, err)
		}
	})
	t.Run("canceled context stops retrying", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		err := retryLeaseDeadlock(ctx, func() error {
			attempts++
			cancel()
			return wrapped(1213)
		})
		if attempts != 1 || err == nil {
			t.Fatalf("attempts=%d err=%v, want 1 attempt after cancel", attempts, err)
		}
	})
}
