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

// SessionCookieName is the HttpOnly cookie that lets an <iframe src>
// navigation reach the gated /api/extensions/:id/frame launch endpoint.
// A browser navigation cannot carry the Authorization header (the JWT
// lives in localStorage and is attached only by the SPA fetch client), so
// the Bearer-authenticated catalog list response sets this short-lived,
// /api/extensions-scoped cookie (see SetSessionCookie) and the frame route
// accepts it via BearerOrSessionCookie. It is never sent to any other route.
const SessionCookieName = "phoenix_session"

// BearerToken extracts the credential from an Authorization: Bearer
// header. It returns "" when the header is absent or not a Bearer value.
// Exported so a handler that has just authenticated a Bearer request can
// copy the same token into the session cookie (see SetSessionCookie).
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(h), bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(h[len(bearerPrefix):])
}

// SetSessionCookie writes a scoped HttpOnly cookie carrying the supplied
// token. An <iframe src> navigation cannot carry an Authorization header,
// so the gated /api/extensions/:id/frame launch endpoint would otherwise
// reject every real browser launch with 401. The catalog list response
// sets this cookie after a Bearer-authenticated fetch, and the frame route
// accepts it via BearerOrSessionCookie. Path=/api/extensions means
// state-changing routes (POST /api/monitors, etc.) never receive it, so no
// CSRF surface is added. Secure under TLS; SameSite=Lax so only same-site
// (Phoenix-host) iframe navigations send it.
func SetSessionCookie(c echo.Context, token string) {
	if token == "" {
		return
	}
	c.SetCookie(&http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/api/extensions",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   c.Scheme() == "https",
		MaxAge:   86400,
	})
}

// BearerOrSessionCookie authenticates like AuthMiddleware but also accepts
// the phoenix_session cookie set by SetSessionCookie. Use it only on routes
// an iframe must reach (the extension launch redirect); keep AuthMiddleware
// on every other route so the cookie never broadens their CSRF surface.
func BearerOrSessionCookie(authSvc *services.AuthService) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			raw := c.Request().Header.Get("Authorization")
			if raw == "" {
				if ck, err := c.Cookie(SessionCookieName); err == nil && ck.Value != "" {
					raw = bearerPrefix + ck.Value
				}
			}
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

// IssueSessionCookieOnBearer sets the phoenix_session cookie (see
// SetSessionCookie) after a successful Bearer-authenticated response, so a
// subsequent <iframe src=/api/extensions/:id/frame> navigation can reach the
// gated launch redirect. Apply after AuthMiddleware on the catalog list
// route; it reads the Bearer token from the request header and only writes
// the cookie when the handler succeeded (2xx). An iframe navigation cannot
// carry the Authorization header, so this cookie is the launch transport.
func IssueSessionCookieOnBearer(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		token := BearerToken(c.Request())
		err := next(c)
		if token != "" && err == nil && c.Response().Status < http.StatusBadRequest {
			SetSessionCookie(c, token)
		}
		return err
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
