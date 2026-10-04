package bootstrap

import (
	"context"
	"errors"
	"sync"
	"testing"

	adapterauth "github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func newBootstrapAuth(repo ports.UserRepository) *services.AuthService {
	return services.NewAuthService(
		repo,
		nil,
		adapterauth.NewJWTAuthenticator("bootstrap-test-secret", 24, repo),
		adapterauth.NewTOTPProvider("Uptime Phoenix"),
	)
}

func TestBootstrapUserConcurrentFirstRegistrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	base := memory.NewUserRepo()
	repo := &bootstrapCountBarrier{UserRepository: base, release: make(chan struct{})}
	authSvc := newBootstrapAuth(repo)

	results := make(chan error, 2)
	var callers sync.WaitGroup
	callers.Add(2)
	for range 2 {
		go func() {
			defer callers.Done()
			results <- bootstrapUser(ctx, authSvc, repo, "admin", "long-enough-password")
		}()
	}
	callers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent bootstrap returned error: %v", err)
		}
	}

	users, err := base.List(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("bootstrap created %d users; want exactly one", len(users))
	}
	if users[0].Username != "admin" || !users[0].IsAdmin {
		t.Fatalf("unexpected first user: %+v", users[0])
	}
}

func TestBootstrapUserSkipsWhenUserAlreadyExists(t *testing.T) {
	ctx := context.Background()
	repo := memory.NewUserRepo()
	authSvc := newBootstrapAuth(repo)
	if _, err := authSvc.Register(ctx, "existing-admin", "long-enough-password"); err != nil {
		t.Fatalf("create existing user: %v", err)
	}

	if err := bootstrapUser(ctx, authSvc, repo, "configured-admin", "short"); err != nil {
		t.Fatalf("bootstrap should skip when a user already exists: %v", err)
	}
	users, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 || users[0].Username != "existing-admin" {
		t.Fatalf("bootstrap changed existing users: %+v", users)
	}
}

func TestBootstrapUserSurfacesUnrelatedRegistrationFailure(t *testing.T) {
	ctx := context.Background()
	repo := memory.NewUserRepo()
	authSvc := newBootstrapAuth(repo)

	err := bootstrapUser(ctx, authSvc, repo, "admin", "short")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bootstrap error = %v; want wrapped validation error", err)
	}
	if errors.Is(err, services.ErrUserExists) {
		t.Fatalf("bootstrap incorrectly classified validation error as duplicate user: %v", err)
	}
	users, listErr := repo.List(ctx)
	if listErr != nil {
		t.Fatalf("list users: %v", listErr)
	}
	if len(users) != 0 {
		t.Fatalf("failed bootstrap left %d users; want none", len(users))
	}
}

type bootstrapCountBarrier struct {
	ports.UserRepository
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (r *bootstrapCountBarrier) Count(ctx context.Context) (int64, error) {
	count, err := r.UserRepository.Count(ctx)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.calls++
	if r.calls == 2 {
		close(r.release)
	}
	r.mu.Unlock()
	select {
	case <-r.release:
		return count, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
