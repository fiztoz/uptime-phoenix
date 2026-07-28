package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- WebAuthn test doubles ----------------------------------------------

// fakeWebAuthn is a deterministic ports.WebAuthnAuthenticator. Begin* returns
// fixed option/session bytes; Finish* echoes a credential derived from the
// response bytes so tests can assert persistence and sign-count updates without
// a real authenticator.
type fakeWebAuthn struct {
	beginErr  error
	finishErr error
	// credential returned by Finish* calls.
	cred *domain.WebAuthnCredential
}

func (f *fakeWebAuthn) BeginRegistration(_ ports.WebAuthnUser) ([]byte, []byte, error) {
	if f.beginErr != nil {
		return nil, nil, f.beginErr
	}
	return []byte(`{"publicKey":{"challenge":"reg"}}`), []byte(`{"challenge":"reg"}`), nil
}

func (f *fakeWebAuthn) FinishRegistration(user ports.WebAuthnUser, _ []byte, _ []byte) (*domain.WebAuthnCredential, error) {
	if f.finishErr != nil {
		return nil, f.finishErr
	}
	c := *f.cred
	c.UserID = user.ID
	return &c, nil
}

func (f *fakeWebAuthn) BeginLogin(_ ports.WebAuthnUser) ([]byte, []byte, error) {
	if f.beginErr != nil {
		return nil, nil, f.beginErr
	}
	return []byte(`{"publicKey":{"challenge":"login"}}`), []byte(`{"challenge":"login"}`), nil
}

func (f *fakeWebAuthn) FinishLogin(_ ports.WebAuthnUser, _ []byte, _ []byte) (*domain.WebAuthnCredential, error) {
	if f.finishErr != nil {
		return nil, f.finishErr
	}
	c := *f.cred
	return &c, nil
}

// inMemWebAuthnRepo is a tiny credential store for these tests.
type inMemWebAuthnRepo struct {
	creds map[int64]*domain.WebAuthnCredential
	next  int64
}

func newInMemWebAuthnRepo() *inMemWebAuthnRepo {
	return &inMemWebAuthnRepo{creds: map[int64]*domain.WebAuthnCredential{}}
}

func (r *inMemWebAuthnRepo) Create(_ context.Context, c *domain.WebAuthnCredential) error {
	r.next++
	c.ID = r.next
	clone := *c
	r.creds[c.ID] = &clone
	return nil
}

func (r *inMemWebAuthnRepo) ListByUser(_ context.Context, userID int64) ([]*domain.WebAuthnCredential, error) {
	out := []*domain.WebAuthnCredential{}
	for _, c := range r.creds {
		if c.UserID == userID {
			clone := *c
			out = append(out, &clone)
		}
	}
	return out, nil
}

