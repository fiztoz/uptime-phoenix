package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// setupProductionTestDB opens the database exactly as production does
// (NewDB: one connection, busy_timeout, WAL), so concurrency tests exercise
// the real serialization instead of a multi-connection pool.
func setupProductionTestDB(t *testing.T) *Repository {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := repository.RunMigrations(db.DB, "sqlite"); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewRepository(db)
}

// #92: concurrent first-user inserts elect exactly one winner.
func TestUserRepo_CreateFirstElectsOneWinner(t *testing.T) {
	repo := setupProductionTestDB(t)
	ctx := context.Background()

	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repo.UserRepo.CreateFirst(ctx, &domain.User{
				Username: fmt.Sprintf("admin%d", i), PasswordHash: "x", Active: true, IsAdmin: true, Timezone: "UTC",
			})
		}()
	}
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
		}
	}
	count, err := repo.UserRepo.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if wins != 1 || count != 1 {
		t.Fatalf("wins=%d users=%d; want exactly one", wins, count)
	}

	// Once a user exists, CreateFirst is a conflict and writes nothing.
	err = repo.UserRepo.CreateFirst(ctx, &domain.User{Username: "late", PasswordHash: "x", Timezone: "UTC"})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("CreateFirst after first user: err = %v; want ErrConflict", err)
	}
}

// The claim row is a lock, not a once-only marker: an install whose users
// were all removed (e.g. an operator recovering a lost admin) can create its
// first admin again.
func TestUserRepo_CreateFirstRecoversAfterUsersRemoved(t *testing.T) {
	repo := setupProductionTestDB(t)
	ctx := context.Background()
	first := &domain.User{Username: "admin", PasswordHash: "x", Active: true, IsAdmin: true, Timezone: "UTC"}
	if err := repo.UserRepo.CreateFirst(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UserRepo.db.ExecContext(ctx, "DELETE FROM users"); err != nil {
		t.Fatal(err)
	}
	again := &domain.User{Username: "recovered", PasswordHash: "x", Active: true, IsAdmin: true, Timezone: "UTC"}
	if err := repo.UserRepo.CreateFirst(ctx, again); err != nil {
		t.Fatalf("CreateFirst after all users were removed: %v", err)
	}
	if count, err := repo.UserRepo.Count(ctx); err != nil || count != 1 {
		t.Fatalf("users=%d err=%v; want 1", count, err)
	}
}
