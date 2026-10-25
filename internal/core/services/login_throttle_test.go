package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newThrottleTestService returns a service with user "alice" (password
// "supersecret") and a clock the test can move.
func newThrottleTestService(t *testing.T) (*AuthService, *fakeTOTP, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	auth := newFakeAuthenticator()
	totp := newFakeTOTP()
	svc := NewAuthService(newInMemUserRepo(), newInMemAPIKeyRepo(), auth, totp, WithClock(clock))
	if _, err := svc.Register(context.Background(), "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	auth.seedHash("alice", "bcrypt$supersecret")
	return svc, totp, clock
}

func failLogins(t *testing.T, svc *AuthService, username string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := svc.Login(context.Background(), username, "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failed attempt %d = %v; want ErrInvalidCredentials", i+1, err)
		}
	}
}

func TestLogin_LocksAccountAfterRepeatedFailures(t *testing.T) {
	svc, _, clock := newThrottleTestService(t)
	ctx := context.Background()

	failLogins(t, svc, "alice", LoginFailuresBeforeBackoff)

	// The right password is refused while locked, and so is any casing of
	// the username: the lock follows the account, not the client.
	for _, name := range []string{"alice", "ALICE"} {
		if _, err := svc.Login(ctx, name, "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
			t.Fatalf("Login(%s) during backoff = %v; want ErrTooManyAttempts", name, err)
		}
	}
	if _, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("Begin2FALogin during backoff = %v; want ErrTooManyAttempts", err)
	}

	clock.now = clock.now.Add(LoginBackoffBase + time.Second)
	if _, err := svc.Login(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Login after backoff = %v; want success", err)
	}

	// Success wipes the history: the next failure does not lock again.
	failLogins(t, svc, "alice", 1)
	if _, err := svc.Login(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Login after reset = %v; want success", err)
	}
}

func TestLogin_BackoffGrowsAndIsCapped(t *testing.T) {
	svc, _, clock := newThrottleTestService(t)
	ctx := context.Background()

	failLogins(t, svc, "alice", LoginFailuresBeforeBackoff)
	// Each failure after the threshold doubles the wait.
	clock.now = clock.now.Add(LoginBackoffBase + time.Second)
	failLogins(t, svc, "alice", 1)
	clock.now = clock.now.Add(LoginBackoffBase + time.Second)
	if _, err := svc.Login(ctx, "alice", "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("Login inside doubled backoff = %v; want ErrTooManyAttempts", err)
	}
	clock.now = clock.now.Add(LoginBackoffBase)
	if _, err := svc.Login(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Login after doubled backoff = %v; want success", err)
	}

	if got := loginBackoff(LoginFailuresBeforeBackoff + 50); got != LoginBackoffMax {
		t.Fatalf("loginBackoff(many) = %v; want cap %v", got, LoginBackoffMax)
	}
}

func TestLogin_UnknownUserIsThrottledLikeKnownUser(t *testing.T) {
	svc, _, _ := newThrottleTestService(t)
	ctx := context.Background()

	failLogins(t, svc, "mallory", LoginFailuresBeforeBackoff)
	if _, err := svc.Login(ctx, "mallory", "anything"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("unknown user after failures = %v; want ErrTooManyAttempts (no enumeration)", err)
	}
	// Other accounts are unaffected.
	if _, err := svc.Login(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Login(alice) = %v; want success", err)
	}
}

func TestLogin_OldFailuresExpire(t *testing.T) {
	svc, _, clock := newThrottleTestService(t)
	ctx := context.Background()

	failLogins(t, svc, "alice", LoginFailuresBeforeBackoff-1)
	clock.now = clock.now.Add(loginFailureWindow + time.Minute)
	failLogins(t, svc, "alice", 1)
	if _, err := svc.Login(ctx, "alice", "supersecret"); err != nil {
		t.Fatalf("Login after stale failures = %v; want success", err)
	}
}

func enableTOTPForAlice(t *testing.T, svc *AuthService, totp *fakeTOTP) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := svc.SetupTOTP(ctx, 1); err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	totp.SetValidCode("123456")
	if err := svc.EnableTOTP(ctx, 1, "123456"); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
}

func TestComplete2FALogin_TicketBurnsAfterAttemptCap(t *testing.T) {
	svc, totp, clock := newThrottleTestService(t)
	ctx := context.Background()
	enableTOTPForAlice(t, svc, totp)

	ticket, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	for i := 0; i < Max2FAAttemptsPerTicket; i++ {
		if _, _, err := svc.Complete2FALogin(ctx, ticket, "000000"); !errors.Is(err, ErrTOTPInvalid) && !errors.Is(err, ErrTooManyAttempts) {
			t.Fatalf("wrong code %d = %v", i+1, err)
		}
	}
	// The ticket is spent: even the right code no longer works on it.
	if _, _, err := svc.Complete2FALogin(ctx, ticket, "123456"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("right code on spent ticket = %v; want ErrTooManyAttempts", err)
	}

	// The wrong codes also counted against the account, so a fresh ticket
	// waits out the backoff before it can be used.
	if _, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("Begin2FALogin during backoff = %v; want ErrTooManyAttempts", err)
	}
	clock.now = clock.now.Add(LoginBackoffBase + time.Second)
	fresh, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin after backoff: %v", err)
	}
	if _, _, err := svc.Complete2FALogin(ctx, fresh, "123456"); err != nil {
		t.Fatalf("Complete2FALogin with fresh ticket = %v; want success", err)
	}
}

func TestComplete2FALogin_TicketCannotBeReplayed(t *testing.T) {
	svc, totp, _ := newThrottleTestService(t)
	ctx := context.Background()
	enableTOTPForAlice(t, svc, totp)

	ticket, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	if _, _, err := svc.Complete2FALogin(ctx, ticket, "123456"); err != nil {
		t.Fatalf("Complete2FALogin: %v", err)
	}
	if _, _, err := svc.Complete2FALogin(ctx, ticket, "123456"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("replayed ticket = %v; want ErrTooManyAttempts", err)
	}
}

func TestLoginWith2FA_BadCodesCountTowardBackoff(t *testing.T) {
	svc, totp, _ := newThrottleTestService(t)
	ctx := context.Background()
	enableTOTPForAlice(t, svc, totp)

	for i := 0; i < LoginFailuresBeforeBackoff; i++ {
		if _, err := svc.LoginWith2FA(ctx, "alice", "supersecret", "000000"); !errors.Is(err, ErrTOTPInvalid) {
			t.Fatalf("bad code %d = %v; want ErrTOTPInvalid", i+1, err)
		}
	}
	if _, err := svc.LoginWith2FA(ctx, "alice", "supersecret", "123456"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("LoginWith2FA during backoff = %v; want ErrTooManyAttempts", err)
	}
}
