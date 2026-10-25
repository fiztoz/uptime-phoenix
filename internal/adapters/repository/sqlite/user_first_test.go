package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// #92: concurrent first-user inserts elect exactly one winner.
func TestUserRepo_CreateFirstElectsOneWinner(t *testing.T) {
	repo := setupTestDB(t)
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
