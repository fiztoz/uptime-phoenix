package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// lockedBuffer is a goroutine-safe log sink for the migration logger.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLockPolicy caps each lock wait at 1 s, the smallest MariaDB accepts.
func testLockPolicy(attempts int, backoff time.Duration, sink *lockedBuffer) MigrationLockPolicy {
	return MigrationLockPolicy{
		LockWait:    time.Second,
		Attempts:    attempts,
		BaseBackoff: backoff,
		MaxBackoff:  backoff,
		Logger:      slog.New(slog.NewJSONHandler(sink, nil)),
	}
}

// reopenMariaDB037Index puts a fully migrated schema back to "037 not yet
// recorded, final heartbeats index missing", so the next run must ALTER
// heartbeats again: the exact statement that stalled in the M6 rehearsal.
func reopenMariaDB037Index(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if err := RunMigrations(db, "mariadb"); err != nil {
		t.Fatalf("baseline migrations: %v", err)
	}
	for _, stmt := range []string{
		"DROP INDEX idx_hb_monitor_probe_time ON heartbeats",
		"DELETE FROM _migrations WHERE filename='037_probe_heartbeat.up.sql'",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// holdHeartbeatsMetadataLock opens a transaction that reads heartbeats and
// keeps it open, like a long dashboard query or an old pod's open transaction.
func holdHeartbeatsMetadataLock(t *testing.T, ctx context.Context, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM heartbeats").Scan(&n); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

func mariaDBIndexExists(t *testing.T, ctx context.Context, db *sql.DB, table, index string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND INDEX_NAME=?", table, index).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestMariaDBMigrationLockWaitIsCappedAndRetried(t *testing.T) {
	db := openReadinessMariaDB(t)
	ctx := context.Background()
	reopenMariaDB037Index(t, ctx, db)
	holder := holdHeartbeatsMetadataLock(t, ctx, db)

	sink := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- RunMigrationsWithPolicy(db, "mariadb", testLockPolicy(6, 300*time.Millisecond, sink))
	}()

	// While the ALTER waits for the metadata lock, every new heartbeats reader
	// queues behind it. The cap must bound that queue to about one lock wait.
	const hold = 4 * time.Second
	var worst time.Duration
	deadline := time.Now().Add(hold)
	for time.Now().Before(deadline) {
		qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		start := time.Now()
		var n int
		err := db.QueryRowContext(qctx, "SELECT COUNT(*) FROM heartbeats WHERE monitor_id = 1").Scan(&n)
		cancel()
		if err != nil {
			t.Fatalf("reader during upgrade: %v", err)
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migration after the long transaction ended: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("migration did not finish after the long transaction ended")
	}
	if worst > 2500*time.Millisecond {
		t.Fatalf("a heartbeats reader waited %s behind the upgrade; the 1s lock-wait cap should bound it", worst)
	}
	logs := sink.String()
	if !strings.Contains(logs, "migration lock wait exceeded; retrying") || !strings.Contains(logs, "037_probe_heartbeat.up.sql") {
		t.Fatalf("expected a logged retry for 037, got logs:\n%s", logs)
	}
	if !mariaDBIndexExists(t, ctx, db, "heartbeats", "idx_hb_monitor_probe_time") {
		t.Fatal("037 index missing after the retried upgrade")
	}
	var ledger int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM _migrations WHERE filename='037_probe_heartbeat.up.sql'").Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("037 ledger rows=%d err=%v", ledger, err)
	}
}

func TestMariaDBMigrationLockWaitRetriesAreBounded(t *testing.T) {
	db := openReadinessMariaDB(t)
	ctx := context.Background()
	reopenMariaDB037Index(t, ctx, db)
	holder := holdHeartbeatsMetadataLock(t, ctx, db)

	sink := &lockedBuffer{}
	start := time.Now()
	err := RunMigrationsWithPolicy(db, "mariadb", testLockPolicy(3, 200*time.Millisecond, sink))
	elapsed := time.Since(start)
	if !errors.Is(err, ErrMigrationLockWaitExhausted) {
		t.Fatalf("err = %v, want ErrMigrationLockWaitExhausted", err)
	}
	if !strings.Contains(err.Error(), "037_probe_heartbeat.up.sql") || !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("error must name the migration and the attempts: %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("bounded retries took %s", elapsed)
	}
	if got := strings.Count(sink.String(), "migration lock wait exceeded; retrying"); got != 2 {
		t.Fatalf("logged %d retries, want 2:\n%s", got, sink.String())
	}
	if mariaDBIndexExists(t, ctx, db, "heartbeats", "idx_hb_monitor_probe_time") {
		t.Fatal("index created although every attempt timed out")
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	// A restart after the long transaction ends resumes 037 cleanly.
	if err := RunMigrationsWithPolicy(db, "mariadb", testLockPolicy(3, 200*time.Millisecond, sink)); err != nil {
		t.Fatalf("restart after the holder committed: %v", err)
	}
	if !mariaDBIndexExists(t, ctx, db, "heartbeats", "idx_hb_monitor_probe_time") {
		t.Fatal("037 index missing after restart")
	}
}

func TestMariaDBMigrationRestoresSessionLockWaits(t *testing.T) {
	db := openReadinessMariaDB(t)
	// One pooled connection: the migration session goes back to the pool and
	// the next caller must see the server defaults, not the migration cap.
	db.SetMaxOpenConns(1)
	sink := &lockedBuffer{}
	if err := RunMigrationsWithPolicy(db, "mariadb", testLockPolicy(2, 100*time.Millisecond, sink)); err != nil {
		t.Fatal(err)
	}
	var restored int
	if err := db.QueryRow("SELECT @@SESSION.lock_wait_timeout = @@GLOBAL.lock_wait_timeout AND @@SESSION.innodb_lock_wait_timeout = @@GLOBAL.innodb_lock_wait_timeout").Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatal("pooled connection kept the migration lock-wait cap")
	}
}

func openSQLiteBusyFixture(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "busy.db") + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(200)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	other, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	holder, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	// Another process holds the write lock (an old pod mid-transaction).
	if _, err := holder.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	return db, holder
}

func TestSQLiteMigrationBusyIsRetried(t *testing.T) {
	db, holder := openSQLiteBusyFixture(t)
	sink := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- RunMigrationsWithPolicy(db, "sqlite", testLockPolicy(10, 150*time.Millisecond, sink))
	}()
	time.Sleep(time.Second)
	if _, err := holder.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("migration after the writer released the lock: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("migration did not finish after the writer released the lock")
	}
	if !strings.Contains(sink.String(), "migration lock wait exceeded; retrying") {
		t.Fatalf("expected a logged busy retry, got:\n%s", sink.String())
	}
	entries, err := sqliteMigrations.ReadDir("sqlite/migrations")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			want++
		}
	}
	var got int
	if err := db.QueryRow("SELECT COUNT(*) FROM _migrations").Scan(&got); err != nil || got != want {
		t.Fatalf("applied %d migrations, want %d (err %v)", got, want, err)
	}
}

