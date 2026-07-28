package handlers

import (
	"errors"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
	"gopkg.in/yaml.v3"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// ConfigHandlers serves declarative config-as-code endpoints (admin-only).
type ConfigHandlers struct {
	svc *services.ConfigService
}

// NewConfigHandlers binds handlers to a ConfigService.
func NewConfigHandlers(svc *services.ConfigService) *ConfigHandlers {
	return &ConfigHandlers{svc: svc}
}

// ConfigApplyRequest is the body of plan/apply.
type ConfigApplyRequest struct {
	Document *services.ConfigDocument `json:"document" yaml:"document"`
	Prune    bool                     `json:"prune" yaml:"prune"`
}

// Export handles GET /api/config/export — redacted YAML.
func (h *ConfigHandlers) Export(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	doc, err := h.svc.Export(c.Request().Context(), userID)
	if err != nil {
		return mapConfigError(c, err)
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("failed to encode YAML"))
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Content-Disposition", `attachment; filename="phoenix-config.yaml"`)
	return c.Blob(http.StatusOK, "application/yaml", raw)
}

// Validate handles POST /api/config/validate.
func (h *ConfigHandlers) Validate(c echo.Context) error {
	doc, err := decodeConfigDocument(c)
	if err != nil {
		return badRequest(c, err.Error())
	}
	errs := h.svc.Validate(c.Request().Context(), doc)
	return c.JSON(http.StatusOK, map[string]any{
		"valid":  len(errs) == 0,
		"errors": errs,
	})
}

// Plan handles POST /api/config/plan.
func (h *ConfigHandlers) Plan(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	req, err := decodeConfigApplyRequest(c)
	if err != nil {
		return badRequest(c, err.Error())
	}
	plan, err := h.svc.Plan(c.Request().Context(), userID, req.Document, services.ConfigApplyOptions{Prune: req.Prune})
	if err != nil {
		return mapConfigError(c, err)
	}
	return c.JSON(http.StatusOK, plan)
}

// Apply handles POST /api/config/apply.
func (h *ConfigHandlers) Apply(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	req, err := decodeConfigApplyRequest(c)
	if err != nil {
		return badRequest(c, err.Error())
	}
	res, err := h.svc.Apply(c.Request().Context(), userID, req.Document, services.ConfigApplyOptions{Prune: req.Prune})
	if err != nil {
		if res != nil && res.Plan != nil && !res.Plan.Valid {
			return c.JSON(http.StatusBadRequest, res)
		}
		return mapConfigError(c, err)
	}
	return c.JSON(http.StatusOK, res)
}

func decodeConfigDocument(c echo.Context) (*services.ConfigDocument, error) {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, errors.New("failed to read body")
	}
	doc := &services.ConfigDocument{}
	// yaml.v3 accepts JSON as well.
	if err := yaml.Unmarshal(body, doc); err != nil {
		return nil, errors.New("invalid document")
	}
	return doc, nil
}

func decodeConfigApplyRequest(c echo.Context) (*ConfigApplyRequest, error) {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, errors.New("failed to read body")
	}
	req := &ConfigApplyRequest{}
	if err := yaml.Unmarshal(body, req); err != nil {
		return nil, errors.New("invalid request body")
	}
	if req.Document == nil {
		// Bare document (no wrapper).
		doc := &services.ConfigDocument{}
		if err := yaml.Unmarshal(body, doc); err != nil {
			return nil, errors.New("document is required")
		}
		if doc.APIVersion != "" || doc.Kind != "" || hasConfigSpec(doc) {
			req.Document = doc
		}
	}
	if req.Document == nil {
		return nil, errors.New("document is required")
	}
	return req, nil
}

func hasConfigSpec(doc *services.ConfigDocument) bool {
	s := doc.Spec
	return len(s.Tags)+len(s.Proxies)+len(s.Notifications)+len(s.MonitorGroups)+
		len(s.Monitors)+len(s.StatusPages)+len(s.MaintenanceWindows) > 0
}

func mapConfigError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
