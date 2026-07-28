package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestRequestID_generatesAndEchoesHeader(t *testing.T) {
	e := echo.New()
	e.Use(RequestID())
	e.GET("/", func(c echo.Context) error {
		if c.Response().Header().Get(echo.HeaderXRequestID) == "" {
			t.Fatal("expected X-Request-ID on response")
		}
		if RequestIDFromContext(c.Request().Context()) == "" {
			t.Fatal("expected request_id in context")
		}
		return c.NoContent(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Header().Get(echo.HeaderXRequestID) == "" {
		t.Fatal("missing response X-Request-ID")
	}
}

func TestRequestID_honoursClientHeader(t *testing.T) {
	e := echo.New()
	e.Use(RequestID())
	e.GET("/", func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(echo.HeaderXRequestID, "client-rid-42")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if got := rec.Header().Get(echo.HeaderXRequestID); got != "client-rid-42" {
		t.Fatalf("got %q want client-rid-42", got)
	}
}
