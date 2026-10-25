package bootstrap

import (
	"errors"
	"strings"
	"testing"
)

// testJWTSecret is a 48-byte key used only by tests that load a full Config.
const testJWTSecret = "bootstrap-test-jwt-secret-0123456789abcdefghijkl"

func TestValidateJWTSecret(t *testing.T) {
	t.Parallel()
	type jwtCase struct {
		name    string
		secret  string
		wantErr string
	}
	cases := make([]jwtCase, 0, 6+len(knownJWTSecrets))
	cases = append(cases,
		jwtCase{"empty", "", "JWT_SECRET is required"},
		jwtCase{"whitespace", "   ", "JWT_SECRET is required"},
		jwtCase{"31 bytes", strings.Repeat("a", 31), "at least 32 bytes"},
		// Whitespace padding must not satisfy the length floor.
		jwtCase{"31 bytes padded to 40", "    " + strings.Repeat("a", 31) + "     ", "at least 32 bytes"},
		jwtCase{"32 bytes", strings.Repeat("a", 32), ""},
		jwtCase{"48 bytes", testJWTSecret, ""},
	)
	for _, known := range knownJWTSecrets {
		cases = append(cases, jwtCase{"known " + known, known, "publicly known placeholder"})
	}
	for _, tc := range cases {
		err := validateJWTSecret(tc.secret)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %v, want containing %q", tc.name, err, tc.wantErr)
		}
		if err != nil && !errors.Is(err, ErrInvalidJWTSecret) {
			t.Errorf("%s: error does not wrap ErrInvalidJWTSecret", tc.name)
		}
		if err != nil && tc.secret != "" && strings.TrimSpace(tc.secret) != "" && strings.Contains(err.Error(), tc.secret) {
			t.Errorf("%s: error echoes the secret", tc.name)
		}
	}
}

func TestLoadConfigRejectsMissingOrWeakJWTSecret(t *testing.T) {
	t.Setenv("PROBES_ENABLED", "false")
	t.Setenv("PROBE_HUB_ID", "")
	t.Setenv("PROBE_SECRET_KEY_FILE", "")
	for _, secret := range []string{"", "change-me-in-production", "change_me_jwt", strings.Repeat("x", 31)} {
		t.Setenv("JWT_SECRET", secret)
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
			t.Fatalf("LoadConfig with JWT_SECRET of length %d: err = %v, want JWT_SECRET error", len(secret), err)
		}
	}
	t.Setenv("JWT_SECRET", testJWTSecret)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with strong secret: %v", err)
	}
	if cfg.JWTSecret != testJWTSecret {
		t.Fatal("strong secret not loaded")
	}
}
