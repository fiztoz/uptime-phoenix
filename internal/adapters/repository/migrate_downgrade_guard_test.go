package repository

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// futureMigration is what a newer release would have recorded.
const futureMigration = "999_from_a_newer_release.up.sql"

func TestMigrationRefusesSchemaNewerThanBinary(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			var db *sql.DB
			if engine == "mariadb" {
				db = openReadinessMariaDB(t)
			} else {
				var err error
				db, err = sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "guard.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
			}
			ctx := context.Background()
			if err := RunMigrations(db, engine); err != nil {
				t.Fatalf("baseline migrations: %v", err)
			}
			// A newer binary ran first and recorded a migration this one lacks.
			if _, err := db.ExecContext(ctx, "INSERT INTO _migrations (filename) VALUES (?)", futureMigration); err != nil {
				t.Fatal(err)
			}
			var before int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM _migrations").Scan(&before); err != nil {
				t.Fatal(err)
			}

			err := RunMigrations(db, engine)
			if !errors.Is(err, ErrSchemaNewerThanBinary) {
				t.Fatalf("err = %v, want ErrSchemaNewerThanBinary", err)
			}
			latest := latestEmbeddedMigration(t, engine)
			for _, want := range []string{futureMigration, latest, "Refusing to start"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not mention %q", err, want)
				}
			}
			var after int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM _migrations").Scan(&after); err != nil || after != before {
				t.Fatalf("ledger changed: before=%d after=%d err=%v", before, after, err)
			}

			// Same or older recorded schema still starts normally.
			if _, err := db.ExecContext(ctx, "DELETE FROM _migrations WHERE filename = ?", futureMigration); err != nil {
				t.Fatal(err)
			}
			if err := RunMigrations(db, engine); err != nil {
				t.Fatalf("binary at the schema version must start: %v", err)
			}
		})
	}
}

func latestEmbeddedMigration(t *testing.T, engine string) string {
	t.Helper()
	fsys, dir := sqliteMigrations, "sqlite/migrations"
	if engine == "mariadb" {
		fsys, dir = mariadbMigrations, "mariadb/migrations"
	}
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	latest := ""
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") && e.Name() > latest {
			latest = e.Name()
		}
	}
	return latest
}
