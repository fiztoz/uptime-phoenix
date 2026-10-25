package repository_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestUserCreateFirstElectsOneWinner covers #92: replicas racing to create
// the first admin must end with exactly one user on every engine.
func TestUserCreateFirstElectsOneWinner(t *testing.T) {
	for name, factory := range map[string]repositoryFactory{"sqlite": sqliteFactory, "mariadb": mariadbFactory} {
		t.Run(name, func(t *testing.T) {
			users := factory(t).users
			ctx := context.Background()

			const n = 8
			errs := make(chan error, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- users.CreateFirst(ctx, &domain.User{
						Username: fmt.Sprintf("first%d", i), PasswordHash: "x", Active: true, IsAdmin: true, Timezone: "UTC",
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
			count, err := users.Count(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if wins != 1 || count != 1 {
				t.Fatalf("wins=%d users=%d; want exactly one", wins, count)
			}
			err = users.CreateFirst(ctx, &domain.User{Username: "late", PasswordHash: "x", Timezone: "UTC"})
			if !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("CreateFirst after the first user: err = %v; want ErrConflict", err)
			}
		})
	}
}
