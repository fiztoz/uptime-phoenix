package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestMariaDB037ResumesEveryCommittedStatementPrefix(t *testing.T) {
	db := openReadinessMariaDB(t)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	source, err := mariadbMigrations.ReadFile("mariadb/migrations/037_probe_heartbeat.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements := splitMigrationStatements(string(source))
	if len(statements) < 10 {
		t.Fatalf("migration 037 unexpectedly has only %d statements", len(statements))
	}
	for prefix := 0; prefix <= len(statements); prefix++ {
		reset037Fixture(t, ctx, conn)
		for i := 0; i < prefix; i++ {
			if _, err := conn.ExecContext(ctx, statements[i]); err != nil {
				t.Fatalf("seed interrupted prefix %d statement %d: %v", prefix, i+1, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `CREATE TABLE _migrations (id INT AUTO_INCREMENT PRIMARY KEY, filename VARCHAR(255) NOT NULL UNIQUE, applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
			t.Fatal(err)
		}
		if err := applyMariaDB037AndRecord(ctx, conn, "037_probe_heartbeat.up.sql", string(source)); err != nil {
			t.Fatalf("resume prefix %d: %v", prefix, err)
		}
		assert037FixturePreserved(t, ctx, conn, prefix)
		var ledger int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM _migrations WHERE filename='037_probe_heartbeat.up.sql'").Scan(&ledger); err != nil || ledger != 1 {
			t.Fatalf("prefix %d ledger count=%d err=%v", prefix, ledger, err)
		}
	}
}

func TestMariaDB037RejectsMalformedExistingIndex(t *testing.T) {
	db := openReadinessMariaDB(t)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	reset037Fixture(t, ctx, conn)
	if _, err := conn.ExecContext(ctx, "CREATE INDEX idx_hb_monitor_probe_time ON heartbeats (monitor_id, time)"); err != nil {
		t.Fatal(err)
	}
	source, err := mariadbMigrations.ReadFile("mariadb/migrations/037_probe_heartbeat.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMariaDB037(ctx, conn, string(source)); err == nil || !strings.Contains(err.Error(), "unexpected index") {
		t.Fatalf("malformed existing index was not rejected clearly: %v", err)
	}
}

func TestMariaDBMigrationOwnershipBlocksUntilOwnerReleases(t *testing.T) {
	db := openReadinessMariaDB(t)
	ctx := context.Background()
	owner, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lockName, err := mariaDBMigrationLockName(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	var acquired sql.NullInt64
	if err := owner.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", lockName).Scan(&acquired); err != nil || !acquired.Valid || acquired.Int64 != 1 {
		t.Fatalf("acquire fixture migration lock: valid=%v value=%d err=%v", acquired.Valid, acquired.Int64, err)
	}
	completed := make(chan error, 1)
	go func() { completed <- RunMigrations(db, "mariadb") }()
	select {
	case err := <-completed:
		t.Fatalf("migration returned while another session owned the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := owner.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", lockName).Scan(&acquired); err != nil || !acquired.Valid || acquired.Int64 != 1 {
		t.Fatalf("release fixture migration lock: valid=%v value=%d err=%v", acquired.Valid, acquired.Int64, err)
	}
	_ = owner.Close()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("migration after ownership release: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("migration did not proceed after owner released the lock")
	}
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- RunMigrations(db, "mariadb") }()
	}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent migration restart: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("concurrent migration restart timed out")
		}
	}
	var total, distinct int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*), COUNT(DISTINCT filename) FROM _migrations").Scan(&total, &distinct); err != nil || total != 74 || distinct != 74 {
		t.Fatalf("concurrent restart ledger: total=%d distinct=%d err=%v", total, distinct, err)
	}
}

func TestMariaDBMigrationDiscardsConnectionWhenLockReleaseFails(t *testing.T) {
	state := &releaseFailState{}
	db := sql.OpenDB(releaseFailConnector{state: state})
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = runMariaDBMigrations(context.Background(), db, conn)
	if err == nil || !strings.Contains(err.Error(), "release MariaDB migration lock") {
		t.Fatalf("expected lock release failure, got %v", err)
	}
	if !state.releaseCalled || state.closed == 0 {
		t.Fatalf("release failure did not discard connection: released=%v closed=%d", state.releaseCalled, state.closed)
	}
}

func openReadinessMariaDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if dsn == "" {
		t.Skip("TEST_MARIADB_DSN is unset; skipping real MariaDB migration readiness contract")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse TEST_MARIADB_DSN: %v", err)
	}
	if config.DBName == "phoenix" || config.DBName == "" {
		t.Fatal("TEST_MARIADB_DSN must name a disposable non-phoenix database")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open TEST_MARIADB_DSN: %v", err)
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Fatalf("ping disposable MariaDB: %v", err)
	}
	name := fmt.Sprintf("phoenix_migration_%x", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated migration fixture database: %v", err)
	}
	fixture := *config
	fixture.DBName = name
	db, err := sql.Open("mysql", fixture.FormatDSN())
	if err != nil {
		t.Fatalf("open isolated migration fixture database: %v", err)
	}
	db.SetMaxOpenConns(8)
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.Exec("DROP DATABASE `" + name + "`")
		_ = admin.Close()
	})
	return db
}

func reset037Fixture(t *testing.T, ctx context.Context, conn *sql.Conn) {
	t.Helper()
	for _, table := range []string{"heartbeats", "heartbeat_1m", "heartbeat_1h", "heartbeat_1d"} {
		if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "DROP TABLE IF EXISTS _migrations"); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE heartbeats (
id BIGINT NOT NULL, monitor_id BIGINT NOT NULL, status TINYINT NOT NULL, time TIMESTAMP NOT NULL,
msg TEXT NULL, ping INT NULL, duration INT NOT NULL DEFAULT 0, important BOOLEAN NOT NULL DEFAULT FALSE,
down_count INT NOT NULL DEFAULT 0, PRIMARY KEY (id,time), KEY idx_hb_monitor_time (monitor_id,time DESC)
) PARTITION BY RANGE (UNIX_TIMESTAMP(time)) (PARTITION p202609 VALUES LESS THAN (UNIX_TIMESTAMP('2026-10-01 00:00:00')), PARTITION pmax VALUES LESS THAN MAXVALUE)`,
		`CREATE TABLE heartbeat_1m (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, monitor_id BIGINT NOT NULL, bucket TIMESTAMP NOT NULL, up_count INT NOT NULL DEFAULT 0, down_count INT NOT NULL DEFAULT 0, pending_count INT NOT NULL DEFAULT 0, maint_count INT NOT NULL DEFAULT 0, avg_ping DOUBLE NULL, min_ping INT NULL, max_ping INT NULL, total_checks INT NOT NULL DEFAULT 0, UNIQUE KEY uq_monitor_bucket (monitor_id,bucket), KEY idx_1m_monitor_bucket (monitor_id,bucket DESC))`,
		`CREATE TABLE heartbeat_1h LIKE heartbeat_1m`,
		`CREATE TABLE heartbeat_1d LIKE heartbeat_1m`,
		`ALTER TABLE heartbeat_1h DROP INDEX idx_1m_monitor_bucket, ADD INDEX idx_1h_monitor_bucket (monitor_id,bucket DESC)`,
		`ALTER TABLE heartbeat_1d DROP INDEX idx_1m_monitor_bucket, ADD INDEX idx_1d_monitor_bucket (monitor_id,bucket DESC)`,
		`INSERT INTO heartbeats (id,monitor_id,status,time,msg) VALUES (1,9,1,'2026-09-29 00:00:00','keep')`,
		`INSERT INTO heartbeat_1m (monitor_id,bucket,up_count) VALUES (9,'2026-09-29 00:00:00',7)`,
		`INSERT INTO heartbeat_1h (monitor_id,bucket,up_count) VALUES (9,'2026-09-29 00:00:00',8)`,
		`INSERT INTO heartbeat_1d (monitor_id,bucket,up_count) VALUES (9,'2026-09-29 00:00:00',9)`,
	}
	for _, stmt := range statements {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("build migration 037 fixture: %v", err)
		}
	}
}

func assert037FixturePreserved(t *testing.T, ctx context.Context, conn *sql.Conn, prefix int) {
	t.Helper()
	var count int
	var msg string
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*), MAX(msg) FROM heartbeats WHERE id=1").Scan(&count, &msg); err != nil || count != 1 || msg != "keep" {
		t.Fatalf("prefix %d lost heartbeat: count=%d msg=%q err=%v", prefix, count, msg, err)
	}
	for table, want := range map[string]int{"heartbeat_1m": 7, "heartbeat_1h": 8, "heartbeat_1d": 9} {
		var got int
		if err := conn.QueryRowContext(ctx, "SELECT up_count FROM "+table+" WHERE monitor_id=9").Scan(&got); err != nil || got != want {
			t.Fatalf("prefix %d lost %s row: got=%d err=%v", prefix, table, got, err)
		}
	}
	var partitions int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='heartbeats'`).Scan(&partitions); err != nil || partitions != 2 {
		t.Fatalf("prefix %d changed heartbeat partitions: got=%d err=%v", prefix, partitions, err)
	}
	for table, index := range map[string]string{"heartbeat_1m": "idx_1m_monitor_probe_bucket", "heartbeat_1h": "idx_1h_monitor_probe_bucket", "heartbeat_1d": "idx_1d_monitor_probe_bucket"} {
		if err := ensureIndex(ctx, conn, table, index, []string{"monitor_id", "probe_id", "bucket"}, false, []string{"A", "A", "D"}, ""); err != nil {
			t.Fatalf("prefix %d invalid %s index: %v", prefix, table, err)
		}
	}
}

type releaseFailState struct {
	releaseCalled bool
	closed        int
}

type releaseFailConnector struct{ state *releaseFailState }

func (c releaseFailConnector) Connect(context.Context) (driver.Conn, error) {
	return &releaseFailConn{state: c.state}, nil
}
func (c releaseFailConnector) Driver() driver.Driver { return releaseFailDriver{} }

type releaseFailDriver struct{}

func (releaseFailDriver) Open(string) (driver.Conn, error) { return nil, fmt.Errorf("use connector") }

type releaseFailConn struct{ state *releaseFailState }

func (c *releaseFailConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare unsupported")
}
func (c *releaseFailConn) Close() error { c.state.closed++; return nil }
func (c *releaseFailConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("transactions unsupported")
}
func (c *releaseFailConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (c *releaseFailConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "SELECT DATABASE()"):
		return &releaseFailRows{values: [][]driver.Value{{"release-test"}}}, nil
	case strings.Contains(query, "GET_LOCK"):
		return &releaseFailRows{values: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(query, "RELEASE_LOCK"):
		c.state.releaseCalled = true
		return nil, fmt.Errorf("injected release failure")
	default:
		return &releaseFailRows{}, nil
	}
}

type releaseFailRows struct {
	values [][]driver.Value
	index  int
}

func (r *releaseFailRows) Columns() []string { return []string{"value"} }
func (r *releaseFailRows) Close() error      { return nil }
func (r *releaseFailRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}
