// Package auth provides adapters for the ports.TwoFactor / ports.Authenticator
// interfaces (TOTP, JWT, password hashing).
package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// PasswordHasher provides password hashing and verification using bcrypt.
// This is a standalone helper that can be used outside the JWTAuthenticator
// (e.g. by the auth service to hash passwords on user registration).
type PasswordHasher struct {
	cost int
}

// NewPasswordHasher creates a new password hasher using bcrypt's default cost.
// Use NewPasswordHasherWithCost in tests to lower the cost and speed tests up.
func NewPasswordHasher() *PasswordHasher {
	return &PasswordHasher{cost: bcrypt.DefaultCost}
}

// NewPasswordHasherWithCost creates a hasher with an explicit bcrypt cost.
// Only intended for tests; production code should use NewPasswordHasher.
func NewPasswordHasherWithCost(cost int) *PasswordHasher {
	return &PasswordHasher{cost: cost}
}

// Hash hashes a plaintext password using bcrypt.
func (p *PasswordHasher) Hash(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("password: cannot hash empty password")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), p.cost)
	if err != nil {
		return "", fmt.Errorf("password: hash: %w", err)
	}
	return string(hash), nil
}

// Verify compares a plaintext password against a bcrypt hash.
//
// It returns nil on success and a non-nil error otherwise. The error wraps
// bcrypt.ErrMismatchedHashAndPassword when the password does not match,
// which is the canonical signal for "wrong password" and intentionally
// returns the same error to all callers to avoid revealing whether the
// hash itself is malformed.
func (p *PasswordHasher) Verify(hashed, password string) error {
	if hashed == "" || password == "" {
		return fmt.Errorf("password: empty hash or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(password)); err != nil {
		return fmt.Errorf("password: verify: %w", err)
	}
	return nil
}

var _ ports.PasswordHasher = (*PasswordHasher)(nil)
