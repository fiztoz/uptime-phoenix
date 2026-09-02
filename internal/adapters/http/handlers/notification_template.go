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

const errNotificationTemplateNotFound = "notification template not found"

// NotificationTemplateHandlers exposes install-wide reusable message layouts.
// Every endpoint requires can_manage_notifications; templates are part of the
// notification configuration surface rather than monitor-scoped read data.
type NotificationTemplateHandlers struct {
	svc    *services.NotificationTemplateService
	access *services.AccessService
}

// NewNotificationTemplateHandlers creates notification-template handlers.
func NewNotificationTemplateHandlers(svc *services.NotificationTemplateService, access *services.AccessService) *NotificationTemplateHandlers {
	return &NotificationTemplateHandlers{svc: svc, access: access}
}

// CreateNotificationTemplateRequest is the body of POST /api/notification-templates.
type CreateNotificationTemplateRequest struct {
	Name          string                     `json:"name"`
	Provider      string                     `json:"provider"`
	TitleTemplate string                     `json:"title_template"`
	BodyTemplate  string                     `json:"body_template"`
	DiscordConfig *DiscordTemplateConfigView `json:"discord_config"`
	SMTPConfig    *SMTPTemplateConfigView    `json:"smtp_config"`
}

// UpdateNotificationTemplateRequest is the body of PUT /api/notification-templates/:id.
type UpdateNotificationTemplateRequest struct {
	Name          string                     `json:"name"`
	TitleTemplate string                     `json:"title_template"`
	BodyTemplate  string                     `json:"body_template"`
	DiscordConfig *DiscordTemplateConfigView `json:"discord_config"`
	SMTPConfig    *SMTPTemplateConfigView    `json:"smtp_config"`
}

// DiscordStatusColorsView is the wire shape for status-specific embed colors.
type DiscordStatusColorsView struct {
	Up          string `json:"up"`
	Down        string `json:"down"`
	Pending     string `json:"pending"`
	Maintenance string `json:"maintenance"`
	Certificate string `json:"certificate"`
}

// DiscordEmbedFieldView is the wire shape for one custom embed field.
type DiscordEmbedFieldView struct {
	NameTemplate  string `json:"name_template"`
	ValueTemplate string `json:"value_template"`
	Inline        bool   `json:"inline"`
}

// DiscordButtonView is the wire shape for one Discord Link button.
type DiscordButtonView struct {
	LabelTemplate string `json:"label_template"`
	URLTemplate   string `json:"url_template"`
}

// DiscordTemplateConfigView is the explicit public shape for Discord embed settings.
type DiscordTemplateConfigView struct {
	TitleURLTemplate string                  `json:"title_url_template"`
	FooterTemplate   string                  `json:"footer_template"`
	ShowTimestamp    bool                    `json:"show_timestamp"`
	Colors           DiscordStatusColorsView `json:"colors"`
	Fields           []DiscordEmbedFieldView `json:"fields"`
	Buttons          []DiscordButtonView     `json:"buttons"`
}

// SMTPTemplateConfigView is the explicit public shape for SMTP email layout settings.
type SMTPTemplateConfigView struct {
	Format           string `json:"format"`
	HTMLBodyTemplate string `json:"html_body_template"`
}

// NotificationTemplateView is the public wire shape for a reusable message layout.
type NotificationTemplateView struct {
	ID            int64                      `json:"id"`
	UserID        int64                      `json:"user_id"`
	Name          string                     `json:"name"`
	Provider      string                     `json:"provider"`
	TitleTemplate string                     `json:"title_template"`
	BodyTemplate  string                     `json:"body_template"`
	DiscordConfig *DiscordTemplateConfigView `json:"discord_config,omitempty"`
	SMTPConfig    *SMTPTemplateConfigView    `json:"smtp_config,omitempty"`
	CreatedAt     string                     `json:"created_at"`
	UpdatedAt     string                     `json:"updated_at"`
}

