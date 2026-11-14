package repository_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
	sqlitedriver "modernc.org/sqlite"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// issue256Connector injects a typed engine failure at the driver boundary, so
// Bun must roll back the real transaction rather than a query hook hiding it.
// SQLite runs the MariaDB lease adapter with only FOR UPDATE stripped: this
// verifies retry/rollback behavior, NOT InnoDB locking. The MariaDB subtest
// executes the same regression without SQL rewriting on a disposable database.
type issue256Connector struct {
	open           func(context.Context) (driver.Conn, error)
	fault          func(string) error
	stripForUpdate bool
	wrapTx         func(driver.Tx) driver.Tx
}

func (c *issue256Connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	return &issue256Conn{Conn: conn, connector: c}, nil
}
func (c *issue256Connector) Driver() driver.Driver {
	if c.stripForUpdate {
		return &sqlitedriver.Driver{}
	}
	return &mysql.MySQLDriver{}
}

func issue256LeaseDB(t *testing.T, f probeRegistryFixture, fault func(string) error, wrapTx ...func(driver.Tx) driver.Tx) *bun.DB {
	t.Helper()
	connector := &issue256Connector{stripForUpdate: f.engine == "sqlite", fault: fault}
	if len(wrapTx) > 0 {
		connector.wrapTx = wrapTx[0]
	}
	if f.engine == "sqlite" {
		connector.open = func(context.Context) (driver.Conn, error) { return (&sqlitedriver.Driver{}).Open(f.dsn) }
	} else {
		cfg, err := mysql.ParseDSN(f.dsn)
		if err != nil {
			t.Fatal("invalid disposable database configuration")
		}
		mc, err := mysql.NewConnector(cfg)
		if err != nil {
			t.Fatal("invalid disposable database connector")
		}
		connector.open = mc.Connect
	}
	db := bun.NewDB(sql.OpenDB(connector), f.db.Dialect())
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type issue256Conn struct {
	driver.Conn
	connector *issue256Connector
}

func (c *issue256Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	if c.connector.wrapTx != nil {
		tx = c.connector.wrapTx(tx)
	}
	return tx, nil
}
func (c *issue256Conn) sql(query string) (string, error) {
	if err := c.connector.fault(query); err != nil {
		return "", err
	}
	if c.connector.stripForUpdate {
		query = strings.TrimSuffix(query, " FOR UPDATE")
	}
	return query, nil
}
func (c *issue256Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	query, err := c.sql(query)
	if err != nil {
		return nil, err
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *issue256Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	query, err := c.sql(query)
	if err != nil {
		return nil, err
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

// issue256CommitFailureTx ends the genuine transaction without committing any
// writes, then reports an ambiguous commit error. It never simulates success.
type issue256CommitFailureTx struct {
	driver.Tx
	failure error
	commits *int
}

func (tx *issue256CommitFailureTx) Commit() error {
	*tx.commits++
	if err := tx.Tx.Rollback(); err != nil {
		return err
	}
	return tx.failure
}

func TestIssue256LeaseAmbiguousCommitDoesNotReplay(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, operation := range []string{"claim", "refresh"} {
				t.Run(operation, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					id := f.monitor(t)
					ctx := context.Background()
					if operation == "refresh" {
						if _, err := newEngineMonitorRepo(f).ClaimBatch(ctx, "issue256", 1000, time.Minute); err != nil {
							t.Fatal(err)
						}
						if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET leased_at = ? WHERE id = ?", time.Now().UTC().Add(-30*time.Second), id); err != nil {
							t.Fatal(err)
						}
					}
					var ownerBefore sql.NullString
					var leasedBefore sql.NullTime
					if err := f.db.QueryRowContext(ctx, "SELECT worker_id, leased_at FROM monitors WHERE id = ?", id).Scan(&ownerBefore, &leasedBefore); err != nil {
						t.Fatal(err)
					}
					epochBefore := leaseEpochOf(t, f, id)
					failure := errors.New("injected ambiguous commit failure")
					attempts, commits, writes := 0, 0, 0
					db := issue256LeaseDB(t, f, func(query string) error {
						if strings.HasPrefix(query, "UPDATE") {
							writes++
						}
						return nil
					}, func(tx driver.Tx) driver.Tx {
						attempts++
						return &issue256CommitFailureTx{Tx: tx, failure: failure, commits: &commits}
					})
					repo := mariadb.NewMonitorRepo(db)
					var err error
					if operation == "claim" {
						claimed, claimErr := repo.ClaimBatch(ctx, "issue256", 1000, time.Minute)
						err = claimErr
						if len(claimed) != 0 {
							t.Fatal("failed commit leaked claim candidates")
						}
					} else {
						var count int64
						count, err = repo.RefreshLease(ctx, "issue256", time.Minute)
						if count != 0 {
							t.Fatal("failed commit reported successful refreshes")
						}
					}
					if !errors.Is(err, failure) {
						t.Fatalf("error=%v, want preserved commit failure", err)
					}
					if attempts != 1 || commits != 1 || writes != 1 {
						t.Fatalf("attempts=%d commits=%d writes=%d, want exactly one each", attempts, commits, writes)
					}
					var ownerAfter sql.NullString
					var leasedAfter sql.NullTime
					if err := f.db.QueryRowContext(ctx, "SELECT worker_id, leased_at FROM monitors WHERE id = ?", id).Scan(&ownerAfter, &leasedAfter); err != nil {
						t.Fatal(err)
					}
					if ownerAfter != ownerBefore || leasedAfter.Valid != leasedBefore.Valid || !leasedAfter.Time.Equal(leasedBefore.Time) || leaseEpochOf(t, f, id) != epochBefore {
						t.Fatal("failed commit changed persisted lease authority")
					}
				})
			}
		})
	}
}

