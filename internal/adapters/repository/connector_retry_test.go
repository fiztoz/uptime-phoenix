package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestConnectorDeadlockRetryContract(t *testing.T) {
	sentinel := errors.New("non retryable")
	for _, tc := range []struct {
		name     string
		err      error
		attempts int
		cancel   bool
	}{
		{"success", nil, 1, false},
		{"ordinary", sentinel, 1, false},
		{"lock wait", &mysql.MySQLError{Number: 1205}, 1, false},
		{"dirty read", &mysql.MySQLError{Number: 1020}, 1, false},
		{"ambiguous connection", driver.ErrBadConn, 1, false},
		{"duplicate", &mysql.MySQLError{Number: 1062}, 1, false},
		{"exhausted", fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: 1213}), 4, false},
		{"canceled", &mysql.MySQLError{Number: 1213}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			attempts := 0
			err := retryConnectorDeadlock(ctx, func() error {
				attempts++
				if tc.cancel {
					cancel()
				}
				return tc.err
			})
			if attempts != tc.attempts {
				t.Fatalf("attempts=%d want=%d", attempts, tc.attempts)
			}
			want := tc.err
			if tc.cancel {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
		})
	}
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := retryConnectorDeadlock(ctx, func() error { t.Fatal("operation ran"); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	t.Run("recovers", func(t *testing.T) {
		calls := 0
		err := retryConnectorDeadlock(t.Context(), func() error {
			calls++
			if calls == 1 {
				return &mysql.MySQLError{Number: 1213}
			}
			return nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}
