package middleware

import (
	"github.com/labstack/echo/v4"
)

// SecurityHeadersConfig controls baseline HTTP security headers.
type SecurityHeadersConfig struct {
	// Production enables a stricter Content-Security-Policy for the embedded SPA.
	Production bool
}

// SecurityHeaders sets common hardening headers on every response.
func SecurityHeaders(cfg SecurityHeadersConfig) echo.MiddlewareFunc {
	csp := "default-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'"
	if !cfg.Production {
		// Dev: allow inline scripts from Vite/Svelte builds without breaking HMR.
		csp = "default-src 'self' 'unsafe-inline' 'unsafe-eval'; connect-src 'self' ws: wss:; img-src 'self' data: blob:; frame-ancestors 'self'"
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "SAMEORIGIN")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Content-Security-Policy", csp)
			return next(c)
		}
	}
}
