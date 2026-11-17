package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"

	modernsqlite "modernc.org/sqlite"
)

func TestNewDBPragmasSurviveConnectionReplacement(t *testing.T) {
	cases := map[string]func(string) string{
		"plain_path": func(p string) string { return p },
		"custom_e2e": func(p string) string { return "file:" + p + "?cache=shared" },
		"explicit_overrides": func(p string) string {
			return "file:" + p + "?_pragma=foreign_keys(0)&_pragma=busy_timeout(1)&_pragma=synchronous(OFF)"
		},
		"memory":       func(string) string { return ":memory:" },
		"named_memory": func(string) string { return "file:replacement?mode=memory&cache=shared" },
	}
	for name, dsn := range cases {
		t.Run(name, func(t *testing.T) {
			db, err := NewDB(dsn(filepath.Join(t.TempDir(), "db.sqlite")))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			for i := 0; i < 3; i++ {
				for pragma, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 10000, "synchronous": 1} {
					var got int
					if err := db.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Fatalf("connection %d %s=%d, want %d", i, pragma, got, want)
					}
				}
				var journal string
				if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journal); err != nil {
					t.Fatal(err)
				}
				wantJournal := "wal"
				if name == "memory" || name == "named_memory" {
					wantJournal = "memory"
				}
				if journal != wantJournal {
					t.Fatalf("journal=%s want %s", journal, wantJournal)
				}
				conn, err := db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				_ = conn.Close()
			}
		})
	}
}

func TestNewDBDoesNotChangeOtherSQLitePools(t *testing.T) {
	configured, err := NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configured.Close() }()
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var fk int
	if err := raw.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 0 {
		t.Fatal("NewDB globally changed unrelated driver connections")
	}
}

func TestInitializedConnectorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	base, err := modernsqlite.NewConnector(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := (initializedConnector{Connector: base}).Connect(ctx)
	if conn != nil || err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatal("canceled connect succeeded")
	}
}

type setupFailureConnector struct {
	driver.Connector
	conn *setupFailureConn
}

func (c setupFailureConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }

type setupFailureConn struct {
	calls, closed int
	cause         error
}

func (c *setupFailureConn) Prepare(string) (driver.Stmt, error) { return nil, c.cause }
func (c *setupFailureConn) Begin() (driver.Tx, error)           { return nil, c.cause }
func (c *setupFailureConn) Close() error                        { c.closed++; return nil }
func (c *setupFailureConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.calls++
	if c.calls == 2 {
		return nil, c.cause
	}
	return driver.RowsAffected(0), nil
}
func TestInitializedConnectorClosesSetupFailure(t *testing.T) {
	cause := errors.New("setup failed")
	raw := &setupFailureConn{cause: cause}
	conn, err := (initializedConnector{Connector: setupFailureConnector{conn: raw}}).Connect(t.Context())
	if conn != nil || !errors.Is(err, cause) || raw.closed != 1 || raw.calls != 2 {
		t.Fatalf("setup failure conn=%v err=%v closed=%d calls=%d", conn, err, raw.closed, raw.calls)
	}
}
