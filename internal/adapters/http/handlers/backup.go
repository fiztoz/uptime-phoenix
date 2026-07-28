package handlers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// BackupHandlers serves configuration export/import endpoints.
type BackupHandlers struct {
	svc *services.BackupService
}

// NewBackupHandlers creates handlers bound to the supplied BackupService.
func NewBackupHandlers(svc *services.BackupService) *BackupHandlers {
	return &BackupHandlers{svc: svc}
}

// Export handles GET /api/backup/export.
//
// SECRETS POLICY: the response intentionally includes notification configs
// (tokens/webhooks) and proxy passwords so a backup is fully restorable.
// This is the one deliberate exception to "never return secrets" (see
// services.BackupDocument and AGENTS.md Wire-Shape Discipline). The endpoint
// is auth-gated and marked Cache-Control: no-store.
func (h *BackupHandlers) Export(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	doc, err := h.svc.Export(c.Request().Context(), userID)
	if err != nil {
		return mapBackupError(c, err)
	}

	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Content-Disposition", `attachment; filename="phoenix-backup.json"`)
	return c.JSON(http.StatusOK, doc)
}

// Import handles POST /api/backup/import.
// Creates new entities for the authenticated user (merge-only). Body is a
// BackupDocument JSON as produced by Export.
func (h *BackupHandlers) Import(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	var doc services.BackupDocument
	if err := c.Bind(&doc); err != nil {
		return badRequest(c, "invalid backup document")
	}

	summary, err := h.svc.Import(c.Request().Context(), userID, &doc)
	if err != nil {
		return mapBackupError(c, err)
	}
	return c.JSON(http.StatusOK, summary)
}

func mapBackupError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		slog.Error("backup handler error", "error", err)
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