func toNotificationTemplateView(template *domain.NotificationTemplate) *NotificationTemplateView {
	if template == nil {
		return nil
	}
	view := &NotificationTemplateView{
		ID:            template.ID,
		UserID:        template.UserID,
		Name:          template.Name,
		Provider:      template.Provider,
		TitleTemplate: template.TitleTemplate,
		BodyTemplate:  template.BodyTemplate,
		CreatedAt:     template.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:     template.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if template.Provider == "discord" {
		config, err := domain.ParseDiscordTemplateConfig(template.Config)
		if err != nil {
			config = domain.DefaultDiscordTemplateConfig()
		}
		view.DiscordConfig = toDiscordTemplateConfigView(config)
	}
	if template.Provider == "smtp" {
		config, err := domain.ParseSMTPTemplateConfig(template.Config)
		if err != nil {
			config = domain.DefaultSMTPTemplateConfig()
		}
		view.SMTPConfig = toSMTPTemplateConfigView(config)
	}
	return view
}

func toSMTPTemplateConfigView(config domain.SMTPTemplateConfig) *SMTPTemplateConfigView {
	return &SMTPTemplateConfigView{
		Format:           config.Format,
		HTMLBodyTemplate: config.HTMLBodyTemplate,
	}
}

func toDiscordTemplateConfigView(config domain.DiscordTemplateConfig) *DiscordTemplateConfigView {
	fields := make([]DiscordEmbedFieldView, len(config.Fields))
	for i, field := range config.Fields {
		fields[i] = DiscordEmbedFieldView{
			NameTemplate: field.NameTemplate, ValueTemplate: field.ValueTemplate, Inline: field.Inline,
		}
	}
	buttons := make([]DiscordButtonView, len(config.Buttons))
	for i, button := range config.Buttons {
		buttons[i] = DiscordButtonView{
			LabelTemplate: button.LabelTemplate, URLTemplate: button.URLTemplate,
		}
	}
	return &DiscordTemplateConfigView{
		TitleURLTemplate: config.TitleURLTemplate,
		FooterTemplate:   config.FooterTemplate,
		ShowTimestamp:    config.ShowTimestamp,
		Colors: DiscordStatusColorsView{
			Up: config.Colors.Up, Down: config.Colors.Down, Pending: config.Colors.Pending,
			Maintenance: config.Colors.Maintenance, Certificate: config.Colors.Certificate,
		},
		Fields:  fields,
		Buttons: buttons,
	}
}

func discordTemplateConfigFromView(view *DiscordTemplateConfigView) map[string]any {
	if view == nil {
		return nil
	}
	fields := make([]domain.DiscordEmbedFieldTemplate, len(view.Fields))
	for i, field := range view.Fields {
		fields[i] = domain.DiscordEmbedFieldTemplate{
			NameTemplate: field.NameTemplate, ValueTemplate: field.ValueTemplate, Inline: field.Inline,
		}
	}
	buttons := make([]domain.DiscordButtonTemplate, len(view.Buttons))
	for i, button := range view.Buttons {
		buttons[i] = domain.DiscordButtonTemplate{
			LabelTemplate: button.LabelTemplate, URLTemplate: button.URLTemplate,
		}
	}
	return domain.DiscordTemplateConfigMap(domain.DiscordTemplateConfig{
		TitleURLTemplate: view.TitleURLTemplate,
		FooterTemplate:   view.FooterTemplate,
		ShowTimestamp:    view.ShowTimestamp,
		Colors: domain.DiscordStatusColors{
			Up: view.Colors.Up, Down: view.Colors.Down, Pending: view.Colors.Pending,
			Maintenance: view.Colors.Maintenance, Certificate: view.Colors.Certificate,
		},
		Fields:  fields,
		Buttons: buttons,
	})
}

func smtpTemplateConfigFromView(view *SMTPTemplateConfigView) map[string]any {
	if view == nil {
		return nil
	}
	return domain.SMTPTemplateConfigMap(domain.SMTPTemplateConfig{
		Format:           view.Format,
		HTMLBodyTemplate: view.HTMLBodyTemplate,
	})
}

func (h *NotificationTemplateHandlers) requireManage(c echo.Context) (int64, error) {
	userID, ok := userIDFromContext(c)
	if !ok {
		_ = unauthenticated(c)
		return 0, errAccessDenied
	}
	if h.access == nil {
		_ = c.JSON(http.StatusForbidden, errorBody("insufficient permissions"))
		return 0, errAccessDenied
	}
	allowed, err := h.access.CanManageNotifications(c.Request().Context(), userID)
	if err != nil || !allowed {
		_ = c.JSON(http.StatusForbidden, errorBody("insufficient permissions"))
		return 0, errAccessDenied
	}
	return userID, nil
}

// Create handles POST /api/notification-templates.
func (h *NotificationTemplateHandlers) Create(c echo.Context) error {
	userID, err := h.requireManage(c)
	if err != nil {
		return err
	}
	var req CreateNotificationTemplateRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.DiscordConfig != nil && req.Provider != "discord" {
		return badRequest(c, "discord_config is only valid for Discord templates")
	}
	if req.SMTPConfig != nil && req.Provider != "smtp" {
		return badRequest(c, "smtp_config is only valid for SMTP templates")
	}
	var config map[string]any
	switch req.Provider {
	case "discord":
		config = discordTemplateConfigFromView(req.DiscordConfig)
	case "smtp":
		config = smtpTemplateConfigFromView(req.SMTPConfig)
	}
	template := &domain.NotificationTemplate{
		UserID:        userID,
		Name:          req.Name,
		Provider:      req.Provider,
		TitleTemplate: req.TitleTemplate,
		BodyTemplate:  req.BodyTemplate,
		Config:        config,
	}
	if err := h.svc.Create(c.Request().Context(), template); err != nil {
		return mapNotificationTemplateError(c, err)
	}
	return c.JSON(http.StatusCreated, toNotificationTemplateView(template))
}

// List handles GET /api/notification-templates.
func (h *NotificationTemplateHandlers) List(c echo.Context) error {
	if _, err := h.requireManage(c); err != nil {
		return err
	}
	templates, err := h.svc.List(c.Request().Context())
	if err != nil {
		return mapNotificationTemplateError(c, err)
	}
	views := make([]*NotificationTemplateView, len(templates))
	for i, template := range templates {
		views[i] = toNotificationTemplateView(template)
	}
	return c.JSON(http.StatusOK, views)
}

// Variables handles GET /api/notification-templates/variables.
func (h *NotificationTemplateHandlers) Variables(c echo.Context) error {
	if _, err := h.requireManage(c); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string][]string{
		"variables": domain.NotificationTemplateVariables(),
	})
}

