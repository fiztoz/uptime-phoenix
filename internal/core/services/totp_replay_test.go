package services

import (
	"context"
	"errors"
	"testing"
)

// TestComplete2FALogin_CodeCannotBeReusedOnANewTicket pins #123: a TOTP code
// that already completed one 2FA login must not complete a second login on a
// fresh ticket within the same time step.
//
// Skipped until the replay-store design is chosen (see the draft PR); remove
// the Skip line to see it fail on main.
func TestComplete2FALogin_CodeCannotBeReusedOnANewTicket(t *testing.T) {
	if !totpReplayProtectionDecided {
		t.Skip("#123: TOTP replay store design pending; see draft PR options")
	}
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

// totpReplayProtectionDecided flips to true in the PR that implements the
// chosen option.
const totpReplayProtectionDecided = false
