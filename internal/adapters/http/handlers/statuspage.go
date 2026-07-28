// Package handlers contains Echo HTTP handlers for the Phoenix REST API.
package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// StatusPageHandlers groups the status page CRUD endpoints, incident
// management, monitor assignment, and email-subscription endpoints.
type StatusPageHandlers struct {
	svc    *services.StatusPageService
	subSvc *services.SubscriptionService
}

// NewStatusPageHandlers creates handlers bound to the supplied service.
func NewStatusPageHandlers(svc *services.StatusPageService) *StatusPageHandlers {
	return &StatusPageHandlers{svc: svc}
}

// SetSubscriptionService attaches the email subscription use-case service.
// Without it, subscription endpoints return 501.
func (h *StatusPageHandlers) SetSubscriptionService(svc *services.SubscriptionService) {
	h.subSvc = svc
}

// CreateSPRequest is the body of POST /api/status-pages.
type CreateSPRequest struct {
	Slug                 string `json:"slug"`
	Title                string `json:"title"`
	Description          string `json:"description"`
	Icon                 string `json:"icon"`
	Favicon              string `json:"favicon"`
	Theme                string `json:"theme"`
	Published            *bool  `json:"published"`
	AccessCode           string `json:"access_code"`
	FooterText           string `json:"footer_text"`
	CustomCSS            string `json:"custom_css"`
	DashboardStyle       string `json:"dashboard_style"`
	ShowTags             *bool  `json:"show_tags"`
	AutoResolveIncidents *bool  `json:"auto_resolve_incidents"`
	// ShowPoweredBy defaults to true when omitted (white-label off).
	ShowPoweredBy *bool    `json:"show_powered_by"`
	SLATarget     *float64 `json:"sla_target"`
}

// UpdateSPRequest is the body of PUT /api/status-pages/:id.
type UpdateSPRequest struct {
	Slug                 string   `json:"slug"`
	Title                string   `json:"title"`
	Description          string   `json:"description"`
	Icon                 *string  `json:"icon"`
	Favicon              *string  `json:"favicon"`
	Theme                string   `json:"theme"`
	Published            *bool    `json:"published"`
	AccessCode           *string  `json:"access_code"`
	FooterText           string   `json:"footer_text"`
	CustomCSS            string   `json:"custom_css"`
	DashboardStyle       string   `json:"dashboard_style"`
	ShowTags             *bool    `json:"show_tags"`
	AutoResolveIncidents *bool    `json:"auto_resolve_incidents"`
	ShowPoweredBy        *bool    `json:"show_powered_by"`
	SLATarget            *float64 `json:"sla_target"`
}

