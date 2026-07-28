// Package auth provides adapters for the ports.Authenticator interface.
package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ticketPurpose is a private JWT claim value that distinguishes a 2FA
// challenge ticket from a regular session token. Keeping it as a typed
// constant prevents typos and keeps all usages auditable.
const ticketPurpose = "2fa_challenge"

// ticketTTL is the lifetime of a 2FA challenge ticket — short enough to
// limit the window in which a stolen ticket can be redeemed, long enough
// to let a user fumble for their authenticator app.
const ticketTTL = 5 * time.Minute

// JWTAuthenticator implements ports.Authenticator using HS256-signed JWTs.
type JWTAuthenticator struct {
	signingKey  []byte
	expireHours int
	userRepo    ports.UserRepository
}

// NewJWTAuthenticator creates a new JWT-based authenticator.
// signingKey is the HMAC secret used to sign and verify tokens.
// expireHours controls the lifetime of a regular session token.
func NewJWTAuthenticator(signingKey string, expireHours int, userRepo ports.UserRepository) *JWTAuthenticator {
	return &JWTAuthenticator{
		signingKey:  []byte(signingKey),
		expireHours: expireHours,
		userRepo:    userRepo,
	}
}

// Login authenticates a user and returns a JWT session token.
func (a *JWTAuthenticator) Login(ctx context.Context, username, password string) (string, error) {
	user, err := a.userRepo.GetByUsername(ctx, username)
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	if !user.Active {
		return "", fmt.Errorf("login: user is inactive")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", fmt.Errorf("login: invalid credentials")
	}

	return a.signToken(user.ID, "session", time.Duration(a.expireHours)*time.Hour)
}

// VerifyToken validates a JWT session token and returns the user ID.
//
// Tokens that are valid but carry a non-session purpose (e.g. a 2FA ticket)
// are rejected — only the AuthMiddleware and similar session consumers
// should use this method. The 2FA ticket has its own verifier.
func (a *JWTAuthenticator) VerifyToken(ctx context.Context, tokenStr string) (int64, error) {
	claims, err := a.parseClaims(tokenStr)
	if err != nil {
		return 0, err
	}
	if purpose, _ := claims["purpose"].(string); purpose != "session" {
		return 0, fmt.Errorf("verify token: unexpected token purpose %q", purpose)
	}
	return a.userIDFromClaims(claims)
}

// IssuePending2FATicket signs a short-lived ticket that records a successful
// password step. It carries only the user ID and a purpose claim, never
// any privileges on its own.
func (a *JWTAuthenticator) IssuePending2FATicket(ctx context.Context, userID int64) (string, error) {
	return a.signToken(userID, ticketPurpose, ticketTTL)
}

// IssueSession mints a regular session JWT for an already-authenticated
// user. The token is signed with the same key and shares the session
// expiry configured at construction time.
func (a *JWTAuthenticator) IssueSession(ctx context.Context, userID int64) (string, error) {
	return a.signToken(userID, "session", time.Duration(a.expireHours)*time.Hour)
}

// VerifyPending2FATicket validates a 2FA challenge ticket and returns the
// user ID. Like VerifyToken, it rejects any token that is not a ticket
// (purpose mismatch) to make ticket → session confusion impossible.
func (a *JWTAuthenticator) VerifyPending2FATicket(ctx context.Context, tokenStr string) (int64, error) {
	claims, err := a.parseClaims(tokenStr)
	if err != nil {
		return 0, err
	}
	if purpose, _ := claims["purpose"].(string); purpose != ticketPurpose {
		return 0, fmt.Errorf("verify 2fa ticket: unexpected token purpose %q", purpose)
	}
	return a.userIDFromClaims(claims)
}

// signToken mints an HS256 JWT containing the user ID, the token purpose,
// and the issued-at / expires-at claims required by RFC 7519 §4.1.
func (a *JWTAuthenticator) signToken(userID int64, purpose string, ttl time.Duration) (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     userID,
		"purpose": purpose,
		"iat":     now.Unix(),
		"exp":     now.Add(ttl).Unix(),
	})
	signed, err := token.SignedString(a.signingKey)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// parseClaims verifies the signature, algorithm, and expiry of a token
// and returns its claims. All errors funnel through a single fmt.Errorf
// wrapper so callers can use errors.Is / errors.As for higher-level
// mapping (e.g. "401 Unauthorized") without leaking JWT internals.
func (a *JWTAuthenticator) parseClaims(tokenStr string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return a.signingKey, nil
	})
	if err != nil {
		return nil, fmt.Errorf("verify token: %w", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("verify token: invalid claims")
	}
	return claims, nil
}

// userIDFromClaims extracts and validates the "sub" claim as an int64.
func (a *JWTAuthenticator) userIDFromClaims(claims jwt.MapClaims) (int64, error) {
	sub, ok := claims["sub"].(float64)
	if !ok {
		return 0, fmt.Errorf("verify token: sub claim missing or invalid")
	}
	return int64(sub), nil
}

// HashPassword hashes a plaintext password using bcrypt.
func (a *JWTAuthenticator) HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword compares a plaintext password against a bcrypt hash.
func (a *JWTAuthenticator) VerifyPassword(hashed, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(password))
}

// Ensure JWTAuthenticator implements Authenticator.
var _ ports.Authenticator = (*JWTAuthenticator)(nil)