func TestIssue256LeaseDeadlockRetry(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, operation := range []string{"claim_select", "claim_update", "refresh_select", "refresh_update", "release"} {
				t.Run(operation, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					id := f.monitor(t)
					ctx := context.Background()
					repo := newEngineMonitorRepo(f)
					if strings.HasPrefix(operation, "refresh") || operation == "release" {
						if _, err := repo.ClaimBatch(ctx, "issue256", 1000, time.Minute); err != nil {
							t.Fatal(err)
						}
						if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET leased_at = ? WHERE id = ?", time.Now().UTC().Add(-30*time.Second), id); err != nil {
							t.Fatal(err)
						}
					}
					attempts := 0
					db := issue256LeaseDB(t, f, func(query string) error {
						selectQuery := strings.HasPrefix(query, "SELECT") && strings.HasSuffix(query, " FOR UPDATE")
						updateQuery := strings.HasPrefix(query, "UPDATE")
						if (strings.HasSuffix(operation, "select") && selectQuery) || ((strings.HasSuffix(operation, "update") || operation == "release") && updateQuery) {
							attempts++
							if attempts == 1 {
								return &mysql.MySQLError{Number: 1213, Message: "injected deadlock victim"}
							}
						}
						return nil
					})
					leaseRepo := mariadb.NewMonitorRepo(db)
					switch {
					case strings.HasPrefix(operation, "claim"):
						claimed, err := leaseRepo.ClaimBatch(ctx, "issue256", 1000, time.Minute)
						if err != nil {
							t.Fatal(err)
						}
						if len(claimed) != 1 || claimed[0].ID != id {
							t.Fatalf("claimed wrong batch: %v", claimed)
						}
					case strings.HasPrefix(operation, "refresh"):
						n, err := leaseRepo.RefreshLease(ctx, "issue256", time.Minute)
						if err != nil || n != 1 {
							t.Fatalf("refresh: count=%d error=%v", n, err)
						}
					case operation == "release":
						n, err := leaseRepo.ReleaseLeases(ctx, "issue256")
						if err != nil || n != 1 {
							t.Fatalf("release: count=%d error=%v", n, err)
						}
					}
					if attempts != 2 {
						t.Fatalf("attempts=%d, want 2", attempts)
					}
					if got := leaseEpochOf(t, f, id); got != 1 {
						t.Fatalf("epoch=%d, want exactly one acquisition", got)
					}
					var worker sql.NullString
					if err := f.db.QueryRowContext(ctx, "SELECT worker_id FROM monitors WHERE id = ?", id).Scan(&worker); err != nil {
						t.Fatal(err)
					}
					if (operation == "release" && worker.Valid) || (operation != "release" && worker.String != "issue256") {
						t.Fatalf("incorrect persisted lease owner: %v", worker)
					}
				})
			}
		})
	}
}

