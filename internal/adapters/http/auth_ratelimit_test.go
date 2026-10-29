package http_test

import (
	"embed"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// authRateLimitRouter builds the real router with only the auth handlers.
// Requests carry an empty JSON body, so every handler rejects them as
// malformed; the point is the limiter in front of it.
func authRateLimitRouter(t *testing.T) http.Handler {
	t.Helper()
	return authRateLimitRouterWithOptions(t, httppkg.RouterOptions{})
}

func authRateLimitRouterWithOptions(t *testing.T, opts httppkg.RouterOptions) *echo.Echo {
	t.Helper()
	users := memory.NewUserRepo()
	jwt := auth.NewJWTAuthenticator(strings.Repeat("auth-ratelimit-test-key-", 2), 1, users)
	authSvc := services.NewAuthService(users, nil, jwt, nil)
	return httppkg.NewRouter(
		handlers.NewHealthHandlers(func() bool { return true }), handlers.NewAuthHandlers(authSvc), nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
		authSvc, nil, nil, nil, embed.FS{}, opts, nil, "")
}

func postFrom(h http.Handler, path, ip string) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestAuthRoutes_SignInBudgetIsSharedAndPerIP(t *testing.T) {
	h := authRateLimitRouter(t)
	const ip = "198.51.100.7"
	signIn := []string{"/api/auth/login", "/api/auth/verify-2fa", "/api/auth/webauthn/login/finish"}

	// Spread the budget across all three verification endpoints.
	for i := 0; i < httppkg.SignInAttemptsPerMinute; i++ {
		path := signIn[i%len(signIn)]
		if code := postFrom(h, path, ip); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d on %s throttled early", i+1, path)
		}
	}
	for _, path := range signIn {
		if code := postFrom(h, path, ip); code != http.StatusTooManyRequests {
			t.Fatalf("%s over budget = %d; want 429", path, code)
		}
	}
	// Another client is unaffected.
	if code := postFrom(h, "/api/auth/login", "203.0.113.9"); code == http.StatusTooManyRequests {
		t.Fatal("other IP throttled; want per-IP budget")
	}
	// Starting a passkey ceremony has its own budget.
	if code := postFrom(h, "/api/auth/webauthn/login/begin", ip); code == http.StatusTooManyRequests {
		t.Fatal("webauthn begin shares the sign-in budget; want its own")
	}
}

func TestAuthRoutes_BeginAndRegisterAreThrottled(t *testing.T) {
	h := authRateLimitRouter(t)
	const ip = "198.51.100.8"
	for i := 0; i < httppkg.AuthBeginAttemptsPerMinute; i++ {
		path := "/api/auth/webauthn/login/begin"
		if i%2 == 1 {
			path = "/api/auth/register"
		}
		if code := postFrom(h, path, ip); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d on %s throttled early", i+1, path)
		}
	}
	for _, path := range []string{"/api/auth/webauthn/login/begin", "/api/auth/register"} {
		if code := postFrom(h, path, ip); code != http.StatusTooManyRequests {
			t.Fatalf("%s over budget = %d; want 429", path, code)
		}
	}
}
