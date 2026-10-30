package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestComplete2FALogin_CodeCannotBeReusedOnANewTicket pins #123: a TOTP code
// that already completed one 2FA login must not complete a second login on a
// fresh ticket within the same time step.
func TestComplete2FALogin_CodeCannotBeReusedOnANewTicket(t *testing.T) {
	svc, totp, _ := newThrottleTestService(t)
	ctx := context.Background()
	enableTOTPForAlice(t, svc, totp)

	first, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	if _, _, err := svc.Complete2FALogin(ctx, first, "123456"); err != nil {
		t.Fatalf("first Complete2FALogin: %v", err)
	}
	second, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("second Begin2FALogin: %v", err)
	}
	if _, _, err := svc.Complete2FALogin(ctx, second, "123456"); !errors.Is(err, ErrTOTPInvalid) {
		t.Fatalf("reused code on a new ticket = %v; want ErrTOTPInvalid", err)
	}
}

// newTOTPReplayService is newThrottleTestService with the user repository
// exposed, so tests can edit records and inject storage failures.
func newTOTPReplayService(t *testing.T) (*AuthService, *fakeTOTP, *inMemUserRepo) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	auth := newFakeAuthenticator()
	totp := newFakeTOTP()
	repo := newInMemUserRepo()
	svc := NewAuthService(repo, newInMemAPIKeyRepo(), auth, totp, WithClock(clock))
	if _, err := svc.Register(context.Background(), "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	auth.seedHash("alice", "bcrypt$supersecret")
	enableTOTPForAlice(t, svc, totp)
	return svc, totp, repo
}

func complete2FA(t *testing.T, svc *AuthService, code string) (string, error) {
	t.Helper()
	ticket, _, err := svc.Begin2FALogin(context.Background(), "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	tok, _, err := svc.Complete2FALogin(context.Background(), ticket, code)
	if err != nil && tok != "" {
		t.Fatalf("Complete2FALogin returned a session token together with error %v", err)
	}
	return tok, err
}

func TestEnableTOTP_ConfirmationCodeCannotCompleteALogin(t *testing.T) {
	svc, _, _ := newTOTPReplayService(t)
	// enableTOTPForAlice confirmed enrolment with "654321".
	if _, err := complete2FA(t, svc, "654321"); !errors.Is(err, ErrTOTPInvalid) {
		t.Fatalf("login with the enrolment code = %v; want ErrTOTPInvalid", err)
	}
}

func TestComplete2FALogin_NewStepWorksAndOlderStepIsRefused(t *testing.T) {
	svc, totp, _ := newTOTPReplayService(t)
	totp.SetValidCode("222222") // a later step than "123456"
	if _, err := complete2FA(t, svc, "222222"); err != nil {
		t.Fatalf("login with a new step's code: %v", err)
	}
	// "123456" was never used but belongs to an earlier step than the one
	// just consumed, so it is refused too (strictly increasing steps).
	if _, err := complete2FA(t, svc, "123456"); !errors.Is(err, ErrTOTPInvalid) {
		t.Fatalf("login with an older step's code = %v; want ErrTOTPInvalid", err)
	}
	totp.SetValidCode("333333")
	if _, err := complete2FA(t, svc, "333333"); err != nil {
		t.Fatalf("login with the next step's code: %v", err)
	}
}

func TestComplete2FALogin_ProfileUpdateCannotResetTOTPState(t *testing.T) {
	svc, _, repo := newTOTPReplayService(t)
	ctx := context.Background()
	if _, err := complete2FA(t, svc, "123456"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	// A generic update made from a stale snapshot must not disable 2FA,
	// clear the secret or reset replay state.
	u, err := repo.GetByID(ctx, 1)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	u.TOTPEnabled, u.TOTPSecret, u.TOTPGeneration = false, "", 0
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := repo.GetByID(ctx, 1)
	if !got.TOTPEnabled || got.TOTPSecret == "" {
		t.Fatalf("Update changed TOTP state: enabled=%v secret-set=%v", got.TOTPEnabled, got.TOTPSecret != "")
	}
	if _, err := complete2FA(t, svc, "123456"); !errors.Is(err, ErrTOTPInvalid) {
		t.Fatalf("reused code after a profile update = %v; want ErrTOTPInvalid", err)
	}
}

func TestComplete2FALogin_RotationDuringATicketIssuesNoSession(t *testing.T) {
	svc, _, _ := newTOTPReplayService(t)
	ctx := context.Background()
	ticket, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	if _, _, err := svc.SetupTOTP(ctx, 1); err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	tok, _, err := svc.Complete2FALogin(ctx, ticket, "123456")
	if err == nil || tok != "" {
		t.Fatalf("Complete2FALogin after rotation = %q, %v; want an error and no session", tok, err)
	}
}

func TestComplete2FALogin_DeactivatedUserCannotConsume(t *testing.T) {
	svc, _, repo := newTOTPReplayService(t)
	ctx := context.Background()
	ticket, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	u, _ := repo.GetByID(ctx, 1)
	u.Active = false
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}
	tok, _, err := svc.Complete2FALogin(ctx, ticket, "123456")
	if err == nil || tok != "" {
		t.Fatalf("Complete2FALogin for a deactivated user = %q, %v; want an error and no session", tok, err)
	}
}

func TestTOTP_StorageFailureFailsClosed(t *testing.T) {
	svc, _, repo := newTOTPReplayService(t)
	ctx := context.Background()
	ticket, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	repo.totpErr = errors.New("database is unavailable")
	tok, _, err := svc.Complete2FALogin(ctx, ticket, "123456")
	if !errors.Is(err, ErrTOTPUnavailable) || tok != "" {
		t.Fatalf("Complete2FALogin with storage down = %q, %v; want ErrTOTPUnavailable and no session", tok, err)
	}
	if errors.Is(err, ErrTOTPInvalid) {
		t.Fatalf("storage failure reported as an invalid code: %v", err)
	}
	if tok, err := svc.LoginWith2FA(ctx, "alice", "supersecret", "123456"); !errors.Is(err, ErrTOTPUnavailable) || tok != "" {
		t.Fatalf("LoginWith2FA with storage down = %q, %v; want ErrTOTPUnavailable and no session", tok, err)
	}
	// Nothing was consumed: once storage is back the same ticket and code
	// complete exactly one login.
	repo.totpErr = nil
	if tok, _, err := svc.Complete2FALogin(ctx, ticket, "123456"); err != nil || tok == "" {
		t.Fatalf("Complete2FALogin after recovery = %q, %v; want a session", tok, err)
	}
	if _, err := complete2FA(t, svc, "123456"); !errors.Is(err, ErrTOTPInvalid) {
		t.Fatalf("replay after recovery = %v; want ErrTOTPInvalid", err)
	}
}

func TestDisableTOTP_ReEnrolmentStartsFreshReplayState(t *testing.T) {
	svc, totp, _ := newTOTPReplayService(t)
	ctx := context.Background()
	if _, err := complete2FA(t, svc, "123456"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := svc.DisableTOTP(ctx, 1); err != nil {
		t.Fatalf("DisableTOTP: %v", err)
	}
	if _, _, err := svc.SetupTOTP(ctx, 1); err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	// Steps consumed under the old credential do not block the new one.
	totp.SetValidCode("654321")
	if err := svc.EnableTOTP(ctx, 1, "654321"); err != nil {
		t.Fatalf("EnableTOTP on the new credential: %v", err)
	}
}
