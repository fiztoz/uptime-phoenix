package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// unsubscribeTokenTTL is long enough for dormant subscribers while still
// bounded. Tokens become useless the moment the subscriber row is deleted.
const unsubscribeTokenTTL = 10 * 365 * 24 * time.Hour

// SubscriberTokenCodec issues purpose-bound JWTs for status-page email
// confirmation and unsubscribe flows, signed with the same HMAC secret as
// session tokens.
type SubscriberTokenCodec struct {
	signingKey []byte
}

// NewSubscriberTokenCodec creates a codec using the application JWT secret.
func NewSubscriberTokenCodec(signingKey string) *SubscriberTokenCodec {
	return &SubscriberTokenCodec{signingKey: []byte(signingKey)}
}

// IssueConfirmation implements ports.SubscriberTokenCodec.
func (c *SubscriberTokenCodec) IssueConfirmation(subscriberID int64, expiresAt time.Time) (string, error) {
	return c.sign(subscriberID, ports.SubscriberTokenConfirm, expiresAt.UTC())
}

// IssueUnsubscribe implements ports.SubscriberTokenCodec.
func (c *SubscriberTokenCodec) IssueUnsubscribe(subscriberID int64) (string, error) {
	return c.sign(subscriberID, ports.SubscriberTokenUnsubscribe, time.Now().UTC().Add(unsubscribeTokenTTL))
}

// Verify implements ports.SubscriberTokenCodec.
func (c *SubscriberTokenCodec) Verify(tokenStr string, expectedPurpose string) (int64, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method", ports.ErrSubscriberToken)
		}
		return c.signingKey, nil
	})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ports.ErrSubscriberToken, err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return 0, fmt.Errorf("%w: invalid claims", ports.ErrSubscriberToken)
	}
	purpose, _ := claims["purpose"].(string)
	if purpose != expectedPurpose {
		return 0, fmt.Errorf("%w: unexpected purpose %q", ports.ErrSubscriberToken, purpose)
	}
	sub, ok := claims["sub"].(float64)
	if !ok {
		return 0, fmt.Errorf("%w: missing sub", ports.ErrSubscriberToken)
	}
	return int64(sub), nil
}

func (c *SubscriberTokenCodec) sign(subscriberID int64, purpose string, expiresAt time.Time) (string, error) {
	now := time.Now().UTC()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     subscriberID,
		"purpose": purpose,
		"iat":     now.Unix(),
		"exp":     expiresAt.Unix(),
	})
	signed, err := token.SignedString(c.signingKey)
	if err != nil {
		return "", fmt.Errorf("sign subscriber token: %w", err)
	}
	return signed, nil
}

var _ ports.SubscriberTokenCodec = (*SubscriberTokenCodec)(nil)
