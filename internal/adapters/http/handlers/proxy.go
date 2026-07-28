package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// errProxyNotFound is the client-facing message used whenever a proxy
// doesn't exist or isn't owned by the caller. Reusing a single message
// avoids leaking whether the ID exists under another user (same pattern as
// errMaintenanceNotFound in maintenance.go).
const errProxyNotFound = "proxy not found"

// ProxyHandlers groups the outbound-proxy CRUD endpoints behind a single receiver.
type ProxyHandlers struct {
	svc *services.ProxyService
}

// NewProxyHandlers creates a ProxyHandlers bound to the supplied service.
func NewProxyHandlers(svc *services.ProxyService) *ProxyHandlers {
	return &ProxyHandlers{svc: svc}
}

// ProxyView is the wire shape of domain.Proxy.
//
// It deliberately OMITS Password. The domain type carries no json tags and
// stores the proxy credential in plaintext (it has to be dial-able), so
// returning domain.Proxy directly would leak it over the wire — see the
// Wire-Shape Discipline rule in AGENTS.md. Auth is exposed as a boolean and
// Username is safe to return; Password must never appear in a response.
type ProxyView struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Protocol  string `json:"protocol"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Auth      bool   `json:"auth"`
	Username  string `json:"username"`
	Active    bool   `json:"active"`
	IsDefault bool   `json:"is_default"`
}

func toProxyView(p *domain.Proxy) *ProxyView {
	if p == nil {
		return nil
	}
	return &ProxyView{
		ID:        p.ID,
		UserID:    p.UserID,
		Protocol:  p.Protocol,
		Host:      p.Host,
		Port:      p.Port,
		Auth:      p.Auth,
		Username:  p.Username,
		Active:    p.Active,
		IsDefault: p.IsDefault,
	}
}

func toProxyViews(proxies []*domain.Proxy) []*ProxyView {
	out := make([]*ProxyView, 0, len(proxies))
	for _, p := range proxies {
		out = append(out, toProxyView(p))
	}
	return out
}

// upsertProxyRequest is the shared body shape for POST/PUT.
// Password is a pointer so PUT can omit it to keep the existing credential
// (only overwriting it when the caller explicitly sends a new one).
type upsertProxyRequest struct {
	Protocol  string  `json:"protocol"`
	Host      string  `json:"host"`
	Port      int     `json:"port"`
	Auth      bool    `json:"auth"`
	Username  string  `json:"username"`
	Password  *string `json:"password"`
	Active    *bool   `json:"active"`
	IsDefault bool    `json:"is_default"`
}

// Create handles POST /api/proxies.
func (h *ProxyHandlers) Create(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	var req upsertProxyRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	active := true
	if req.Active != nil {
		active = *req.Active
	}

	p := &domain.Proxy{
		UserID:    userID,
		Protocol:  req.Protocol,
		Host:      req.Host,
		Port:      req.Port,
		Auth:      req.Auth,
		Username:  req.Username,
		Active:    active,
		IsDefault: req.IsDefault,
	}
	if req.Password != nil {
		p.Password = *req.Password
	}

	if err := h.svc.Create(c.Request().Context(), p); err != nil {
		return mapProxyError(c, err)
	}
	return c.JSON(http.StatusCreated, toProxyView(p))
}

// List handles GET /api/proxies.
func (h *ProxyHandlers) List(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	proxies, err := h.svc.List(c.Request().Context(), userID)
	if err != nil {
		return mapProxyError(c, err)
	}
	return c.JSON(http.StatusOK, toProxyViews(proxies))
}

// Update handles PUT /api/proxies/:id.
func (h *ProxyHandlers) Update(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}

	existing, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapProxyError(c, err)
	}
	if existing.UserID != userID {
		return c.JSON(http.StatusNotFound, errorBody(errProxyNotFound))
	}

	var req upsertProxyRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	existing.Protocol = req.Protocol
	existing.Host = req.Host
	existing.Port = req.Port
	existing.Auth = req.Auth
	existing.Username = req.Username
	if req.Password != nil {
		existing.Password = *req.Password
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}
	existing.IsDefault = req.IsDefault

	if err := h.svc.Update(c.Request().Context(), existing); err != nil {
		return mapProxyError(c, err)
	}
	return c.JSON(http.StatusOK, toProxyView(existing))
}

// Delete handles DELETE /api/proxies/:id.
func (h *ProxyHandlers) Delete(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid id")
	}

	existing, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapProxyError(c, err)
	}
	if existing.UserID != userID {
		return c.JSON(http.StatusNotFound, errorBody(errProxyNotFound))
	}

	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		return mapProxyError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// mapProxyError translates a service/repository error into the app's
// {"error": "..."} JSON envelope. Mirrors mapMonitorError/mapMaintenanceError.
func mapProxyError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody(errProxyNotFound))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		slog.Error("proxy handler error", "error", err)
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