func TestIssue256LeaseRetryRebuildsAuthority(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, operation := range []string{"claim", "refresh"} {
				t.Run(operation, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					id := f.monitor(t)
					ctx := context.Background()
					if operation == "refresh" {
						if _, err := newEngineMonitorRepo(f).ClaimBatch(ctx, "issue256", 1000, time.Minute); err != nil {
							t.Fatal(err)
						}
					}
					reads, writes := 0, 0
					db := issue256LeaseDB(t, f, func(query string) error {
						if strings.HasPrefix(query, "SELECT") && strings.HasSuffix(query, " FOR UPDATE") {
							reads++
							if reads == 2 {
								// The first transaction is rolled back, and its replacement
								// has not read or locked anything yet. Change the real row
								// through another connection, not the injected lease handle.
								var err error
								if operation == "claim" {
									_, err = f.db.ExecContext(ctx, "UPDATE monitors SET active = FALSE WHERE id = ?", id)
								} else {
									_, err = f.db.ExecContext(ctx, "UPDATE monitors SET leased_at = ? WHERE id = ?", time.Now().UTC().Add(-2*time.Minute), id)
								}
								if err != nil {
									return err
								}
							}
						}
						if strings.HasPrefix(query, "UPDATE") {
							writes++
							if writes == 1 {
								return &mysql.MySQLError{Number: 1213}
							}
						}
						return nil
					})
					repo := mariadb.NewMonitorRepo(db)
					wantEpoch := int64(0)
					if operation == "claim" {
						claimed, err := repo.ClaimBatch(ctx, "issue256", 1000, time.Minute)
						if err != nil || len(claimed) != 0 {
							t.Fatalf("retry leaked aborted candidates: count=%d error=%v", len(claimed), err)
						}
					} else {
						wantEpoch = 1
						n, err := repo.RefreshLease(ctx, "issue256", time.Minute)
						if err != nil || n != 0 {
							t.Fatalf("retry revived expired lease: count=%d error=%v", n, err)
						}
						var valid int
						if err := f.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM monitors WHERE id = ? AND leased_at >= ?", id, time.Now().UTC().Add(-time.Minute)).Scan(&valid); err != nil || valid != 0 {
							t.Fatalf("expired lease was extended: valid=%d error=%v", valid, err)
						}
					}
					if reads != 2 {
						t.Fatalf("locking reads=%d, want fresh read after rollback", reads)
					}
					if got := leaseEpochOf(t, f, id); got != wantEpoch {
						t.Fatalf("epoch=%d, want %d", got, wantEpoch)
					}
				})
			}
		})
	}
}

// A deadlock budget must not become an unbounded loop or a general-purpose
// error suppressor. Domain sentinels and non-deadlock engine failures retain
// their identities, and cancellation stops the loop before another query.
func TestIssue256LeaseRetryFailureContract(t *testing.T) {
	for _, operation := range []string{"claim", "refresh", "release"} {
		t.Run(operation, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				failure  error
				attempts int
				cancel   bool
			}{
				{"exhausted", &mysql.MySQLError{Number: 1213}, 4, false},
				{"lock_timeout", &mysql.MySQLError{Number: 1205}, 1, false},
				{"record_changed", &mysql.MySQLError{Number: 1020}, 1, false},
				{"domain_error", ports.ErrNotFound, 1, false},
				{"canceled", &mysql.MySQLError{Number: 1213}, 1, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newProbeRegistryFixture(t, "sqlite")
					id := f.monitor(t)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if operation != "claim" {
						if _, err := newEngineMonitorRepo(f).ClaimBatch(ctx, "issue256", 1000, time.Minute); err != nil {
							t.Fatal(err)
						}
					}
					attempts := 0
					db := issue256LeaseDB(t, f, func(string) error {
						attempts++
						if tc.cancel {
							cancel()
						}
						return tc.failure
					})
					repo := mariadb.NewMonitorRepo(db)
					var err error
					switch operation {
					case "claim":
						var count int
						claimed, claimErr := repo.ClaimBatch(ctx, "issue256", 1000, time.Minute)
						count, err = len(claimed), claimErr
						if count != 0 {
							t.Fatal("failed claim leaked candidates")
						}
					case "refresh":
						var count int64
						count, err = repo.RefreshLease(ctx, "issue256", time.Minute)
						if count != 0 {
							t.Fatal("failed refresh reported success")
						}
					case "release":
						var count int64
						count, err = repo.ReleaseLeases(ctx, "issue256")
						if count != 0 {
							t.Fatal("failed release reported success")
						}
					}
					wantErr := tc.failure
					if tc.cancel {
						wantErr = context.Canceled
					}
					if !errors.Is(err, wantErr) {
						t.Fatalf("error=%v, want preserved %v", err, wantErr)
					}
					if attempts != tc.attempts {
						t.Fatalf("attempts=%d, want %d", attempts, tc.attempts)
					}
					wantEpoch := int64(1)
					if operation == "claim" {
						wantEpoch = 0
					}
					if got := leaseEpochOf(t, f, id); got != wantEpoch {
						t.Fatalf("failed operation changed epoch: %d", got)
					}
				})
			}
		})
	}
}
