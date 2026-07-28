package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// RequireAdmin returns middleware that restricts a route to admin users.
//
// It must run after a middleware that has already authenticated the
// request and stored the resolved user ID under ContextUserIDKey (e.g.
// AuthMiddleware or SessionOrAPIKey) — RequireAdmin only performs the
// authorization check, not authentication. Phoenix has no separate roles
// table; "admin" is the domain.User.IsAdmin flag.
func RequireAdmin(authSvc *services.AuthService) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			userID, ok := contextUserID(c)
			if !ok {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			}
			user, err := authSvc.GetUser(c.Request().Context(), userID)
			if err != nil || !user.IsAdmin {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "admin privileges required"})
			}
			return next(c)
		}
	}
}

// contextUserID extracts the userID stored under ContextUserIDKey by an
// upstream auth middleware. It accepts the handful of numeric types a
// context value might arrive as, mirroring handlers.userIDFromContext.
func contextUserID(c echo.Context) (int64, bool) {
	switch v := c.Get(ContextUserIDKey).(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	default:
		return 0, false
	}
}
