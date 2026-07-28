package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestRateLimit_allowsHealthBypass(t *testing.T) {
	e := echo.New()
	e.Use(RateLimit(RateLimitConfig{RequestsPerSecond: 0.001, Burst: 1}))
	e.GET("/api/health/live", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/health/live", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("health check %d: got %d", i, rec.Code)
		}
	}
}

func TestRateLimit_allowsStaticAssets(t *testing.T) {
	e := echo.New()
	e.Use(RateLimit(RateLimitConfig{RequestsPerSecond: 0.001, Burst: 1}))
	e.GET("/_app/immutable/chunk.js", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/_app/immutable/chunk.js", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("static asset %d: got %d", i, rec.Code)
		}
	}
}

func TestRateLimit_blocksWhenExceeded(t *testing.T) {
	e := echo.New()
	e.Use(RateLimit(RateLimitConfig{RequestsPerSecond: 1, Burst: 1}))
	e.GET("/api/monitors", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	var last int
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/monitors", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.1")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		last = rec.Code
		if rec.Code == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatalf("expected 429, last status %d", last)
}

func TestStatusPageAccessAttemptsNeedCredentialGradeLimit(t *testing.T) {
	e := echo.New()
	e.POST("/api/status/:slug/verify-access", func(c echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	}, CredentialRateLimit(CredentialRateLimitConfig{
		MaxAttempts: 5,
		Window:      time.Minute,
	}))

	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/status/protected/verify-access", nil)
		req.RemoteAddr = "203.0.113.20:1234"
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if i < 5 && rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d throttled too early", i+1)
		}
		if i == 5 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("sixth access attempt status = %d; want 429", rec.Code)
		}
	}
}

func TestCredentialRateLimitScopesAttemptsBySlug(t *testing.T) {
	e := echo.New()
	e.POST("/api/status/:slug/verify-access", func(c echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	}, CredentialRateLimit(CredentialRateLimitConfig{
		MaxAttempts: 1,
		Window:      time.Minute,
	}))

	for _, slug := range []string{"first", "second"} {
		req := httptest.NewRequest(http.MethodPost, "/api/status/"+slug+"/verify-access", nil)
		req.RemoteAddr = "203.0.113.21:1234"
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("first attempt for %q status = %d; want 204", slug, rec.Code)
		}
	}
}
