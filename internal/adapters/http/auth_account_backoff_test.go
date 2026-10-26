package http_test

import (
	"context"
	"embed"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// The strict per-account backoff is keyed by (account, client IP), so the
// handler must hand the service the request's client IP. If it did not,
// every client would share one bucket and a remote guesser could lock the
// owner out from everywhere with five requests.
func TestLogin_AccountBackoffFollowsTheClientIP(t *testing.T) {
	users := memory.NewUserRepo()
	jwt := auth.NewJWTAuthenticator(strings.Repeat("auth-backoff-test-key-", 2), 1, users)
	authSvc := services.NewAuthService(users, nil, jwt, nil)
	if _, err := authSvc.Register(context.Background(), "owner", "correct-horse-battery"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := httppkg.NewRouter(
		handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		authSvc, nil, nil, nil, embed.FS{}, httppkg.RouterOptions{}, nil, "")

	login := func(ip, password string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
			strings.NewReader(`{"username":"owner","password":"`+password+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	const attacker, owner = "203.0.113.66", "192.0.2.20"
	for i := 0; i < services.LoginFailuresBeforeBackoff; i++ {
		if code := login(attacker, "guess"); code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d; want 401", i+1, code)
		}
	}
	// Below the per-IP route limit, so this 429 is the account backoff.
	if code := login(attacker, "correct-horse-battery"); code != http.StatusTooManyRequests {
		t.Fatalf("attacker after %d failures = %d; want 429", services.LoginFailuresBeforeBackoff, code)
	}
	if code := login(owner, "correct-horse-battery"); code != http.StatusOK {
		t.Fatalf("owner from another IP = %d; want 200 (backoff must not follow the attacker's IP)", code)
	}
}