// GetByID handles GET /api/notification-templates/:id.
func (h *NotificationTemplateHandlers) GetByID(c echo.Context) error {
	if _, err := h.requireManage(c); err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification template id")
	}
	template, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapNotificationTemplateError(c, err)
	}
	return c.JSON(http.StatusOK, toNotificationTemplateView(template))
}

// Update handles PUT /api/notification-templates/:id. Provider is immutable so
// existing notification assignments cannot become incompatible after an edit.
func (h *NotificationTemplateHandlers) Update(c echo.Context) error {
	if _, err := h.requireManage(c); err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification template id")
	}
	template, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapNotificationTemplateError(c, err)
	}
	var req UpdateNotificationTemplateRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	template.Name = req.Name
	template.TitleTemplate = req.TitleTemplate
	template.BodyTemplate = req.BodyTemplate
	if req.DiscordConfig != nil && template.Provider != "discord" {
		return badRequest(c, "discord_config is only valid for Discord templates")
	}
	if req.SMTPConfig != nil && template.Provider != "smtp" {
		return badRequest(c, "smtp_config is only valid for SMTP templates")
	}
	if req.DiscordConfig != nil {
		template.Config = discordTemplateConfigFromView(req.DiscordConfig)
	}
	if req.SMTPConfig != nil {
		template.Config = smtpTemplateConfigFromView(req.SMTPConfig)
	}
	if err := h.svc.Update(c.Request().Context(), template); err != nil {
		return mapNotificationTemplateError(c, err)
	}
	return c.JSON(http.StatusOK, toNotificationTemplateView(template))
}

// Delete handles DELETE /api/notification-templates/:id.
func (h *NotificationTemplateHandlers) Delete(c echo.Context) error {
	if _, err := h.requireManage(c); err != nil {
		return err
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid notification template id")
	}
	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		return mapNotificationTemplateError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func mapNotificationTemplateError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody(errNotificationTemplateNotFound))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
