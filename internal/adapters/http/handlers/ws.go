// Package handlers contains Echo HTTP handlers for the Phoenix REST API.
package handlers

import (
	"net/http"

	"github.com/coder/websocket"
	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/ws"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// WSConfig controls the origin policy applied to WebSocket upgrades.
//
// The zero value is the SECURE default: coder/websocket's same-origin check
// runs against the request Host, and cross-origin upgrades are refused.
type WSConfig struct {
	// AllowedOriginPatterns lists extra origin HOST patterns (no scheme —
	// e.g. "status.example.com" or "localhost:5173") accepted in addition
	// to same-origin requests. See websocket.AcceptOptions.OriginPatterns.
	AllowedOriginPatterns []string
	// InsecureSkipOriginCheck disables origin verification entirely. Dev
	// convenience only (the Vite dev server on :5173 talks to the API on
	// :3000); never enable it in production — any website could then open
	// authenticated WebSocket connections from a visitor's browser.
	InsecureSkipOriginCheck bool
}

// wsAcceptOptions translates the origin policy into coder/websocket accept
// options. Kept as a pure function so the policy is unit-testable without
// driving a full WebSocket handshake.
func wsAcceptOptions(cfg WSConfig) *websocket.AcceptOptions {
	return &websocket.AcceptOptions{
		InsecureSkipVerify: cfg.InsecureSkipOriginCheck,
		OriginPatterns:     cfg.AllowedOriginPatterns,
	}
}

// WSHandlers groups the WebSocket upgrade endpoint behind a single receiver.
type WSHandlers struct {
	hub     *ws.Hub
	authSvc *services.AuthService
	cfg     WSConfig
}

// NewWSHandlers creates a WSHandlers with the given hub, auth service, and
// origin policy. Pass the zero WSConfig for the secure same-origin default.
func NewWSHandlers(hub *ws.Hub, authSvc *services.AuthService, cfg WSConfig) *WSHandlers {
	return &WSHandlers{hub: hub, authSvc: authSvc, cfg: cfg}
}

// HandleWS upgrades the HTTP connection to WebSocket using coder/websocket.Accept.
// Authentication is performed via the JWT token in the query parameter (since
// browsers cannot set custom headers on WebSocket upgrade requests).
//
// The origin check is ON by default: same-origin requests plus any hosts in
// cfg.AllowedOriginPatterns. cfg.InsecureSkipOriginCheck turns it off for
// dev setups only.
//
// GET /ws?token=<jwt>
func (h *WSHandlers) HandleWS(c echo.Context) error {
	// Accept the WebSocket upgrade. On a refused origin, Accept has already
	// written its own 403 before returning the error, so the JSON below only
	// reaches the wire for failures that never wrote a response.
	conn, err := websocket.Accept(c.Response(), c.Request(), wsAcceptOptions(h.cfg))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "websocket upgrade failed"})
	}

	// Extract JWT from query parameter.
	jwt := c.QueryParam("token")

	// Carry the JWT on the context via a typed key consumed by the hub.
	ctx := ws.WithJWT(c.Request().Context(), jwt)

	h.hub.HandleWebSocket(ctx, conn, h.authSvc)

	return nil
}
