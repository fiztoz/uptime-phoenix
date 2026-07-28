package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// APIKeyHandlers groups API key management endpoints.
type APIKeyHandlers struct {
	authSvc *services.AuthService
}

// NewAPIKeyHandlers creates handlers bound to auth service.
func NewAPIKeyHandlers(authSvc *services.AuthService) *APIKeyHandlers {
	return &APIKeyHandlers{authSvc: authSvc}
}

// APIKeyView is the wire shape of domain.APIKey for REST/frontend consumers.
type APIKeyView struct {
	ID         int64    `json:"id"`
	UserID     int64    `json:"user_id"`
	Name       string   `json:"name"`
	Active     bool     `json:"active"`
	ExpiresAt  *string  `json:"expires_at"`
	Scopes     []string `json:"scopes"`
	LastUsedAt *string  `json:"last_used_at"`
	CreatedAt  string   `json:"created_at"`
}

// CreateAPIKeyResponse is returned by POST /api/api-keys (plaintext shown once).
type CreateAPIKeyResponse struct {
	Key    string     `json:"key"`
	APIKey APIKeyView `json:"api_key"`
}

func formatOptionalTime(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func toAPIKeyView(ak *domain.APIKey) APIKeyView {
	if ak == nil {
		return APIKeyView{}
	}
	scopes := ak.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	created := ak.CreatedAt.UTC().Format(time.RFC3339Nano)
	if ak.CreatedAt.IsZero() {
		created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return APIKeyView{
		ID:         ak.ID,
		UserID:     ak.UserID,
		Name:       ak.Name,
		Active:     ak.Active,
		ExpiresAt:  formatOptionalTime(ak.ExpiresAt),
		Scopes:     scopes,
		LastUsedAt: formatOptionalTime(ak.LastUsedAt),
		CreatedAt:  created,
	}
}

func toAPIKeyViews(keys []*domain.APIKey) []APIKeyView {
	out := make([]APIKeyView, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyView(k))
	}
	return out
}

// Create handles POST /api/api-keys — returns plaintext key ONCE.
func (h *APIKeyHandlers) Create(c echo.Context) error {
	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt *string  `json:"expires_at"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	userID, ok := userIDFromContext(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && strings.TrimSpace(*req.ExpiresAt) != "" {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*req.ExpiresAt))
		if err != nil {
			// Also accept RFC3339 without fractional seconds.
			t, err = time.Parse(time.RFC3339, strings.TrimSpace(*req.ExpiresAt))
			if err != nil {
				return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid expires_at (use RFC3339)"})
			}
		}
		t = t.UTC()
		expiresAt = &t
	}
	key, ak, err := h.authSvc.CreateAPIKey(c.Request().Context(), userID, req.Name, req.Scopes, expiresAt)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusCreated, CreateAPIKeyResponse{
		Key:    key,
		APIKey: toAPIKeyView(ak),
	})
}

// List handles GET /api/api-keys.
func (h *APIKeyHandlers) List(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
	keys, err := h.authSvc.ListAPIKeys(c.Request().Context(), userID)
	if err != nil {
		return mapAuthError(c, err)
	}
	return c.JSON(http.StatusOK, toAPIKeyViews(keys))
}

// Delete handles DELETE /api/api-keys/:id.
func (h *APIKeyHandlers) Delete(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid id"})
	}
	userID, ok := userIDFromContext(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
	if err := h.authSvc.RevokeAPIKey(c.Request().Context(), userID, id); err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "not found"})
		}
		return mapAuthError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
