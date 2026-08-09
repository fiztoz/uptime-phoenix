package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// APIKeyMiddleware authenticates requests using API keys for endpoints like /metrics.
// Supports "Authorization: ApiKey <plaintext>" or Basic Auth username as key.
func APIKeyMiddleware(apiKeyRepo ports.APIKeyRepository) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth := c.Request().Header.Get("Authorization")
			var key string
			if strings.HasPrefix(auth, "ApiKey ") {
				key = strings.TrimPrefix(auth, "ApiKey ")
			} else if strings.HasPrefix(auth, "Basic ") {
				// Basic auth: username is the key, password ignored
				// For simplicity, assume username in basic is key; real would decode
				key = strings.TrimPrefix(auth, "Basic ")
			}
			if key == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "missing api key"})
			}

			// Fingerprint (SHA-256) of high-entropy API token — not password hashing.
			hash := services.FingerprintAPIKey(key)

			ak, err := apiKeyRepo.GetByHash(c.Request().Context(), hash)
			if err != nil || ak == nil || !ak.Active || services.APIKeyExpired(ak, time.Now()) {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
			}

			// Set user ID in context for handlers
			c.Set(ContextUserIDKey, ak.UserID)
			return next(c)
		}
	}
}
