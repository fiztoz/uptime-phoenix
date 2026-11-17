package repository_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
)

func TestSQLiteReconnectPreservesAssignmentCascade(t *testing.T) {
	for name, suffix := range map[string]string{"custom_e2e": "?cache=shared", "pragma_control": "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"} {
		t.Run(name, func(t *testing.T) {
			for _, trigger := range []string{"eviction", "canceled_query"} {
				t.Run(trigger, func(t *testing.T) {
					db, err := sqlite.NewDB("file:" + filepath.Join(t.TempDir(), "reopen.db") + suffix)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = db.Close() }()
					if err = repository.RunMigrations(db.DB, "sqlite"); err != nil {
						t.Fatal(err)
					}
					f := probeRegistryFixture{db: db, engine: "sqlite", assignments: sqlite.NewProbeAssignmentRepo(db)}
					id := f.monitor(t)
					if _, err = f.assignments.InitializeLocal(t.Context(), id); err != nil {
						t.Fatal(err)
					}
					reader := repository.NewLocalProbeConfigSourceStore(db)
					if _, err = reader.ReadLocal(t.Context()); err != nil {
						t.Fatal("before", err)
					}
					var fk int
					if err = db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
						t.Fatal("initial FK", fk, err)
					}
					var physicalBefore any
					observed, err := db.Conn(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if err := observed.Raw(func(c any) error { physicalBefore = c; return nil }); err != nil {
						t.Fatal(err)
					}
					_ = observed.Close()
					if trigger == "eviction" {
						conn, err := db.Conn(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						_ = conn.Raw(func(any) error { return driver.ErrBadConn })
						_ = conn.Close()
					} else {
						ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
						var result int64
						err = db.QueryRowContext(ctx, "WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000000000) SELECT SUM(x) FROM n").Scan(&result)
						cancel()
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("long query did not reach cancellation: %v", err)
						}
					}
					var physicalAfter any
					observed, err = db.Conn(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if err := observed.Raw(func(c any) error { physicalAfter = c; return nil }); err != nil {
						t.Fatal(err)
					}
					_ = observed.Close()
					if physicalBefore == physicalAfter {
						t.Fatal("query did not cause physical connection replacement")
					}
					if err = db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
						t.Fatal(err)
					}
					t.Log("foreign_keys after replacement", fk)
					if err = sqlite.NewMonitorRepo(db).Delete(t.Context(), id); err != nil {
						t.Fatal(err)
					}
					var orphan int
					if err = db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM monitor_probe_assignments a LEFT JOIN monitors m ON m.id=a.monitor_id WHERE m.id IS NULL").Scan(&orphan); err != nil {
						t.Fatal(err)
					}
					_, sourceErr := reader.ReadLocal(t.Context())
					t.Logf("orphan assignments=%d; source read=%v", orphan, sourceErr)
					if fk != 1 || orphan != 0 || sourceErr != nil {
						t.Fatalf("replacement lost durable invariants: fk=%d orphan=%d source=%v", fk, orphan, sourceErr)
					}
				})
			}
		})
	}
}
