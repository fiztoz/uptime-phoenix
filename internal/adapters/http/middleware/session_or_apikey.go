package middleware

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// apiKeyPrefix is the header prefix that marks an Authorization value as an
// API key credential (as opposed to a "Bearer <jwt>" session token).
const apiKeyPrefix = "apikey "

// SessionOrAPIKey returns an Echo middleware that authenticates a request
// via either a session JWT ("Authorization: Bearer <jwt>") or an API key
// ("Authorization: ApiKey <key>" or the "X-API-Key: <key>" header).
//
// The session token is tried first because it is the common case for
// browser-driven admin actions. If that is absent or invalid, the request
// falls back to the API key path, which additionally requires the key to
// be active and to carry requiredScope (skipped when requiredScope is "").
//
// On success the resolved user ID is stored under ContextUserIDKey (the
// same key AuthMiddleware uses) so downstream handlers do not need to know
// which credential type was presented.
func SessionOrAPIKey(authSvc *services.AuthService, apiKeyRepo ports.APIKeyRepository, requiredScope string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ctx := c.Request().Context()
			raw := c.Request().Header.Get("Authorization")

			if strings.HasPrefix(strings.ToLower(raw), bearerPrefix) {
				token := strings.TrimSpace(raw[len(bearerPrefix):])
				if token != "" {
					if userID, err := authSvc.VerifyToken(ctx, token); err == nil {
						c.Set(ContextUserIDKey, userID)
						return next(c)
					}
				}
			}

			key := apiKeyFromRequest(c, raw)
			if key != "" {
				// Fingerprint (SHA-256) of high-entropy API token — not password hashing.
				hash := services.FingerprintAPIKey(key)
				ak, err := apiKeyRepo.GetByHash(ctx, hash)
				if err == nil && ak != nil && ak.Active &&
					!services.APIKeyExpired(ak, time.Now()) &&
					(requiredScope == "" || slices.Contains(ak.Scopes, requiredScope)) {
					c.Set(ContextUserIDKey, ak.UserID)
					return next(c)
				}
			}

			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}
	}
}

// apiKeyFromRequest extracts a plaintext API key from either the
// "Authorization: ApiKey <key>" header or the "X-API-Key" header.
func apiKeyFromRequest(c echo.Context, authHeader string) string {
	if strings.HasPrefix(strings.ToLower(authHeader), apiKeyPrefix) {
		return strings.TrimSpace(authHeader[len(apiKeyPrefix):])
	}
	return strings.TrimSpace(c.Request().Header.Get("X-API-Key"))
}