func TestSQLiteMigrationBusyRetriesAreBounded(t *testing.T) {
	db, holder := openSQLiteBusyFixture(t)
	sink := &lockedBuffer{}
	start := time.Now()
	err := RunMigrationsWithPolicy(db, "sqlite", testLockPolicy(3, 100*time.Millisecond, sink))
	if !errors.Is(err, ErrMigrationLockWaitExhausted) {
		t.Fatalf("err = %v, want ErrMigrationLockWaitExhausted", err)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("error must name the attempts: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("bounded retries took %s", elapsed)
	}
	if got := strings.Count(sink.String(), "migration lock wait exceeded; retrying"); got != 2 {
		t.Fatalf("logged %d retries, want 2:\n%s", got, sink.String())
	}
	if _, err := holder.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := RunMigrationsWithPolicy(db, "sqlite", testLockPolicy(3, 100*time.Millisecond, sink)); err != nil {
		t.Fatalf("restart after the writer released the lock: %v", err)
	}
}

// countingLockCatalog records ExecContext calls and always returns err.
type countingLockCatalog struct {
	execs int
	err   error
}

func (c *countingLockCatalog) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	c.execs++
	return nil, c.err
}

func (c *countingLockCatalog) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("unexpected QueryRowContext")
}

func (c *countingLockCatalog) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("unexpected QueryContext")
}

