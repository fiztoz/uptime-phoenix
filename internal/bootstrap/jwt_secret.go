package bootstrap

import (
	"errors"
	"fmt"
	"strings"
)

// MinJWTSecretBytes is the shortest JWT_SECRET LoadConfig accepts. HS256 keys
// shorter than the 32-byte hash output are below the RFC 7518 minimum.
const MinJWTSecretBytes = 32

// ErrInvalidJWTSecret marks a LoadConfig failure caused by JWT_SECRET, so a
// CLI can say which setting to fix without echoing the value.
var ErrInvalidJWTSecret = errors.New("invalid JWT_SECRET")

// knownJWTSecrets lists every placeholder signing key this repository has
// shipped in code, compose files, scripts or docs. Anyone can read them, so a
// server signing with one of them hands out forgeable sessions.
var knownJWTSecrets = []string{
	"change-me-in-production",
	"change_me_jwt",
	"changeme",
	"change-me",
	"replace-me",
	"secret",
	"e2e_secret",
	"load_secret",
	"private-cli-test-only-secret",
}

// validateJWTSecret refuses a signing key that is empty, a published
// placeholder, or shorter than MinJWTSecretBytes once surrounding whitespace
// is ignored (a short key padded with spaces is still a short key). The
// message never echoes the configured value. Every error wraps
// ErrInvalidJWTSecret.
func validateJWTSecret(secret string) error {
	const hint = "generate one with: openssl rand -base64 48"
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return fmt.Errorf("%w: JWT_SECRET is required; %s", ErrInvalidJWTSecret, hint)
	}
	for _, known := range knownJWTSecrets {
		if strings.EqualFold(trimmed, known) {
			return fmt.Errorf("%w: JWT_SECRET is a publicly known placeholder value; %s", ErrInvalidJWTSecret, hint)
		}
	}
	if len(trimmed) < MinJWTSecretBytes {
		return fmt.Errorf("%w: JWT_SECRET must be at least %d bytes (ignoring surrounding whitespace), got %d; %s",
			ErrInvalidJWTSecret, MinJWTSecretBytes, len(trimmed), hint)
	}
	return nil
}
