package services

import (
	"context"
	"errors"
	"testing"
)

// --- Tests: admin RBAC + DeleteUser guards --------------------------------
//
// These exercise AuthService.DeleteUser directly rather than through the
// HTTP layer. Note that, in the real deployment, the acting principal must
// already be an admin to reach DELETE /api/users/:id (see
// middleware.RequireAdmin), which makes some of these exact combinations
// (e.g. "last user" with a different acting principal, or "last admin"
// triggered by a non-admin actor) unreachable over HTTP — the sole user
// or sole admin IS necessarily the only possible authenticated actor in
// those cases. The service layer still enforces each invariant
// independently of the caller's own privileges, so we test it here.

func TestAuthService_Register_FirstUserIsAdmin(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	u, err := svc.Register(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !u.IsAdmin {
		t.Errorf("first registered user IsAdmin = false; want true")
	}
}

func TestAuthService_CreateUser_DefaultsNonAdmin(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	u, err := svc.CreateUser(ctx, "bob", "supersecret", true, false, "UTC", UserCapabilities{})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.IsAdmin {
		t.Errorf("CreateUser(isAdmin=false).IsAdmin = true; want false")
	}
}

func TestAuthService_CreateUser_ExplicitAdmin(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	u, err := svc.CreateUser(ctx, "carol", "supersecret", true, true, "UTC", UserCapabilities{})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if !u.IsAdmin {
		t.Errorf("CreateUser(isAdmin=true).IsAdmin = false; want true")
	}
}

func TestAuthService_DeleteUser_Self(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	admin, err := svc.Register(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A second user so the "last user" guard cannot shadow the self-delete
	// guard we are isolating here.
	if _, err := svc.CreateUser(ctx, "bob", "supersecret", true, false, "UTC", UserCapabilities{}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = svc.DeleteUser(ctx, admin.ID, admin.ID)
	if !errors.Is(err, ErrDeleteSelf) {
		t.Errorf("DeleteUser(self) = %v; want ErrDeleteSelf", err)
	}
}

func TestAuthService_DeleteUser_LastUser(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	admin, err := svc.Register(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Only one user exists in the system. Use a currentUserID distinct
	// from the target so the self-delete guard does not mask the
	// last-user guard we are testing.
	err = svc.DeleteUser(ctx, admin.ID+999, admin.ID)
	if !errors.Is(err, ErrLastUser) {
		t.Errorf("DeleteUser(last user) = %v; want ErrLastUser", err)
	}
}

func TestAuthService_DeleteUser_LastAdmin(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	admin, err := svc.Register(ctx, "alice", "supersecret") // first user => admin
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	member, err := svc.CreateUser(ctx, "bob", "supersecret", true, false, "UTC", UserCapabilities{}) // non-admin
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// member (non-admin) targets alice, the sole admin. The last-admin
	// guard must trip regardless of the deleting principal's own
	// privileges — the invariant is about the system's admin count, not
	// who is asking.
	err = svc.DeleteUser(ctx, member.ID, admin.ID)
	if !errors.Is(err, ErrLastAdmin) {
		t.Errorf("DeleteUser(last admin) = %v; want ErrLastAdmin", err)
	}
}

func TestAuthService_DeleteUser_SecondAdminSucceeds(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	admin, err := svc.Register(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	secondAdmin, err := svc.CreateUser(ctx, "carol", "supersecret", true, true, "UTC", UserCapabilities{})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Two admins exist, so deleting one of them (by the other) must
	// succeed — the remaining admin keeps the system reachable.
	if err := svc.DeleteUser(ctx, admin.ID, secondAdmin.ID); err != nil {
		t.Fatalf("DeleteUser(second admin) returned error: %v", err)
	}
	if _, err := svc.GetUser(ctx, secondAdmin.ID); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("GetUser(deleted) = %v; want ErrUserNotFound", err)
	}
}

func TestAuthService_DeleteUser_Success(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	admin, err := svc.Register(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	member, err := svc.CreateUser(ctx, "bob", "supersecret", true, false, "UTC", UserCapabilities{})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := svc.DeleteUser(ctx, admin.ID, member.ID); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := svc.GetUser(ctx, member.ID); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("GetUser(deleted) = %v; want ErrUserNotFound", err)
	}
}

func TestAuthService_ListUsers(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.CreateUser(ctx, "bob", "supersecret", true, false, "UTC", UserCapabilities{}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	users, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("len(users) = %d; want 2", len(users))
	}
	if users[0].ID > users[1].ID {
		t.Errorf("ListUsers is not ordered by id ascending: %d before %d", users[0].ID, users[1].ID)
	}
}