// TestMariaDBLedgerCreateRetryIsNotNested proves CREATE _migrations is wrapped
// once: a lockRetryExecutor already retries, and createMigrationsLedger must
// not wrap that again (which would allow up to Attempts² statement tries).
func TestMariaDBLedgerCreateRetryIsNotNested(t *testing.T) {
	sink := &lockedBuffer{}
	policy := testLockPolicy(3, time.Millisecond, sink)
	stub := &countingLockCatalog{err: &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded; try restarting transaction"}}
	exec := &lockRetryExecutor{mariaDBMigrationCatalog: stub, policy: policy}
	err := createMigrationsLedger(context.Background(), exec, policy, "CREATE TABLE IF NOT EXISTS _migrations (id INT)")
	if !errors.Is(err, ErrMigrationLockWaitExhausted) {
		t.Fatalf("err = %v, want ErrMigrationLockWaitExhausted", err)
	}
	if stub.execs != 3 {
		t.Fatalf("ExecContext called %d times, want 3 (configured attempts, not attempts×attempts)", stub.execs)
	}
	if got := strings.Count(sink.String(), "migration lock wait exceeded; retrying"); got != 2 {
		t.Fatalf("logged %d retries, want 2:\n%s", got, sink.String())
	}
}

func TestMigrationNonLockSQLErrorIsNotRetried(t *testing.T) {
	t.Run("mariadb syntax via lockRetryExecutor", func(t *testing.T) {
		sink := &lockedBuffer{}
		policy := testLockPolicy(6, time.Millisecond, sink)
		want := &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}
		stub := &countingLockCatalog{err: want}
		exec := &lockRetryExecutor{mariaDBMigrationCatalog: stub, policy: policy, migration: "037_probe_heartbeat.up.sql"}
		_, err := exec.ExecContext(context.Background(), "ALTER TABLE heartbeats ADD INDEX idx_bad (")
		if !errors.As(err, new(*mysql.MySQLError)) || err.Error() != want.Error() {
			t.Fatalf("err = %v, want the syntax error", err)
		}
		if errors.Is(err, ErrMigrationLockWaitExhausted) {
			t.Fatalf("syntax error was wrapped as exhausted: %v", err)
		}
		if stub.execs != 1 {
			t.Fatalf("ExecContext called %d times, want 1", stub.execs)
		}
		if strings.Contains(sink.String(), "migration lock wait exceeded; retrying") {
			t.Fatalf("logged a lock-wait retry for a syntax error:\n%s", sink.String())
		}
	})

	t.Run("sqlite syntax via migration transaction", func(t *testing.T) {
		dsn := "file:" + filepath.Join(t.TempDir(), "bad.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(200)"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(`CREATE TABLE _migrations (id INTEGER PRIMARY KEY AUTOINCREMENT, filename TEXT NOT NULL UNIQUE, applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
			t.Fatal(err)
		}
		sink := &lockedBuffer{}
		policy := testLockPolicy(6, time.Millisecond, sink)
		ctx := context.Background()
		attempts := 0
		err = withMigrationLockRetry(ctx, policy, "999_bad.up.sql", "transaction", func() error {
			attempts++
			return applyMigrationWithExecutor(ctx, db, db, "sqlite", "999_bad.up.sql", "CREATE TABLE foo (;")
		})
		if err == nil {
			t.Fatal("expected a SQLite syntax error")
		}
		if errors.Is(err, ErrMigrationLockWaitExhausted) {
			t.Fatalf("syntax error was wrapped as exhausted: %v", err)
		}
		if attempts != 1 {
			t.Fatalf("transaction attempted %d times, want 1", attempts)
		}
		if strings.Contains(sink.String(), "migration lock wait exceeded; retrying") {
			t.Fatalf("logged a lock-wait retry for a syntax error:\n%s", sink.String())
		}
	})

	t.Run("mariadb live syntax", func(t *testing.T) {
		db := openReadinessMariaDB(t)
		ctx := context.Background()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		sink := &lockedBuffer{}
		policy := testLockPolicy(6, time.Millisecond, sink)
		exec := &lockRetryExecutor{mariaDBMigrationCatalog: conn, policy: policy, migration: "999_bad.up.sql"}
		start := time.Now()
		_, err = exec.ExecContext(ctx, "CREATE TABLE foo (;")
		if err == nil {
			t.Fatal("expected a MariaDB syntax error")
		}
		var my *mysql.MySQLError
		if !errors.As(err, &my) || my.Number == 1205 || my.Number == 1213 {
			t.Fatalf("err = %v, want a non-lock MySQL error", err)
		}
		if errors.Is(err, ErrMigrationLockWaitExhausted) {
			t.Fatalf("syntax error was wrapped as exhausted: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("non-lock error took %s, suggesting retries", elapsed)
		}
		if strings.Contains(sink.String(), "migration lock wait exceeded; retrying") {
			t.Fatalf("logged a lock-wait retry for a syntax error:\n%s", sink.String())
		}
	})
}