func (r *inMemWebAuthnRepo) GetByCredentialID(_ context.Context, credentialID []byte) (*domain.WebAuthnCredential, error) {
	for _, c := range r.creds {
		if string(c.CredentialID) == string(credentialID) {
			clone := *c
			return &clone, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *inMemWebAuthnRepo) UpdateUsage(_ context.Context, id int64, signCount uint32, flags byte, cloneWarning bool, attachment string, lastUsedAt time.Time) error {
	c, ok := r.creds[id]
	if !ok {
		return ports.ErrNotFound
	}
	c.SignCount = signCount
	c.Flags = flags
	c.FlagsKnown = true
	c.CloneWarning = cloneWarning
	c.Attachment = attachment
	lu := lastUsedAt
	c.LastUsedAt = &lu
	return nil
}

func (r *inMemWebAuthnRepo) Delete(_ context.Context, id, userID int64) error {
	c, ok := r.creds[id]
	if !ok || c.UserID != userID {
		return ports.ErrNotFound
	}
	delete(r.creds, id)
	return nil
}

var _ ports.WebAuthnCredentialRepository = (*inMemWebAuthnRepo)(nil)

// newWebAuthnTestService wires a service with passkeys enabled and returns the
// fakes so tests can drive ceremonies.
func newWebAuthnTestService(t *testing.T) (*AuthService, *fakeWebAuthn, *inMemWebAuthnRepo, int64) {
	t.Helper()
	userRepo := newInMemUserRepo()
	akRepo := newInMemAPIKeyRepo()
	auth := newFakeAuthenticator()
	totp := newFakeTOTP()
	wa := &fakeWebAuthn{cred: &domain.WebAuthnCredential{
		CredentialID: []byte{0xaa, 0xbb, 0xcc},
		PublicKey:    []byte{0x01, 0x02},
		SignCount:    5,
		Transports:   []string{"internal"},
	}}
	waRepo := newInMemWebAuthnRepo()
	svc := NewAuthService(userRepo, akRepo, auth, totp,
		WithClock(&fakeClock{now: time.Now().UTC()}),
		WithWebAuthn(wa, waRepo),
	)
	u, err := svc.Register(context.Background(), "alice", "supersecret")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return svc, wa, waRepo, u.ID
}

func TestPasskey_RegistrationRoundTrip(t *testing.T) {
	svc, _, waRepo, userID := newWebAuthnTestService(t)
	ctx := context.Background()

	options, sessionID, err := svc.BeginPasskeyRegistration(ctx, userID)
	if err != nil {
		t.Fatalf("BeginPasskeyRegistration: %v", err)
	}
	if len(options) == 0 || sessionID == "" {
		t.Fatal("begin returned empty options or session id")
	}

	cred, err := svc.FinishPasskeyRegistration(ctx, userID, sessionID, "My Laptop", []byte(`{"id":"x"}`))
	if err != nil {
		t.Fatalf("FinishPasskeyRegistration: %v", err)
	}
	if cred.ID == 0 {
		t.Error("credential not assigned an ID")
	}
	if cred.Name != "My Laptop" {
		t.Errorf("name = %q; want My Laptop", cred.Name)
	}

	stored, _ := waRepo.ListByUser(ctx, userID)
	if len(stored) != 1 {
		t.Fatalf("stored credentials = %d; want 1", len(stored))
	}
}

func TestPasskey_FinishRegistration_BadSession(t *testing.T) {
	svc, _, _, userID := newWebAuthnTestService(t)
	ctx := context.Background()

	_, err := svc.FinishPasskeyRegistration(ctx, userID, "nonexistent-session", "x", []byte(`{}`))
	if !errors.Is(err, ErrWebAuthnChallenge) {
		t.Errorf("err = %v; want ErrWebAuthnChallenge", err)
	}
}

func TestPasskey_Session_SingleUse(t *testing.T) {
	svc, _, _, userID := newWebAuthnTestService(t)
	ctx := context.Background()

	_, sessionID, err := svc.BeginPasskeyRegistration(ctx, userID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := svc.FinishPasskeyRegistration(ctx, userID, sessionID, "x", []byte(`{}`)); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	// Re-using the same session id must fail.
	if _, err := svc.FinishPasskeyRegistration(ctx, userID, sessionID, "x", []byte(`{}`)); !errors.Is(err, ErrWebAuthnChallenge) {
		t.Errorf("second finish err = %v; want ErrWebAuthnChallenge", err)
	}
}

func TestPasskey_LoginRoundTrip_UpdatesSignCount(t *testing.T) {
	svc, wa, waRepo, userID := newWebAuthnTestService(t)
	ctx := context.Background()

	// Seed a credential the login ceremony can match.
	seed := &domain.WebAuthnCredential{
		UserID:       userID,
		CredentialID: []byte{0xaa, 0xbb, 0xcc},
		PublicKey:    []byte{0x01, 0x02},
		SignCount:    5,
	}
	if err := waRepo.Create(ctx, seed); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	// The fake returns the assertion with an incremented counter.
	wa.cred = &domain.WebAuthnCredential{
		CredentialID: []byte{0xaa, 0xbb, 0xcc},
		SignCount:    6,
		Flags:        0x1d,
		FlagsKnown:   true,
		CloneWarning: true,
		Attachment:   "platform",
	}

	_, sessionID, err := svc.BeginPasskeyLogin(ctx, "alice")
	if err != nil {
		t.Fatalf("BeginPasskeyLogin: %v", err)
	}

	token, user, err := svc.FinishPasskeyLogin(ctx, "alice", sessionID, []byte(`{"id":"x"}`))
	if err != nil {
		t.Fatalf("FinishPasskeyLogin: %v", err)
	}
	if token == "" {
		t.Error("expected a session token")
	}
	if user.Username != "alice" {
		t.Errorf("user = %q; want alice", user.Username)
	}

	stored, _ := waRepo.GetByCredentialID(ctx, []byte{0xaa, 0xbb, 0xcc})
	if stored.SignCount != 6 {
		t.Errorf("sign count = %d; want 6 (updated)", stored.SignCount)
	}
	if stored.LastUsedAt == nil {
		t.Error("last_used_at not updated")
	}
	if !stored.FlagsKnown || stored.Flags != 0x1d || !stored.CloneWarning || stored.Attachment != "platform" {
		t.Errorf("authenticator state not updated: %+v", stored)
	}
}

func TestPasskey_BeginLogin_NoCredentials(t *testing.T) {
	svc, _, _, _ := newWebAuthnTestService(t)
	_, _, err := svc.BeginPasskeyLogin(context.Background(), "alice")
	if !errors.Is(err, ErrWebAuthnNoCredentials) {
		t.Errorf("err = %v; want ErrWebAuthnNoCredentials", err)
	}
}

func TestPasskey_BeginLogin_UnknownUser(t *testing.T) {
	svc, _, _, _ := newWebAuthnTestService(t)
	_, _, err := svc.BeginPasskeyLogin(context.Background(), "ghost")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v; want ErrInvalidCredentials", err)
	}
}

func TestPasskey_NotConfigured(t *testing.T) {
	// A service without WithWebAuthn must report passkeys as unavailable.
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	u, _ := svc.Register(ctx, "bob", "supersecret")

	if _, _, err := svc.BeginPasskeyRegistration(ctx, u.ID); !errors.Is(err, ErrWebAuthnNotConfigured) {
		t.Errorf("BeginPasskeyRegistration err = %v; want ErrWebAuthnNotConfigured", err)
	}
	if _, _, err := svc.BeginPasskeyLogin(ctx, "bob"); !errors.Is(err, ErrWebAuthnNotConfigured) {
		t.Errorf("BeginPasskeyLogin err = %v; want ErrWebAuthnNotConfigured", err)
	}
}

func TestPasskey_ListAndDelete(t *testing.T) {
	svc, _, _, userID := newWebAuthnTestService(t)
	ctx := context.Background()

	_, sessionID, err := svc.BeginPasskeyRegistration(ctx, userID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	cred, err := svc.FinishPasskeyRegistration(ctx, userID, sessionID, "Key", []byte(`{}`))
	if err != nil {
		t.Fatalf("finish: %v", err)
	}

	list, err := svc.ListPasskeys(ctx, userID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListPasskeys = %v, len %d; want 1", err, len(list))
	}

	if err := svc.DeletePasskey(ctx, userID, cred.ID); err != nil {
		t.Fatalf("DeletePasskey: %v", err)
	}
	list, _ = svc.ListPasskeys(ctx, userID)
	if len(list) != 0 {
		t.Errorf("after delete len = %d; want 0", len(list))
	}

	// Deleting another user's (or missing) credential is a not-found.
	if err := svc.DeletePasskey(ctx, userID, 9999); !errors.Is(err, ErrWebAuthnNotConfigured) && err == nil {
		t.Error("expected error deleting missing credential")
	}
}
