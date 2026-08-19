package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestNormalizeSessionExpireHours(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want int
	}{
		{24, 24},
		{168, 168},
		{0, defaultSessionExpireHours},
		{-1, defaultSessionExpireHours},
		{-1920, defaultSessionExpireHours},
		{maxSessionExpireHours, maxSessionExpireHours},
		{maxSessionExpireHours + 1, maxSessionExpireHours},
	}
	for _, tc := range cases {
		if got := normalizeSessionExpireHours(tc.in); got != tc.want {
			t.Errorf("normalizeSessionExpireHours(%d) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

func TestIssueSession_ZeroExpireHoursMints24hToken(t *testing.T) {
	t.Parallel()
	a := NewJWTAuthenticator("test-signing-key", 0, nil)
	tok, err := a.IssueSession(context.Background(), 1)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	assertSessionTTL(t, tok, "test-signing-key", 24*time.Hour)
}

func TestIssueSession_NegativeExpireHoursMints24hToken(t *testing.T) {
	t.Parallel()
	// The v2 "pending forever" token was issued with exp ~80 days before iat.
	// A negative JWT_EXPIRE_HOURS is the only way signToken produces that.
	a := NewJWTAuthenticator("test-signing-key", -1920, nil)
	tok, err := a.IssueSession(context.Background(), 1)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	assertSessionTTL(t, tok, "test-signing-key", 24*time.Hour)
}

func TestIssueSession_ConfiguredTTL(t *testing.T) {
	t.Parallel()
	a := NewJWTAuthenticator("test-signing-key", 168, nil)
	tok, err := a.IssueSession(context.Background(), 7)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	assertSessionTTL(t, tok, "test-signing-key", 168*time.Hour)
}

func TestVerifyToken_RejectsExpiredSession(t *testing.T) {
	t.Parallel()
	a := NewJWTAuthenticator("test-signing-key", 24, nil)
	now := time.Now().Add(-2 * time.Hour)
	raw := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":     int64(1),
		"purpose": "session",
		"iat":     now.Unix(),
		"exp":     now.Add(time.Hour).Unix(),
	})
	tok, err := raw.SignedString([]byte("test-signing-key"))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := a.VerifyToken(context.Background(), tok); err == nil {
		t.Fatal("VerifyToken accepted an expired session; want error")
	}
}

func TestVerifyToken_AcceptsFreshSession(t *testing.T) {
	t.Parallel()
	a := NewJWTAuthenticator("test-signing-key", 24, nil)
	tok, err := a.IssueSession(context.Background(), 1)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	uid, err := a.VerifyToken(context.Background(), tok)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if uid != 1 {
		t.Errorf("user id = %d; want 1", uid)
	}
}

func assertSessionTTL(t *testing.T, token, key string, want time.Duration) {
	t.Helper()
	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (any, error) {
		return []byte(key), nil
	})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("token claims are not MapClaims")
	}
	exp, err := claims.GetExpirationTime()
	if err != nil {
		t.Fatalf("exp: %v", err)
	}
	iat, err := claims.GetIssuedAt()
	if err != nil {
		t.Fatalf("iat: %v", err)
	}
	got := exp.Sub(iat.Time)
	slack := 2 * time.Second
	if got < want-slack || got > want+slack {
		t.Errorf("session TTL = %s; want %s ± %s", got, want, slack)
	}
	if !exp.After(time.Now()) {
		t.Errorf("token exp %s is not in the future; minted already-expired", exp.Time)
	}
}
