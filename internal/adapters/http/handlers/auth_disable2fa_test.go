package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
)

// TestAuthHandlers_Disable2FA_RequiresAFreshCode pins #123: turning 2FA off
// needs the current password AND a current TOTP code, and that code is
// consumed through the same replay-protected path as a login, so a code
// that was already used (or a stolen session plus the password alone)
// cannot disable 2FA.
func TestAuthHandlers_Disable2FA_RequiresAFreshCode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	register := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"setup_token": testSetupToken,
		"username":    "alice",
		"password":    "supersecret",
	}, "")
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d; body=%s", register.Code, register.Body.String())
	}
	var reg handlers.LoginResponse
	if err := json.Unmarshal(register.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	setup := h.do(t, http.MethodPost, "/api/auth/setup-2fa", nil, reg.Token)
	if setup.Code != http.StatusOK {
		t.Fatalf("setup-2fa status = %d; body=%s", setup.Code, setup.Body.String())
	}
	var sr handlers.Setup2FAResponse
	if err := json.Unmarshal(setup.Body.Bytes(), &sr); err != nil || sr.Secret == "" {
		t.Fatalf("decode setup-2fa: %v (secret empty: %t)", err, sr.Secret == "")
	}
	code := func(at time.Time) string {
		t.Helper()
		c, err := totp.GenerateCode(sr.Secret, at)
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		return c
	}
	// Enroll with the previous step's code (inside the ±1 skew window), so
	// the current step is still unused for the disable call below.
	enrollCode := code(time.Now().Add(-30 * time.Second))
	if rec := h.do(t, http.MethodPost, "/api/auth/enable-2fa", map[string]string{"token": enrollCode}, reg.Token); rec.Code != http.StatusOK {
		t.Fatalf("enable-2fa status = %d; body=%s", rec.Code, rec.Body.String())
	}
	stillEnabled := func(when string) {
		t.Helper()
		u, err := h.users.GetByID(ctx, 1)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if !u.TOTPEnabled || u.TOTPSecret == "" {
			t.Fatalf("%s: 2FA was disabled; want it still enabled", when)
		}
	}
	disable := func(body map[string]string) int {
		t.Helper()
		return h.do(t, http.MethodPost, "/api/auth/disable-2fa", body, reg.Token).Code
	}

	if got := disable(map[string]string{"password": "supersecret"}); got != http.StatusBadRequest {
		t.Errorf("password without a code: status = %d; want 400", got)
	}
	stillEnabled("password without a code")

	if got := disable(map[string]string{"password": "supersecret", "token": "000000"}); got != http.StatusUnauthorized {
		t.Errorf("wrong code: status = %d; want 401", got)
	}
	stillEnabled("wrong code")

	if got := disable(map[string]string{"password": "supersecret", "token": enrollCode}); got != http.StatusUnauthorized {
		t.Errorf("already-consumed enrollment code: status = %d; want 401", got)
	}
	stillEnabled("already-consumed code")

	fresh := code(time.Now())
	if got := disable(map[string]string{"password": "wrong-password", "token": fresh}); got != http.StatusUnauthorized {
		t.Errorf("wrong password with a fresh code: status = %d; want 401", got)
	}
	stillEnabled("wrong password")

	// The password is checked before the code is consumed, so the refused
	// attempt above did not burn the fresh code.
	if got := disable(map[string]string{"password": "supersecret", "token": fresh}); got != http.StatusOK {
		t.Fatalf("password and a fresh code: status = %d; want 200", got)
	}
	u, err := h.users.GetByID(ctx, 1)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if u.TOTPEnabled || u.TOTPSecret != "" {
		t.Fatalf("2FA still enabled after a valid disable: enabled=%t", u.TOTPEnabled)
	}
}
