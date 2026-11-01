// Package auth provides adapters for the ports.TwoFactor / ports.Authenticator
// interfaces (TOTP, JWT, password hashing).
package auth

import (
	"crypto/subtle"
	"fmt"
	"time"

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

// TOTPPeriod is the TOTP time-step length in seconds.
const TOTPPeriod = 30

// TOTPSkewSteps is how many steps either side of the server's current step
// still match (clock drift between the authenticator and the server).
const TOTPSkewSteps = 1

// MatchToken validates a 6-digit TOTP token against a secret at instant at
// and returns the matched time step.
//
// The window is steps floor(at/30)-1 … floor(at/30)+1. When the same code is
// valid in more than one step of the window (a 1-in-a-million collision) the
// latest step wins, so a consumer that refuses steps at or below the last
// consumed one can never be tricked into accepting a reused code. The
// adapter keeps no per-user state: replay refusal is the caller's job.
func (t *TOTPProvider) MatchToken(secret, token string, at time.Time) (int64, bool) {
	if secret == "" || len(token) != int(otp.DigitsSix) {
		return 0, false
	}
	current := at.Unix() / TOTPPeriod
	if at.Unix() < 0 && at.Unix()%TOTPPeriod != 0 {
		current-- // floor, not truncation, for pre-epoch instants
	}
	for step := current + TOTPSkewSteps; step >= current-TOTPSkewSteps; step-- {
		if step < 0 {
			continue
		}
		code, err := totp.GenerateCodeCustom(secret, time.Unix(step*TOTPPeriod, 0).UTC(), totp.ValidateOpts{
			Period:    TOTPPeriod,
			Digits:    otp.DigitsSix,
			Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return 0, false // malformed secret
		}
		if subtle.ConstantTimeCompare([]byte(code), []byte(token)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// Ensure TOTPProvider implements TwoFactor.
var _ ports.TwoFactor = (*TOTPProvider)(nil)
