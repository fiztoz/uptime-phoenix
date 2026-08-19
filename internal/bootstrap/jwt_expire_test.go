package bootstrap

import "testing"

func TestValidateJWTExpireHours(t *testing.T) {
	t.Parallel()
	if err := validateJWTExpireHours(24); err != nil {
		t.Errorf("24 hours: unexpected error: %v", err)
	}
	if err := validateJWTExpireHours(168); err != nil {
		t.Errorf("168 hours: unexpected error: %v", err)
	}
	if err := validateJWTExpireHours(0); err == nil {
		t.Error("0 hours: want error (would mint exp == iat)")
	}
	if err := validateJWTExpireHours(-1920); err == nil {
		t.Error("negative hours: want error (would mint an already-expired session)")
	}
}
