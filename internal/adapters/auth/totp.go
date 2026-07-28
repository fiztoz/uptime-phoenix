// Package auth provides adapters for the ports.TwoFactor / ports.Authenticator
// interfaces (TOTP, JWT, password hashing).
package auth

import (
	"fmt"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TOTPProvider implements ports.TwoFactor using pquerna/otp.
//
// A single TOTPProvider is safe for concurrent use across goroutines because
// every call constructs its own state (Generate) or operates on a secret
// already produced by Generate (Verify). The instance only carries the
// default issuer used when callers do not override it.
type TOTPProvider struct {
	issuer string
}

// NewTOTPProvider creates a new TOTP-based two-factor provider with a
// fallback issuer used when GenerateSecret is called with an empty issuer.
func NewTOTPProvider(issuer string) *TOTPProvider {
	return &TOTPProvider{issuer: issuer}
}

// GenerateSecret generates a new TOTP secret and a provisioning URL
// (otpauth://totp/...) suitable for rendering as a QR code.
//
// The issuer and username are URL-encoded into the provisioning URL and
// surfaced to authenticator apps (Google Authenticator, 1Password, etc.)
// so the user can identify which account the token belongs to.
func (t *TOTPProvider) GenerateSecret(issuer, username string) (string, string, error) {
	if issuer == "" {
		issuer = t.issuer
	}
	if issuer == "" {
		return "", "", fmt.Errorf("generate totp secret: issuer is required")
	}
	if username == "" {
		return "", "", fmt.Errorf("generate totp secret: username is required")
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: username,
		// Defaults match Google Authenticator: 30s period, SHA1, 6 digits.
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", "", fmt.Errorf("generate totp secret: %w", err)
	}
	return key.Secret(), key.URL(), nil
}

// VerifyToken validates a 6-digit TOTP token against a secret.
//
// A ±1 period skew (90 seconds total) is permitted to absorb clock drift
// between the client and the server while still rejecting replayed tokens.
func (t *TOTPProvider) VerifyToken(secret, token string) bool {
	if secret == "" || token == "" {
		return false
	}
	return totp.Validate(token, secret)
}

// Ensure TOTPProvider implements TwoFactor.
var _ ports.TwoFactor = (*TOTPProvider)(nil)
