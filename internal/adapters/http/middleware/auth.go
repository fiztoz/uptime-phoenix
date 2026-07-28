// Package middleware contains Echo middleware for the Phoenix HTTP layer.
package middleware

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// ContextUserIDKey is re-exported here so middleware callers do not
// have to import the handlers package just to read the user ID. The
// underlying value is the same string, so writes from middleware are
// visible to handlers.Keep them in sync.
const ContextUserIDKey = handlers.ContextUserIDKey

// bearerPrefix is the case-insensitive prefix that marks an HTTP
// Authorization header as a JWT bearer credential. Echo strips the
// prefix when wrapping the value, so we re-add a single canonical
// form here for consistent error reporting.
const bearerPrefix = "bearer "

// AuthMiddleware returns an Echo middleware that requires a valid
// JWT in the Authorization: Bearer header.
//
// On success it stores the verified user ID under ContextUserIDKey so
// downstream handlers can read it via c.Get("userID"). On failure it
// short-circuits with 401 and a JSON error body.
func AuthMiddleware(authSvc *services.AuthService) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			raw := c.Request().Header.Get("Authorization")
			if raw == "" {
				return unauthorized(c, "missing authorization header")
			}
			if !strings.HasPrefix(strings.ToLower(raw), bearerPrefix) {
				return unauthorized(c, "authorization header must use Bearer scheme")
			}
			token := strings.TrimSpace(raw[len(bearerPrefix):])
			if token == "" {
				return unauthorized(c, "empty bearer token")
			}
			userID, err := authSvc.VerifyToken(c.Request().Context(), token)
			if err != nil {
				return unauthorized(c, "invalid or expired token")
			}
			c.Set(ContextUserIDKey, userID)
			return next(c)
		}
	}
}

// OptionalAuthMiddleware behaves like AuthMiddleware but does not
// reject requests without a token. When a valid token is present the
// userID is set; when it is missing or invalid, the request continues
// anonymously. This is the right middleware for endpoints that have a
// different behavior for logged-in users (e.g. a status page that
// shows extra detail to authenticated admins) but are still public.
func OptionalAuthMiddleware(authSvc *services.AuthService) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			raw := c.Request().Header.Get("Authorization")
			if raw == "" {
				return next(c)
			}
			if !strings.HasPrefix(strings.ToLower(raw), bearerPrefix) {
				// We do not reject — we just skip the userID lookup.
				return next(c)
			}
			token := strings.TrimSpace(raw[len(bearerPrefix):])
			if token == "" {
				return next(c)
			}
			userID, err := authSvc.VerifyToken(c.Request().Context(), token)
			if err == nil {
				c.Set(ContextUserIDKey, userID)
			}
			// Any other error: silently treat as anonymous.
			return next(c)
		}
	}
}

// unauthorized is the canonical 401 response body. It matches the
// error envelope used by the auth handlers ({"error": "..."}) so
// clients can rely on a single shape across the API.
func unauthorized(c echo.Context, msg string) error {
	return c.JSON(http.StatusUnauthorized, map[string]string{"error": msg})
}
