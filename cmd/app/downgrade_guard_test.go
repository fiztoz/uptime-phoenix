package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

// TestMainRefusesSchemaNewerThanBinary runs the real entry point against a
// database that a newer release already migrated: it must exit non-zero with
// the downgrade message before serving anything.
func TestMainRefusesSchemaNewerThanBinary(t *testing.T) {
	if os.Getenv("PHOENIX_DOWNGRADE_CHILD") == "1" {
		main()
		return
	}
	path := filepath.Join(t.TempDir(), "phoenix.db")
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.RunMigrations(db, "sqlite"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO _migrations (filename) VALUES ('999_from_a_newer_release.up.sql')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// Without the guard the child would start serving; bound it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainRefusesSchemaNewerThanBinary$")
	cmd.Env = append(os.Environ(),
		"PHOENIX_DOWNGRADE_CHILD=1",
		"DB_ENGINE=sqlite",
		"DB_DSN="+dsn,
		"JWT_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef",
		"PORT=0",
		"HOST=127.0.0.1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if ctx.Err() != nil {
		t.Fatalf("binary kept running against a newer schema; stderr:\n%s", stderr.String())
	}
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("expected a non-zero exit, got err=%v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "database schema is newer than this binary") ||
		!strings.Contains(stderr.String(), "999_from_a_newer_release.up.sql") {
		t.Fatalf("stderr does not explain the refusal:\n%s", stderr.String())
	}
}
