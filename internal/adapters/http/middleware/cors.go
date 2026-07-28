// Package middleware contains Echo middleware for the Phoenix HTTP layer.
package middleware

import (
	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
)

// CORSConfig holds the runtime configuration for the CORS middleware.
type CORSConfig struct {
	// AllowOrigins is the list of origins permitted by the
	// Access-Control-Allow-Origin header. Use []string{"*"} for the
	// dev default. Production deployments should be set to a
	// comma-separated allow-list via environment configuration.
	AllowOrigins []string
	// AllowMethods is the list of HTTP methods advertised in
	// Access-Control-Allow-Methods for preflight responses.
	AllowMethods []string
	// AllowHeaders is the list of headers advertised in
	// Access-Control-Allow-Headers for preflight responses.
	AllowHeaders []string
	// DisableCrossOrigin switches CORS off entirely: the middleware becomes
	// a no-op, no Access-Control-Allow-* headers are ever emitted, and
	// browsers therefore refuse cross-origin reads. This is the production
	// default when CORS_ALLOW_ORIGINS is not set — deny by default rather
	// than the dev wildcard.
	DisableCrossOrigin bool
}

// DefaultCORSConfig is the permissive dev configuration. The wildcard
// origin is safe to use in local development because credentials are
// not exposed via Access-Control-Allow-Credentials in this config. A
// production deployment should override AllowOrigins to a fixed
// allow-list (see values.yaml / configmap.yaml).
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{
			echo.GET, echo.HEAD, echo.PUT, echo.PATCH, echo.POST, echo.DELETE,
		},
		AllowHeaders: []string{
			echo.HeaderOrigin,
			echo.HeaderContentType,
			echo.HeaderAccept,
			echo.HeaderAuthorization,
			"X-Requested-With",
		},
	}
}

// SecureCORSConfig is the deny-by-default production configuration: no
// cross-origin browser access is granted at all. Production deployments
// that serve a browser frontend from another origin must opt in via
// CORS_ALLOW_ORIGINS.
func SecureCORSConfig() CORSConfig {
	return CORSConfig{DisableCrossOrigin: true}
}

// CORS returns Echo's built-in CORS middleware configured from the
// supplied CORSConfig. We wrap Echo's middleware (rather than writing
// our own) so we inherit its preflight short-circuit, header
// canonicalisation, and per-origin regular-expression handling.
//
// A DisableCrossOrigin config yields a pass-through middleware instead:
// with no Access-Control-Allow-Origin grant ever emitted, browsers refuse
// cross-origin reads while same-origin traffic is unaffected.
func CORS(cfg CORSConfig) echo.MiddlewareFunc {
	if cfg.DisableCrossOrigin {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	if len(cfg.AllowOrigins) == 0 {
		cfg.AllowOrigins = DefaultCORSConfig().AllowOrigins
	}
	if len(cfg.AllowMethods) == 0 {
		cfg.AllowMethods = DefaultCORSConfig().AllowMethods
	}
	if len(cfg.AllowHeaders) == 0 {
		cfg.AllowHeaders = DefaultCORSConfig().AllowHeaders
	}
	return echomw.CORSWithConfig(echomw.CORSConfig{
		AllowOrigins: cfg.AllowOrigins,
		AllowMethods: cfg.AllowMethods,
		AllowHeaders: cfg.AllowHeaders,
	})
}
