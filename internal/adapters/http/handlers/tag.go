package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// errTagNotFound is the client-facing message used whenever a tag is missing.
const errTagNotFound = "tag not found"

// TagHandlers groups the tag CRUD and monitor-assignment endpoints.
//
// RBAC: reading tags is open to any authenticated user — tag names and colors are
// display metadata, and the dashboard's tag filter needs the list. WRITING is
// admin-only, in both senses:
//
//   - tag CRUD, because tags are install-wide: a non-admin deleting a tag would
//     strip it from every admin-owned monitor carrying it;
//   - assigning/removing a tag on a monitor, because that mutates the monitor, and
//     non-admins are read-only on monitors.
//
// The admin gate is applied by middleware.RequireAdmin in the router; the
// per-monitor view check still runs on the assignment routes so a tag cannot be
// pinned to a monitor that does not exist.
type TagHandlers struct {
	svc    *services.TagService
	access *services.AccessService
}

// NewTagHandlers creates handlers bound to the supplied services.
func NewTagHandlers(svc *services.TagService, access *services.AccessService) *TagHandlers {
	return &TagHandlers{svc: svc, access: access}
}

// TagView is the wire shape of domain.Tag.
//
// domain.Tag has no json tags, so returning it directly emitted capitalized Go
// field names (ID, Name, Color) while the frontend's Tag type — and every other
// endpoint — uses snake_case. See the Wire-Shape Discipline rule in AGENTS.md:
// never serialize a domain type.
type TagView struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

func toTagView(t *domain.Tag) *TagView {
	if t == nil {
		return nil
	}
	return &TagView{ID: t.ID, Name: t.Name, Color: t.Color}
}

func toTagViews(tags []*domain.Tag) []*TagView {
	out := make([]*TagView, 0, len(tags))
	for _, t := range tags {
		out = append(out, toTagView(t))
	}
	return out
}

// mapTagError translates service errors into the app's JSON error envelope.
// Previously these handlers returned the raw error, which produced Echo's
// default {"message": "Internal Server Error"} instead of {"error": "..."}.
func mapTagError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody(errTagNotFound))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	case errors.Is(err, ports.ErrConflict):
		return c.JSON(http.StatusConflict, errorBody("tag already exists"))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}

// Create handles POST /api/tags
func (h *TagHandlers) Create(c echo.Context) error {
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	tag := &domain.Tag{Name: req.Name, Color: req.Color}
	if err := h.svc.Create(c.Request().Context(), tag); err != nil {
		return mapTagError(c, err)
	}
	return c.JSON(http.StatusCreated, toTagView(tag))
}

// List handles GET /api/tags
func (h *TagHandlers) List(c echo.Context) error {
	tags, err := h.svc.List(c.Request().Context())
	if err != nil {
		return mapTagError(c, err)
	}
	return c.JSON(http.StatusOK, toTagViews(tags))
}

// Update handles PUT /api/tags/:id
func (h *TagHandlers) Update(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid id"})
	}
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	tag := &domain.Tag{ID: id, Name: req.Name, Color: req.Color}
	if err := h.svc.Update(c.Request().Context(), tag); err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorBody(errTagNotFound))
		}
		return mapTagError(c, err)
	}
	return c.JSON(http.StatusOK, toTagView(tag))
}

// Delete handles DELETE /api/tags/:id
func (h *TagHandlers) Delete(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid id"})
	}
	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorBody(errTagNotFound))
		}
		return mapTagError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// AssignToMonitor handles POST /api/monitors/:id/tags
func (h *TagHandlers) AssignToMonitor(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid monitor id"})
	}
	// requireMonitorOwnership has already written the response; propagate the
	// sentinel verbatim so we do not attempt a second write.
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}
	var req struct {
		TagID int64  `json:"tag_id"`
		Value string `json:"value"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if err := h.svc.AssignTagToMonitor(c.Request().Context(), monitorID, req.TagID, req.Value); err != nil {
		return mapTagError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// RemoveFromMonitor handles DELETE /api/monitors/:id/tags/:tag_id
func (h *TagHandlers) RemoveFromMonitor(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid monitor id"})
	}
	// requireMonitorOwnership has already written the response; propagate the
	// sentinel verbatim so we do not attempt a second write.
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}
	tagID, err := strconv.ParseInt(c.Param("tag_id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid tag id"})
	}
	if err := h.svc.RemoveTagFromMonitor(c.Request().Context(), monitorID, tagID); err != nil {
		return mapTagError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// ListForMonitor handles GET /api/monitors/:id/tags
func (h *TagHandlers) ListForMonitor(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid monitor id"})
	}
	// requireMonitorOwnership has already written the response; propagate the
	// sentinel verbatim so we do not attempt a second write.
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}
	tags, err := h.svc.ListTagsForMonitor(c.Request().Context(), monitorID)
	if err != nil {
		return mapTagError(c, err)
	}
	return c.JSON(http.StatusOK, toTagViews(tags))
}
