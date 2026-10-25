package bootstrap

import (
	"fmt"
	"strings"
)

// MinJWTSecretBytes is the shortest JWT_SECRET LoadConfig accepts. HS256 keys
// shorter than the 32-byte hash output are below the RFC 7518 minimum.
const MinJWTSecretBytes = 32

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
// placeholder, or shorter than MinJWTSecretBytes. The message never echoes
// the configured value.
func validateJWTSecret(secret string) error {
	const hint = "generate one with: openssl rand -base64 48"
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return fmt.Errorf("JWT_SECRET is required; %s", hint)
	}
	for _, known := range knownJWTSecrets {
		if strings.EqualFold(trimmed, known) {
			return fmt.Errorf("JWT_SECRET is a publicly known placeholder value; %s", hint)
		}
	}
	if len(secret) < MinJWTSecretBytes {
		return fmt.Errorf("JWT_SECRET must be at least %d bytes, got %d; %s", MinJWTSecretBytes, len(secret), hint)
	}
	return nil
}
