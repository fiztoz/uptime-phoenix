package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestSessionPrincipalRevocation(t *testing.T) {
	for _, transport := range []string{"bearer", "cookie", "session or key", "optional"} {
		t.Run(transport, func(t *testing.T) {
			users := memory.NewUserRepo()
			user := &domain.User{Username: "session-owner", Active: true}
			if err := users.Create(t.Context(), user); err != nil {
				t.Fatal(err)
			}
			jwt := auth.NewJWTAuthenticator(strings.Repeat("fixture", 8), 1, users)
			svc := services.NewAuthService(users, nil, jwt, nil)
			token, err := jwt.IssueSession(t.Context(), user.ID)
			if err != nil {
				t.Fatal(err)
			}
			gate := middleware.AuthMiddleware(svc)
			switch transport {
			case "cookie":
				gate = middleware.BearerOrSessionCookie(svc)
			case "session or key":
				gate = middleware.SessionOrAPIKey(svc, nil, "read")
			case "optional":
				gate = middleware.OptionalAuthMiddleware(svc)
			}
			reached := false
			e := echo.New()
			e.GET("/protected", func(c echo.Context) error {
				reached = true
				if transport == "optional" && c.Get(middleware.ContextUserIDKey) == nil {
					return c.NoContent(http.StatusOK)
				}
				return c.NoContent(http.StatusNoContent)
			}, gate)
			request := func(want int, wantReached bool) {
				t.Helper()
				reached = false
				req := httptest.NewRequest(http.MethodGet, "/protected", nil)
				if transport == "cookie" {
					req.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: token})
				} else {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				if rec.Code != want || reached != wantReached {
					t.Fatalf("HTTP=%d reached=%v; want %d/%v", rec.Code, reached, want, wantReached)
				}
			}
			request(http.StatusNoContent, true)
			user.Active = false
			if err := users.Update(t.Context(), user); err != nil {
				t.Fatal(err)
			}
			want, wantReached := http.StatusUnauthorized, false
			if transport == "optional" {
				want, wantReached = http.StatusOK, true
			}
			request(want, wantReached)
			if err := users.Delete(t.Context(), user.ID); err != nil {
				t.Fatal(err)
			}
			request(want, wantReached)
		})
	}
}
