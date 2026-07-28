package middleware

import (
	"context"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// requestIDCtxKey is the typed context key for the request ID, avoiding
// collisions with other packages that use plain string keys.
type requestIDCtxKey struct{}

// contextKeyRequestID is the context key for correlating logs across layers.
const contextKeyRequestID = "request_id"

// RequestID returns middleware that assigns a unique request ID per HTTP request.
// It honors X-Request-ID from clients when present; otherwise generates a UUID.
// The ID is stored on echo.Context and in request.Context for slog correlation.
func RequestID() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			rid := c.Request().Header.Get(echo.HeaderXRequestID)
			if rid == "" {
				rid = uuid.NewString()
			}
			c.Response().Header().Set(echo.HeaderXRequestID, rid)
			c.Set(contextKeyRequestID, rid)

			req := c.Request()
			ctx := context.WithValue(req.Context(), requestIDCtxKey{}, rid)
			c.SetRequest(req.WithContext(ctx))

			return next(c)
		}
	}
}

// RequestIDFromContext returns the request ID from context, if set.
func RequestIDFromContext(ctx context.Context) string {
	if v := ctx.Value(requestIDCtxKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
