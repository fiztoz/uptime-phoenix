package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// corsTestServer builds a one-route echo app behind the CORS middleware.
func corsTestServer(cfg CORSConfig) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(CORS(cfg))
	e.GET("/", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	return e
}

// TestCORS_SecureConfigEmitsNoCORSHeaders asserts the deny-by-default effect:
// with SecureCORSConfig no Access-Control-Allow-Origin grant is ever emitted,
// for simple requests or preflights, while same-origin traffic still serves.
func TestCORS_SecureConfigEmitsNoCORSHeaders(t *testing.T) {
	e := corsTestServer(SecureCORSConfig())

	// Simple cross-origin request: request succeeds (server-side CORS is a
	// browser grant, not a firewall) but carries no grant header.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderOrigin, "https://evil.example.net")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET / = %d; want 200 (deny-by-default must not break same-origin serving)", rec.Code)
	}
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q; want no header at all", got)
	}

	// Preflight: no grant either.
	pre := httptest.NewRequest(http.MethodOptions, "/", nil)
	pre.Header.Set(echo.HeaderOrigin, "https://evil.example.net")
	pre.Header.Set(echo.HeaderAccessControlRequestMethod, http.MethodGet)
	preRec := httptest.NewRecorder()
	e.ServeHTTP(preRec, pre)
	if got := preRec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("preflight Access-Control-Allow-Origin = %q; want no header at all", got)
	}
}

// TestCORS_DefaultConfigAllowsAnyOrigin documents the permissive dev default
// this middleware ships when explicitly asked for it.
func TestCORS_DefaultConfigAllowsAnyOrigin(t *testing.T) {
	e := corsTestServer(DefaultCORSConfig())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderOrigin, "https://anywhere.example.net")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q; want *", got)
	}
}

// TestCORS_ExplicitAllowlistGrantsListedOriginOnly asserts the production
// override path: a configured origin is granted, an unlisted one is not.
func TestCORS_ExplicitAllowlistGrantsListedOriginOnly(t *testing.T) {
	cfg := DefaultCORSConfig()
	cfg.AllowOrigins = []string{"https://ok.example.com"}
	e := corsTestServer(cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderOrigin, "https://ok.example.com")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "https://ok.example.com" {
		t.Errorf("allowed origin grant = %q; want https://ok.example.com", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderOrigin, "https://evil.example.net")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if got := rec.Header().Get(echo.HeaderAccessControlAllowOrigin); got != "" {
		t.Errorf("unlisted origin grant = %q; want no header", got)
	}
}
