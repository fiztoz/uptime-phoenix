// Package ports defines the interfaces that adapters must implement.
// No implementations, no external imports — only stdlib and domain types.
package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// PasswordHasher defines transport- and algorithm-independent password hashing
// for services that protect stored secrets without owning authentication.
type PasswordHasher interface {
	// Hash returns a one-way hash of password.
	Hash(password string) (string, error)
	// Verify compares a plaintext password with a stored hash.
	Verify(hashed, password string) error
}

// Authenticator defines authentication operations (login, token verification,
// password hashing, and short-lived 2FA challenge tickets).
type Authenticator interface {
	// Login authenticates a user with username + password and returns a JWT.
	Login(ctx context.Context, username, password string) (token string, err error)
	// VerifyToken validates a JWT and returns the user ID encoded in the claims.
	VerifyToken(ctx context.Context, token string) (userID int64, err error)
	// HashPassword hashes a plaintext password.
	HashPassword(password string) (string, error)
	// VerifyPassword compares a plaintext password against a stored hash.
	VerifyPassword(hashed, password string) error
	// IssueSession mints a fresh session JWT for an already-authenticated
	// user. It is the "post-credential" counterpart to Login: callers
	// use it when they have proven the user's identity through some other
	// channel (e.g. a 2FA challenge ticket) and just need a session token.
	IssueSession(ctx context.Context, userID int64) (token string, err error)
	// IssuePending2FATicket signs a short-lived ticket that records a successful
	// password step and signals that TOTP verification is still required.
	IssuePending2FATicket(ctx context.Context, userID int64) (ticket string, err error)
	// VerifyPending2FATicket validates a 2FA challenge ticket and returns the
	// user ID it was issued for. The token is consumed and cannot be reused.
	VerifyPending2FATicket(ctx context.Context, ticket string) (userID int64, err error)
}

// TwoFactor defines TOTP-based two-factor authentication operations.
//
// Implementations are expected to be stateless with respect to users: the
// adapter only knows how to generate and verify TOTP secrets, the service
// layer owns the persistence of those secrets.
type TwoFactor interface {
	// GenerateSecret returns a freshly generated TOTP secret and a provisioning
	// URL (otpauth://) suitable for rendering as a QR code.
	GenerateSecret(issuer, username string) (secret string, qrURL string, err error)
	// VerifyToken validates a 6-digit TOTP token against the secret with a
	// ±1 period skew (Google Authenticator-compatible).
	VerifyToken(secret, token string) bool
}

// WebAuthnUser is the minimal view of a user the WebAuthnAuthenticator needs
// to run a ceremony. The adapter translates it (plus the user's stored
// credentials) into the go-webauthn library's user model at the boundary, so
// the port stays free of library types.
type WebAuthnUser struct {
	ID          int64
	Username    string
	DisplayName string
	Credentials []*domain.WebAuthnCredential
}

// WebAuthnAuthenticator defines passkey (WebAuthn) ceremonies for both
// registration and login.
//
// Like TwoFactor, implementations are stateless with respect to users and
// challenges: the service layer owns the persistence of credentials and of
// the short-lived ceremony session. To keep this port free of library types,
// the begin/finish calls exchange opaque JSON payloads:
//
//   - "options" is the JSON the browser passes to navigator.credentials
//     .create() / .get().
//   - "session" is the opaque server-side ceremony state that must be handed
//     back unchanged to the matching Finish call.
//   - "response" is the raw JSON body the browser returns from the
//     authenticator.
//
// Finish calls return a *domain.WebAuthnCredential the service persists
// (registration) or whose SignCount the service updates (login).
type WebAuthnAuthenticator interface {
	// BeginRegistration starts a passkey registration ceremony for an
	// already-authenticated user and returns the creation options to send to
	// the browser plus the opaque session state to persist.
	BeginRegistration(user WebAuthnUser) (options []byte, session []byte, err error)
	// FinishRegistration validates the authenticator's response against the
	// stored session and returns the new credential to persist.
	FinishRegistration(user WebAuthnUser, session []byte, response []byte) (*domain.WebAuthnCredential, error)
	// BeginLogin starts a passkey assertion ceremony for a known user and
	// returns the request options plus the opaque session state.
	BeginLogin(user WebAuthnUser) (options []byte, session []byte, err error)
	// FinishLogin validates the authenticator's assertion against the stored
	// session and returns the matched credential with its updated SignCount.
	FinishLogin(user WebAuthnUser, session []byte, response []byte) (*domain.WebAuthnCredential, error)
}
