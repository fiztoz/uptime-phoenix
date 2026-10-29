package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type sessionUserRepo struct {
	ports.UserRepository
	user  *domain.User
	err   error
	calls int
}

func (r *sessionUserRepo) GetByID(context.Context, int64) (*domain.User, error) {
	r.calls++
	return r.user, r.err
}

func TestAuthSessionRevocation(t *testing.T) {
	ctx := context.Background()
	svc, auth, _ := newTestService(t)
	auth.sessions["existing-session"] = 42
	repo := &sessionUserRepo{user: &domain.User{ID: 42, Active: true}}
	svc.users = repo
	if id, err := svc.VerifyToken(ctx, "existing-session"); err != nil || id != 42 {
		t.Fatalf("active session: id=%d err=%v", id, err)
	}
	repo.user.Active = false
	if id, err := svc.VerifyToken(ctx, "existing-session"); id != 0 || !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("deactivated session: id=%d err=%v", id, err)
	}
	repo.user, repo.err = nil, ports.ErrNotFound
	if id, err := svc.VerifyToken(ctx, "existing-session"); id != 0 || !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("deleted session: id=%d err=%v", id, err)
	}
	repo.err = errors.New("repository unavailable")
	if id, err := svc.VerifyToken(ctx, "existing-session"); id != 0 || !errors.Is(err, repo.err) {
		t.Fatalf("storage failure must fail closed: id=%d err=%v", id, err)
	}
	repo.user, repo.err = nil, nil
	if id, err := svc.VerifyToken(ctx, "existing-session"); id != 0 || !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("missing principal must fail closed: id=%d err=%v", id, err)
	}
	calls := repo.calls
	if id, err := svc.VerifyToken(ctx, "invalid-session"); id != 0 || err == nil || repo.calls != calls {
		t.Fatalf("invalid token must not query users: id=%d err=%v calls=%d", id, err, repo.calls)
	}
	svc.users = nil
	if id, err := svc.VerifyToken(ctx, "existing-session"); id != 0 || !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("unconfigured principal lookup must fail closed: id=%d err=%v", id, err)
	}
}
