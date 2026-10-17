package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestM8PreflightClockAfterAuthorityLock(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			store := repository.NewProbePreflightStore(f.db)
			if _, _, err := store.CreatePreflight(ctx, m8PreflightIdentity(m8PreflightOp1, m8PreflightActorID), m8PreflightOwnerA); err != nil {
				t.Fatal(err)
			}
			var second *bun.DB
			var err error
			if engine == "sqlite" {
				second, err = sqlite.NewDB(f.dsn)
			} else {
				second, err = mariadb.NewDB(f.dsn)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = second.Close() })
			tx, err := f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			query := "SELECT strftime('%Y-%m-%d %H:%M:%f', 'now')"
			if engine == "mariadb" {
				query = "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f')"
			}
			var stamp string
			if err := tx.NewRaw(query).Scan(ctx, &stamp); err != nil {
				t.Fatal(err)
			}
			now, err := time.ParseInLocation("2006-01-02 15:04:05.999999", stamp, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			deadline := now.Add(900 * time.Millisecond)
			if _, err := tx.ExecContext(ctx, "UPDATE probe_host_preflights SET created_at = ?, deadline_at = ? WHERE operation_id = ?", deadline.Add(-domain.ProbePreflightLifetime), deadline, m8PreflightOp1); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			// Commit the deadline BEFORE holding authority. Updating it in the
			// blocker makes MariaDB return 1020, whose retry masks a stale
			// clock sample. A FOR UPDATE read holds the unchanged row instead.
			tx, err = f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if engine == "mariadb" {
				var lockedID string
				if err := tx.NewRaw("SELECT operation_id FROM probe_host_preflights WHERE operation_id = ? FOR UPDATE", m8PreflightOp1).Scan(ctx, &lockedID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := tx.ExecContext(ctx, "UPDATE probe_host_preflights SET operation_id = operation_id WHERE operation_id = ?", m8PreflightOp1); err != nil {
					t.Fatal(err)
				}
			}
			result := make(chan error, 1)
			entered := make(chan struct{})
			go func() {
				close(entered)
				_, err := repository.NewProbePreflightStore(second).BeginPreflight(ctx, m8PreflightOp1, m8PreflightOwnerA)
				result <- err
			}()
			<-entered
			select {
			case err := <-result:
				t.Fatalf("begin bypassed held authority lock: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			// Release unchanged authority only after the committed deadline
			// elapsed on the DB clock. A sample from before this wait is stale.
			for {
				if err := tx.NewRaw(query).Scan(ctx, &stamp); err != nil {
					t.Fatal(err)
				}
				observed, err := time.ParseInLocation("2006-01-02 15:04:05.999999", stamp, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				if !observed.Before(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("expired authority began after lock wait: %v", err)
			}
			observed, err := store.ObservePreflight(ctx, m8PreflightOp1)
			if err != nil || observed.State != domain.ProbePreflightInterrupted {
				t.Fatalf("late begin mutated expired work: %+v %v", observed, err)
			}
		})
	}
}