// SPView is the wire shape of domain.StatusPage.
type SPView struct {
	ID                   int64    `json:"id"`
	Slug                 string   `json:"slug"`
	Title                string   `json:"title"`
	Description          string   `json:"description"`
	Icon                 string   `json:"icon"`
	Favicon              string   `json:"favicon"`
	Theme                string   `json:"theme"`
	Published            bool     `json:"published"`
	HasAccess            bool     `json:"has_access"`
	FooterText           string   `json:"footer_text"`
	CustomCSS            string   `json:"custom_css"`
	DashboardStyle       string   `json:"dashboard_style"`
	ShowTags             bool     `json:"show_tags"`
	AutoResolveIncidents bool     `json:"auto_resolve_incidents"`
	ShowPoweredBy        bool     `json:"show_powered_by"`
	SLATarget            *float64 `json:"sla_target"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
}

func toSPView(sp *domain.StatusPage) *SPView {
	if sp == nil {
		return nil
	}
	hasAccess := false
	if len(sp.PasswordHash) > 0 {
		hasAccess = true
	}
	return &SPView{
		ID:                   sp.ID,
		Slug:                 sp.Slug,
		Title:                sp.Title,
		Description:          sp.Description,
		Icon:                 sp.Icon,
		Favicon:              sp.Favicon,
		Theme:                sp.Theme,
		Published:            sp.Published,
		HasAccess:            hasAccess,
		FooterText:           sp.FooterText,
		CustomCSS:            sp.CustomCSS,
		DashboardStyle:       domain.NormalizeDashboardStyle(sp.DashboardStyle),
		ShowTags:             sp.ShowTags,
		AutoResolveIncidents: sp.AutoResolveIncidents,
		ShowPoweredBy:        sp.ShowPoweredBy,
		SLATarget:            sp.SLATarget,
		CreatedAt:            sp.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:            sp.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// IncidentUpdateView is the wire shape of an incident timeline update.
type IncidentUpdateView struct {
	ID           int64  `json:"id"`
	IncidentID   int64  `json:"incident_id"`
	StatusPageID int64  `json:"status_page_id"`
	Status       string `json:"status"`
	Content      string `json:"content"`
	CreatedAt    string `json:"created_at"`
}

func toIncidentUpdateView(update *domain.IncidentUpdate) *IncidentUpdateView {
	if update == nil {
		return nil
	}
	return &IncidentUpdateView{
		ID:           update.ID,
		IncidentID:   update.IncidentID,
		StatusPageID: update.StatusPageID,
		Status:       string(update.Status),
		Content:      update.Content,
		CreatedAt:    update.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// IncidentView is the wire shape of domain.Incident.
type IncidentView struct {
	ID           int64                 `json:"id"`
	StatusPageID int64                 `json:"status_page_id"`
	Title        string                `json:"title"`
	Content      string                `json:"content"`
	Style        string                `json:"style"`
	Pinned       bool                  `json:"pinned"`
	Active       bool                  `json:"active"`
	CreatedAt    string                `json:"created_at"`
	Updates      []*IncidentUpdateView `json:"updates"`
}

func toIncidentView(inc *domain.Incident) *IncidentView {
	if inc == nil {
		return nil
	}
	return &IncidentView{
		ID:           inc.ID,
		StatusPageID: inc.StatusPageID,
		Title:        inc.Title,
		Content:      inc.Content,
		Style:        inc.Style,
		Pinned:       inc.Pinned,
		Active:       inc.Active,
		CreatedAt:    inc.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Updates:      []*IncidentUpdateView{},
	}
}

func (h *StatusPageHandlers) incidentViewWithUpdates(c echo.Context, inc *domain.Incident) *IncidentView {
	view := toIncidentView(inc)
	if view == nil {
		return nil
	}
	updates, err := h.svc.ListIncidentUpdates(c.Request().Context(), inc.ID)
	if err != nil {
		return view
	}
	view.Updates = make([]*IncidentUpdateView, len(updates))
	for i, update := range updates {
		view.Updates[i] = toIncidentUpdateView(update)
	}
	return view
}

// CreateIncidentRequest is the body of POST /api/status-pages/:id/incidents.
type CreateIncidentRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Style   string `json:"style"`
	Pinned  *bool  `json:"pinned"`
	Active  *bool  `json:"active"`
}

// UpdateIncidentRequest is the body of PUT /api/status-pages/:spId/incidents/:incId.
type UpdateIncidentRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Style   string `json:"style"`
	Pinned  *bool  `json:"pinned"`
	Active  *bool  `json:"active"`
}

// CreateIncidentUpdateRequest is the body of POST /api/status-pages/:spId/incidents/:incId/updates.
type CreateIncidentUpdateRequest struct {
	Status  string `json:"status"`
	Content string `json:"content"`
}

// SPMonitorView is the wire shape of domain.StatusPageMonitor.
type SPMonitorView struct {
	ID           int64 `json:"id"`
	StatusPageID int64 `json:"status_page_id"`
	MonitorID    int64 `json:"monitor_id"`
	DisplayOrder int   `json:"display_order"`
}

// SPCNAMEView is the wire shape of domain.StatusPageCNAME.
type SPCNAMEView struct {
	ID           int64  `json:"id"`
	StatusPageID int64  `json:"status_page_id"`
	Domain       string `json:"domain"`
}

// Create handles POST /api/status-pages.
func (h *StatusPageHandlers) Create(c echo.Context) error {
	var req CreateSPRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Slug == "" {
		return badRequest(c, "slug is required")
	}
	if req.Title == "" {
		return badRequest(c, "title is required")
	}

	published := true
	if req.Published != nil {
		published = *req.Published
	}
	showTags := false
	if req.ShowTags != nil {
		showTags = *req.ShowTags
	}
	autoResolve := false
	if req.AutoResolveIncidents != nil {
		autoResolve = *req.AutoResolveIncidents
	}
	showPoweredBy := true
	if req.ShowPoweredBy != nil {
		showPoweredBy = *req.ShowPoweredBy
	}

	sp := &domain.StatusPage{
		Slug:                 req.Slug,
		Title:                req.Title,
		Description:          req.Description,
		Icon:                 req.Icon,
		Favicon:              req.Favicon,
		Theme:                req.Theme,
		Published:            published,
		FooterText:           req.FooterText,
		CustomCSS:            req.CustomCSS,
		DashboardStyle:       req.DashboardStyle,
		ShowTags:             showTags,
		AutoResolveIncidents: autoResolve,
		ShowPoweredBy:        showPoweredBy,
		SLATarget:            req.SLATarget,
	}

	// Use the service layer to hash and set the page access code.
	if req.AccessCode != "" {
		hash, err := h.svc.SetPassword(req.AccessCode)
		if err != nil {
			return mapSPError(c, err)
		}
		sp.PasswordHash = hash
	}

	if err := h.svc.Create(c.Request().Context(), sp); err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusCreated, toSPView(sp))
}

// List handles GET /api/status-pages.
func (h *StatusPageHandlers) List(c echo.Context) error {
	pages, err := h.svc.List(c.Request().Context())
	if err != nil {
		return mapSPError(c, err)
	}

	views := make([]*SPView, len(pages))
	for i, sp := range pages {
		views[i] = toSPView(sp)
	}
	return c.JSON(http.StatusOK, views)
}

// GetBySlug handles GET /api/status/pages/:slug (public).
func (h *StatusPageHandlers) GetBySlug(c echo.Context) error {
	slug := c.Param("slug")

	sp, err := h.svc.GetBySlug(c.Request().Context(), slug)
	if err != nil {
		return mapSPError(c, err)
	}
	if !sp.Published {
		return c.JSON(http.StatusNotFound, errorBody("status page not found"))
	}

	return c.JSON(http.StatusOK, toSPView(sp))
}

// publicStatusView is the wire shape of the public status page endpoint.
// It mirrors services.PublicStatusResponse but projects the status page
// through SPView instead of embedding the raw domain.StatusPage — the
// domain type carries no JSON tags and would otherwise serialize its
// PasswordHash (a bcrypt hash) and capitalized field names on a public,
// unauthenticated endpoint. SPView omits the hash and exposes has_access.
type publicStatusView struct {
	StatusPage             *SPView                         `json:"status_page"`
	Monitors               []*services.PublicMonitorStatus `json:"monitors"`
	Incidents              []*services.PublicIncidentView  `json:"incidents"`
	SubscriptionsAvailable bool                            `json:"subscriptions_available"`
}

func toPublicStatusView(resp *services.PublicStatusResponse) *publicStatusView {
	return &publicStatusView{
		StatusPage:             toSPView(resp.StatusPage),
		Monitors:               resp.Monitors,
		Incidents:              resp.Incidents,
		SubscriptionsAvailable: resp.SubscriptionsAvailable,
	}
}

// GetPublicStatus handles GET /api/status/:slug (public).
// Returns the full status page with monitors, their current status,
// and active incidents.
func (h *StatusPageHandlers) GetPublicStatus(c echo.Context) error {
	slug := c.Param("slug")

	resp, err := h.svc.GetPublicStatus(c.Request().Context(), slug)
	if err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusOK, toPublicStatusView(resp))
}

// GetByID handles GET /api/status-pages/:id (admin).
func (h *StatusPageHandlers) GetByID(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	sp, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusOK, toSPView(sp))
}

// Update handles PUT /api/status-pages/:id.
func (h *StatusPageHandlers) Update(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	existing, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapSPError(c, err)
	}

	var req UpdateSPRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	if req.Slug != "" {
		existing.Slug = req.Slug
	}
	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.Icon != nil {
		existing.Icon = *req.Icon
	}
	if req.Favicon != nil {
		existing.Favicon = *req.Favicon
	}
	if req.Theme != "" {
		existing.Theme = req.Theme
	}
	if req.Published != nil {
		existing.Published = *req.Published
	}
	if req.AccessCode != nil {
		hash, err := h.svc.SetPassword(*req.AccessCode)
		if err != nil {
			return mapSPError(c, err)
		}
		existing.PasswordHash = hash
	}
	if req.FooterText != "" {
		existing.FooterText = req.FooterText
	}
	if req.CustomCSS != "" {
		existing.CustomCSS = req.CustomCSS
	}
	if req.DashboardStyle != "" {
		existing.DashboardStyle = req.DashboardStyle
	}
	if req.ShowTags != nil {
		existing.ShowTags = *req.ShowTags
	}
	if req.AutoResolveIncidents != nil {
		existing.AutoResolveIncidents = *req.AutoResolveIncidents
	}
	if req.ShowPoweredBy != nil {
		existing.ShowPoweredBy = *req.ShowPoweredBy
	}
	if req.SLATarget != nil {
		existing.SLATarget = req.SLATarget
	}

	if err := h.svc.Update(c.Request().Context(), existing); err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusOK, toSPView(existing))
}

// Delete handles DELETE /api/status-pages/:id.
func (h *StatusPageHandlers) Delete(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		return mapSPError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// CreateIncident handles POST /api/status-pages/:spId/incidents.
func (h *StatusPageHandlers) CreateIncident(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	if _, err := h.svc.GetByID(c.Request().Context(), spID); err != nil {
		return mapSPError(c, err)
	}

	var req CreateIncidentRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Title == "" {
		return badRequest(c, "title is required")
	}

	pinned := true
	if req.Pinned != nil {
		pinned = *req.Pinned
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}

	inc := &domain.Incident{
		StatusPageID: spID,
		Title:        req.Title,
		Content:      req.Content,
		Style:        req.Style,
		Pinned:       pinned,
		Active:       active,
	}

	if err := h.svc.CreateIncident(c.Request().Context(), inc); err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusCreated, h.incidentViewWithUpdates(c, inc))
}

// ListAllIncidents handles GET /api/incidents — aggregates incidents across status pages.
func (h *StatusPageHandlers) ListAllIncidents(c echo.Context) error {
	incidents, err := h.svc.ListAllIncidents(c.Request().Context())
	if err != nil {
		return mapSPError(c, err)
	}
	views := make([]*IncidentView, len(incidents))
	for i, inc := range incidents {
		views[i] = h.incidentViewWithUpdates(c, inc)
	}
	return c.JSON(http.StatusOK, views)
}

// ListIncidents handles GET /api/status-pages/:spId/incidents.
func (h *StatusPageHandlers) ListIncidents(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	incidents, err := h.svc.ListIncidents(c.Request().Context(), spID)
	if err != nil {
		return mapSPError(c, err)
	}

	views := make([]*IncidentView, len(incidents))
	for i, inc := range incidents {
		views[i] = h.incidentViewWithUpdates(c, inc)
	}
	return c.JSON(http.StatusOK, views)
}

// UpdateIncident handles PUT /api/status-pages/:spId/incidents/:incId.
func (h *StatusPageHandlers) UpdateIncident(c echo.Context) error {
	incID, err := strconv.ParseInt(c.Param("incId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid incident id")
	}

	existing, err := h.svc.GetIncidentByID(c.Request().Context(), incID)
	if err != nil {
		return mapSPError(c, err)
	}

	var req UpdateIncidentRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.Content != "" {
		existing.Content = req.Content
	}
	if req.Style != "" {
		existing.Style = req.Style
	}
	if req.Pinned != nil {
		existing.Pinned = *req.Pinned
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}

	if err := h.svc.UpdateIncident(c.Request().Context(), existing); err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusOK, h.incidentViewWithUpdates(c, existing))
}

// ListIncidentUpdates handles GET /api/status-pages/:spId/incidents/:incId/updates.
func (h *StatusPageHandlers) ListIncidentUpdates(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}
	incID, err := strconv.ParseInt(c.Param("incId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid incident id")
	}
	inc, err := h.svc.GetIncidentByID(c.Request().Context(), incID)
	if err != nil {
		return mapSPError(c, err)
	}
	if inc.StatusPageID != spID {
		return c.JSON(http.StatusNotFound, errorBody("incident not found"))
	}
	updates, err := h.svc.ListIncidentUpdates(c.Request().Context(), incID)
	if err != nil {
		return mapSPError(c, err)
	}
	views := make([]*IncidentUpdateView, len(updates))
	for i, update := range updates {
		views[i] = toIncidentUpdateView(update)
	}
	return c.JSON(http.StatusOK, views)
}

// CreateIncidentUpdate handles POST /api/status-pages/:spId/incidents/:incId/updates.
func (h *StatusPageHandlers) CreateIncidentUpdate(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}
	incID, err := strconv.ParseInt(c.Param("incId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid incident id")
	}
	inc, err := h.svc.GetIncidentByID(c.Request().Context(), incID)
	if err != nil {
		return mapSPError(c, err)
	}
	if inc.StatusPageID != spID {
		return c.JSON(http.StatusNotFound, errorBody("incident not found"))
	}

	var req CreateIncidentUpdateRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Status) == "" {
		return badRequest(c, "status is required")
	}
	update, err := h.svc.CreateIncidentUpdate(
		c.Request().Context(),
		incID,
		domain.NormalizeIncidentStatus(req.Status),
		req.Content,
	)
	if err != nil {
		return mapSPError(c, err)
	}
	return c.JSON(http.StatusCreated, toIncidentUpdateView(update))
}

// DeleteIncident handles DELETE /api/status-pages/:spId/incidents/:incId.
func (h *StatusPageHandlers) DeleteIncident(c echo.Context) error {
	incID, err := strconv.ParseInt(c.Param("incId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid incident id")
	}

	if err := h.svc.DeleteIncident(c.Request().Context(), incID); err != nil {
		return mapSPError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// ResolveIncident handles POST /api/status-pages/:spId/incidents/:incId/resolve.
func (h *StatusPageHandlers) ResolveIncident(c echo.Context) error {
	incID, err := strconv.ParseInt(c.Param("incId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid incident id")
	}

	if err := h.svc.ResolveIncident(c.Request().Context(), incID); err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "resolved"})
}

// AddMonitor handles POST /api/status-pages/:spId/monitors.
func (h *StatusPageHandlers) AddMonitor(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	var req struct {
		MonitorID    int64 `json:"monitor_id"`
		DisplayOrder int   `json:"display_order"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.MonitorID == 0 {
		return badRequest(c, "monitor_id is required")
	}

	if err := h.svc.AddMonitor(c.Request().Context(), spID, req.MonitorID, req.DisplayOrder); err != nil {
		return mapSPError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// RemoveMonitor handles DELETE /api/status-pages/:spId/monitors/:monitorId.
func (h *StatusPageHandlers) RemoveMonitor(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}
	monitorID, err := strconv.ParseInt(c.Param("monitorId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}

	if err := h.svc.RemoveMonitor(c.Request().Context(), spID, monitorID); err != nil {
		return mapSPError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// ReorderMonitors handles PUT /api/status-pages/:spId/monitors. It replaces the
// full ordered monitor set in one transaction — monitor_ids[0] gets
// display_order 10, [1] gets 20, etc. Any monitor previously assigned but
// absent from the list is removed.
func (h *StatusPageHandlers) ReorderMonitors(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	var req struct {
		MonitorIDs []int64 `json:"monitor_ids"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	if err := h.svc.ReorderMonitors(c.Request().Context(), spID, req.MonitorIDs); err != nil {
		return mapSPError(c, err)
	}

	// Return the reordered list so the caller sees what was stored.
	monitors, err := h.svc.ListMonitors(c.Request().Context(), spID)
	if err != nil {
		return mapSPError(c, err)
	}
	views := make([]*SPMonitorView, len(monitors))
	for i, m := range monitors {
		views[i] = &SPMonitorView{
			ID:           m.ID,
			StatusPageID: m.StatusPageID,
			MonitorID:    m.MonitorID,
			DisplayOrder: m.DisplayOrder,
		}
	}
	return c.JSON(http.StatusOK, views)
}

// ListMonitors handles GET /api/status-pages/:spId/monitors.
func (h *StatusPageHandlers) ListMonitors(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	monitors, err := h.svc.ListMonitors(c.Request().Context(), spID)
	if err != nil {
		return mapSPError(c, err)
	}

	views := make([]*SPMonitorView, len(monitors))
	for i, m := range monitors {
		views[i] = &SPMonitorView{
			ID:           m.ID,
			StatusPageID: m.StatusPageID,
			MonitorID:    m.MonitorID,
			DisplayOrder: m.DisplayOrder,
		}
	}
	return c.JSON(http.StatusOK, views)
}

// AddCNAME handles POST /api/status-pages/:spId/cnames.
func (h *StatusPageHandlers) AddCNAME(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	var req struct {
		Domain string `json:"domain"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Domain == "" {
		return badRequest(c, "domain is required")
	}

	cname := &domain.StatusPageCNAME{
		StatusPageID: spID,
		Domain:       req.Domain,
	}

	if err := h.svc.AddCNAME(c.Request().Context(), cname); err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusCreated, &SPCNAMEView{
		ID:           cname.ID,
		StatusPageID: cname.StatusPageID,
		Domain:       cname.Domain,
	})
}

// RemoveCNAME handles DELETE /api/status-pages/:spId/cnames/:cnameId.
func (h *StatusPageHandlers) RemoveCNAME(c echo.Context) error {
	cnameID, err := strconv.ParseInt(c.Param("cnameId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid cname id")
	}

	if err := h.svc.RemoveCNAME(c.Request().Context(), cnameID); err != nil {
		return mapSPError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

// ListCNAMEs handles GET /api/status-pages/:spId/cnames.
func (h *StatusPageHandlers) ListCNAMEs(c echo.Context) error {
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}

	cnames, err := h.svc.ListCNAMEs(c.Request().Context(), spID)
	if err != nil {
		return mapSPError(c, err)
	}

	views := make([]*SPCNAMEView, len(cnames))
	for i, c := range cnames {
		views[i] = &SPCNAMEView{
			ID:           c.ID,
			StatusPageID: c.StatusPageID,
			Domain:       c.Domain,
		}
	}
	return c.JSON(http.StatusOK, views)
}

// ResolveDomain handles GET /api/status/resolve?domain=... (public).
func (h *StatusPageHandlers) ResolveDomain(c echo.Context) error {
	domainVal := c.QueryParam("domain")
	if domainVal == "" {
		return badRequest(c, "domain query parameter is required")
	}

	sp, err := h.svc.ResolveDomain(c.Request().Context(), domainVal)
	if err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusOK, toSPView(sp))
}

// VerifyAccess handles POST /api/status/:slug/verify-access (public), returning
// the complete public payload only after the access code has been verified.
func (h *StatusPageHandlers) VerifyAccess(c echo.Context) error {
	slug := c.Param("slug")
	if slug == "" {
		return badRequest(c, "slug is required")
	}

	var req struct {
		Entry string `json:"access_code"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	resp, err := h.svc.GetPublicStatusWithAccess(c.Request().Context(), slug, req.Entry)
	if err != nil {
		return mapSPError(c, err)
	}

	return c.JSON(http.StatusOK, toPublicStatusView(resp))
}

// ---------------------------------------------------------------------------
// Email subscriptions
// ---------------------------------------------------------------------------

// SubscribeRequest is the body of POST /api/status/:slug/subscribers.
type SubscribeRequest struct {
	Email      string `json:"email"`
	AccessCode string `json:"access_code"`
}

// TokenActionRequest is the body of confirm / unsubscribe endpoints.
type TokenActionRequest struct {
	Token string `json:"token"`
}

// SubscriberView is the admin-safe wire shape of a status-page email subscriber.
// Tokens and secrets never appear.
type SubscriberView struct {
	ID           int64  `json:"id"`
	StatusPageID int64  `json:"status_page_id"`
	Email        string `json:"email"`
	Active       bool   `json:"active"`
	ConfirmedAt  string `json:"confirmed_at,omitempty"`
	CreatedAt    string `json:"created_at"`
}

func toSubscriberView(sub *domain.StatusPageSubscriber) *SubscriberView {
	if sub == nil {
		return nil
	}
	v := &SubscriberView{
		ID:           sub.ID,
		StatusPageID: sub.StatusPageID,
		Email:        sub.Email,
		Active:       sub.Active,
		CreatedAt:    sub.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if sub.ConfirmedAt != nil && !sub.ConfirmedAt.IsZero() {
		v.ConfirmedAt = sub.ConfirmedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return v
}

// SubscriptionChannelView is the wire shape of the per-page SMTP channel.
type SubscriptionChannelView struct {
	NotificationID int64 `json:"notification_id"`
}

// SubscriptionChannelRequest is the body of PUT …/subscription-channel.
type SubscriptionChannelRequest struct {
	NotificationID int64 `json:"notification_id"`
}

// Subscribe handles POST /api/status/:slug/subscribers (public, rate-limited).
// Always returns 202 on the post-validation path to resist email enumeration.
func (h *StatusPageHandlers) Subscribe(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	slug := c.Param("slug")
	var req SubscribeRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if err := h.subSvc.Subscribe(c.Request().Context(), slug, req.Email, req.AccessCode); err != nil {
		return mapSubscriptionError(c, err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{
		"status": "accepted",
	})
}

// ConfirmSubscription handles POST /api/status/subscriptions/confirm.
func (h *StatusPageHandlers) ConfirmSubscription(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	var req TokenActionRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Token) == "" {
		return badRequest(c, "token is required")
	}
	if err := h.subSvc.Confirm(c.Request().Context(), req.Token); err != nil {
		return mapSubscriptionError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "confirmed"})
}

// Unsubscribe handles POST /api/status/subscriptions/unsubscribe.
func (h *StatusPageHandlers) Unsubscribe(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	var req TokenActionRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if strings.TrimSpace(req.Token) == "" {
		return badRequest(c, "token is required")
	}
	if err := h.subSvc.Unsubscribe(c.Request().Context(), req.Token); err != nil {
		return mapSubscriptionError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "unsubscribed"})
}

// ListSubscribers handles GET /api/status-pages/:spId/subscribers (admin).
func (h *StatusPageHandlers) ListSubscribers(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		// Also accept :id for consistency with other admin routes.
		spID, err = strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return badRequest(c, "invalid status page id")
		}
	}
	subs, err := h.subSvc.ListSubscribers(c.Request().Context(), spID)
	if err != nil {
		return mapSPError(c, err)
	}
	views := make([]*SubscriberView, len(subs))
	for i, sub := range subs {
		views[i] = toSubscriberView(sub)
	}
	return c.JSON(http.StatusOK, views)
}

// DeleteSubscriber handles DELETE /api/status-pages/:spId/subscribers/:subscriberId.
func (h *StatusPageHandlers) DeleteSubscriber(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	spID, err := strconv.ParseInt(c.Param("spId"), 10, 64)
	if err != nil {
		spID, err = strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil {
			return badRequest(c, "invalid status page id")
		}
	}
	subID, err := strconv.ParseInt(c.Param("subscriberId"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid subscriber id")
	}
	if err := h.subSvc.DeleteSubscriber(c.Request().Context(), spID, subID); err != nil {
		return mapSPError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// GetSubscriptionChannel handles GET /api/status-pages/:spId/subscription-channel.
func (h *StatusPageHandlers) GetSubscriptionChannel(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	spID, err := parseStatusPageID(c)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}
	ch, err := h.subSvc.GetChannel(c.Request().Context(), spID)
	if err != nil {
		return mapSPError(c, err)
	}
	if ch == nil {
		return c.JSON(http.StatusOK, nil)
	}
	return c.JSON(http.StatusOK, &SubscriptionChannelView{NotificationID: ch.NotificationID})
}

// SetSubscriptionChannel handles PUT /api/status-pages/:spId/subscription-channel.
func (h *StatusPageHandlers) SetSubscriptionChannel(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	spID, err := parseStatusPageID(c)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}
	var req SubscriptionChannelRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.NotificationID == 0 {
		return badRequest(c, "notification_id is required")
	}
	ch, err := h.subSvc.SetChannel(c.Request().Context(), spID, req.NotificationID)
	if err != nil {
		return mapSPError(c, err)
	}
	return c.JSON(http.StatusOK, &SubscriptionChannelView{NotificationID: ch.NotificationID})
}

// DeleteSubscriptionChannel handles DELETE /api/status-pages/:spId/subscription-channel.
func (h *StatusPageHandlers) DeleteSubscriptionChannel(c echo.Context) error {
	if h.subSvc == nil {
		return c.JSON(http.StatusNotImplemented, errorBody("subscriptions not configured"))
	}
	spID, err := parseStatusPageID(c)
	if err != nil {
		return badRequest(c, "invalid status page id")
	}
	if err := h.subSvc.DeleteChannel(c.Request().Context(), spID); err != nil {
		return mapSPError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func parseStatusPageID(c echo.Context) (int64, error) {
	if v := c.Param("spId"); v != "" {
		return strconv.ParseInt(v, 10, 64)
	}
	return strconv.ParseInt(c.Param("id"), 10, 64)
}

func mapSubscriptionError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, services.ErrSubscriptionsUnavailable):
		return c.JSON(http.StatusServiceUnavailable, errorBody("subscriptions unavailable"))
	case errors.Is(err, ports.ErrSubscriberToken):
		return c.JSON(http.StatusBadRequest, errorBody("invalid or expired token"))
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody("not found"))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	case errors.Is(err, domain.ErrUnauthorized):
		return c.JSON(http.StatusForbidden, errorBody("access denied"))
	default:
		return mapSPError(c, err)
	}
}

func mapSPError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody("not found"))
	case errors.Is(err, domain.ErrIncidentActive):
		return c.JSON(http.StatusConflict, errorBody("resolve the incident before deleting it"))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	case errors.Is(err, domain.ErrUnauthorized):
		return c.JSON(http.StatusForbidden, errorBody("access denied"))
	case errors.Is(err, ports.ErrConflict):
		// Both unique columns behind this mapper are user-chosen: status_pages.slug
		// and status_page_cnames.domain. Without this case a taken slug surfaced as
		// a 500 "internal error", telling the user the server broke when in fact
		// their input was simply already in use.
		return c.JSON(http.StatusConflict, errorBody("slug or custom domain already in use"))
	case errors.Is(err, ports.ErrMonitorAlreadyLinked):
		return c.JSON(http.StatusConflict, errorBody("monitor is already linked to this status page"))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
