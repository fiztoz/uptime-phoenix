// Package handlers contains Echo HTTP handlers for the Phoenix REST API.
package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// MonitorHandlers groups the monitor CRUD endpoints behind a single receiver.
//
// access answers both authorization questions these handlers ask, and they are
// different questions:
//
//	"which monitors may this caller SEE?"    — every read path; scoping, not rejection.
//	"may this caller CHANGE this monitor?"   — requireMonitorEditAccess; admin or creator.
//
// Creating is gated in the router by the create_monitors capability. Editing an
// existing monitor is gated HERE, not in the router, because it depends on who
// owns the target.
//
// tags is used to embed each monitor's tags in the wire shape. It is optional:
// when nil, monitors serialize with an empty tags array rather than failing.
type MonitorHandlers struct {
	svc    *services.MonitorService
	access *services.AccessService
	tags   *services.TagService
	// groups is optional; when set, MonitorView.EffectiveOwner can resolve
	// inherited group contacts. Without it inherit falls back to the monitor's
	// own Owner field.
	groups ports.MonitorGroupRepository
}

// NewMonitorHandlers creates a MonitorHandlers bound to the supplied services.
// groups may be nil (effective owner then equals stored owner).
func NewMonitorHandlers(svc *services.MonitorService, access *services.AccessService, tags *services.TagService, groups ports.MonitorGroupRepository) *MonitorHandlers {
	return &MonitorHandlers{svc: svc, access: access, tags: tags, groups: groups}
}

// --- Request / response DTOs ---------------------------------------------

// CreateMonitorRequest is the body of POST /api/monitors.
type CreateMonitorRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Owner is informational contact text for the team responsible for this
	// monitor. It is independent of UserID, which records the Phoenix account
	// that created and may edit the monitor. When inherit_group_owner is true,
	// display prefers the group chain contact (see EffectiveOwner).
	Owner string `json:"owner"`
	// InheritGroupOwner prefers the monitor's group (and ancestors) contact.
	InheritGroupOwner   bool           `json:"inherit_group_owner"`
	Type                string         `json:"type"`
	Active              *bool          `json:"active"`
	Interval            int            `json:"interval"`
	RetryInterval       int            `json:"retry_interval"`
	MaxRetries          int            `json:"max_retries"`
	Timeout             float64        `json:"timeout"`
	Config              map[string]any `json:"config"`
	AcceptedStatusCodes []string       `json:"accepted_statuscodes"`
	UpsideDown          bool           `json:"upside_down"`
	// TLSIgnore skips TLS certificate verification on https checks
	// (self-signed / internal CAs). Honored by the http checker.
	TLSIgnore bool `json:"tls_ignore"`
	// CertExpiryNotify opts into fixed 30/14/7 day certificate-expiry alerts.
	// Only meaningful for HTTP(S) monitors; defaults false.
	CertExpiryNotify bool `json:"cert_expiry_notify"`
	ResendInterval   int  `json:"resend_interval"`
	// GroupID files this monitor under a MonitorGroup (folder). nil/omitted
	// means top-level (not in any group). Replaces the old ParentID, which
	// nested a monitor under another *monitor*.
	GroupID *int64 `json:"group_id"`
	// ProxyID routes this monitor's checks through an outbound proxy owned
	// by the same user (see internal/adapters/http/handlers/proxy.go).
	// nil/omitted means no proxy.
	ProxyID *int64 `json:"proxy_id"`
	// Weight is the manual display order (lower first). Omitted/0 on create
	// becomes 2000 (schema default). Same meaning as MonitorGroup.Weight.
	Weight int `json:"weight"`
}

// UpdateMonitorRequest is the body of PUT /api/monitors/:id.
//
// Omitted fields are left unchanged. That is load-bearing: pause/resume send
// only {active: false|true}, and treating a missing group_id as null used to
// yank the monitor out of its folder. Send JSON null (or "") to clear a
// nullable/string field; send an explicit value to change it.
type UpdateMonitorRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Owner: omitted leaves the contact; "" or a string sets it ("" clears).
	Owner *string `json:"owner"`
	// InheritGroupOwner: omitted leaves the flag; true/false sets it.
	InheritGroupOwner   *bool          `json:"inherit_group_owner"`
	Active              *bool          `json:"active"`
	Interval            int            `json:"interval"`
	RetryInterval       *int           `json:"retry_interval"`
	MaxRetries          *int           `json:"max_retries"`
	Timeout             float64        `json:"timeout"`
	Config              map[string]any `json:"config"`
	AcceptedStatusCodes []string       `json:"accepted_statuscodes"`
	UpsideDown          *bool          `json:"upside_down"`
	TLSIgnore           *bool          `json:"tls_ignore"`
	CertExpiryNotify    *bool          `json:"cert_expiry_notify"`
	ResendInterval      *int           `json:"resend_interval"`
	// GroupID: omitted leaves the folder; null clears it (top-level);
	// a number files the monitor under that group.
	GroupID optionalNullableInt64 `json:"group_id"`
	// ProxyID: omitted leaves the proxy; null clears it; a number assigns it.
	ProxyID optionalNullableInt64 `json:"proxy_id"`
	// Weight: omitted leaves the order; 0 is a real value (top of the list).
	Weight *int `json:"weight"`
}

// MonitorTagView is one tag as it appears on a monitor: the tag definition
// joined with this monitor's value for it. `id` is the TAG's id (not the
// assignment row's) — that is what a tag filter matches on.
type MonitorTagView struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Value string `json:"value"`
}

// MonitorView is the wire shape of domain.Monitor.
//
// Tags is ALWAYS a non-nil slice, so the field serializes as [] and never null:
// the dashboard's tag filter iterates it unconditionally.
type MonitorView struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Owner is the monitor's OWN stored contact (may be empty when inheriting).
	Owner string `json:"owner"`
	// InheritGroupOwner means EffectiveOwner should prefer the group chain.
	InheritGroupOwner bool `json:"inherit_group_owner"`
	// EffectiveOwner is the contact to show/use (resolved; not stored).
	EffectiveOwner      string         `json:"effective_owner"`
	Type                string         `json:"type"`
	Active              bool           `json:"active"`
	Interval            int            `json:"interval"`
	RetryInterval       int            `json:"retry_interval"`
	MaxRetries          int            `json:"max_retries"`
	Timeout             float64        `json:"timeout"`
	Config              map[string]any `json:"config"`
	AcceptedStatusCodes []string       `json:"accepted_statuscodes"`
	UpsideDown          bool           `json:"upside_down"`
	TLSIgnore           bool           `json:"tls_ignore"`
	CertExpiryNotify    bool           `json:"cert_expiry_notify"`
	ResendInterval      int            `json:"resend_interval"`
	GroupID             *int64         `json:"group_id"`
	ProxyID             *int64         `json:"proxy_id"`
	// Weight is the manual display order (lower first). Lists order by
	// weight, then name, then id.
	Weight    int              `json:"weight"`
	Tags      []MonitorTagView `json:"tags"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
}

// toMonitorView projects a domain.Monitor to the public DTO. tags may be nil —
// it is normalized to an empty slice so the JSON is [] rather than null.
// groupsByID resolves EffectiveOwner when inherit_group_owner is set.
func toMonitorView(m *domain.Monitor, tags []services.MonitorTagDetail, groupsByID map[int64]*domain.MonitorGroup) *MonitorView {
	if m == nil {
		return nil
	}
	return &MonitorView{
		ID:                  m.ID,
		UserID:              m.UserID,
		Name:                m.Name,
		Description:         m.Description,
		Owner:               m.Owner,
		InheritGroupOwner:   m.InheritGroupOwner,
		EffectiveOwner:      m.EffectiveOwner(groupsByID),
		Type:                m.Type,
		Active:              m.Active,
		Interval:            m.Interval,
		RetryInterval:       m.RetryInterval,
		MaxRetries:          m.MaxRetries,
		Timeout:             m.Timeout,
		Config:              m.Config,
		AcceptedStatusCodes: m.AcceptedStatusCodes,
		UpsideDown:          m.UpsideDown,
		TLSIgnore:           m.TLSIgnore,
		CertExpiryNotify:    m.CertExpiryNotify,
		ResendInterval:      m.ResendInterval,
		GroupID:             m.GroupID,
		ProxyID:             m.ProxyID,
		Weight:              m.Weight,
		Tags:                toMonitorTagViews(tags),
		CreatedAt:           m.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:           m.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func toMonitorTagViews(tags []services.MonitorTagDetail) []MonitorTagView {
	out := make([]MonitorTagView, 0, len(tags))
	for _, t := range tags {
		out = append(out, MonitorTagView{ID: t.TagID, Name: t.Name, Color: t.Color, Value: t.Value})
	}
	return out
}

// monitorTags fetches one monitor's tags for the wire shape. A tag-lookup failure
// degrades to "no tags" rather than failing the whole request: tags are display
// metadata, and blanking a monitor because its labels could not be read would be
// a worse outcome than showing it unlabeled.
func (h *MonitorHandlers) monitorTags(c echo.Context, monitorID int64) []services.MonitorTagDetail {
	if h.tags == nil {
		return nil
	}
	tags, err := h.tags.TagsForMonitor(c.Request().Context(), monitorID)
	if err != nil {
		slog.Warn("monitor: tag lookup failed", "monitor_id", monitorID, "error", err)
		return nil
	}
	return tags
}

// validateMonitorConfig looks up the checker for the given type and calls
// Validate on the config. Returns the first validation error, if any.
func validateMonitorConfig(monitorType string, config map[string]any) error {
	if config == nil {
		config = make(map[string]any)
	}
	chk, ok := checker.Get(monitorType)
	if !ok {
		return domain.ErrValidation // unknown type validation handled elsewhere
	}
	return chk.Validate(config)
}

// --- Handlers -----------------------------------------------------------

// grantCreatorAccess gives the creator a view grant on the monitor they just
// made, so it appears in their list and in the admin permission editor.
//
// Only meaningful for a non-admin: an admin already sees every monitor, and
// AccessService.VisibleMonitorIDs short-circuits before grants are consulted, so
// the row changes nothing for them today. It is still written for everyone, and
// that is deliberate — it is the record of "this user was given sight of this",
// and it is what makes a LATER demotion from admin behave sanely instead of
// blinding someone to their own monitors.
//
// Failure is logged, never returned. The monitor exists by the time this runs;
// turning a grant write into a 500 would tell the caller their monitor was not
// created when it was, and there is no rollback here to make that true. The
// degradation is visible and repairable (an admin re-grants in the UI), which is
// the right trade against a lie. It cannot silently under-permit an admin, who
// needs no grant.
func (h *MonitorHandlers) grantCreatorAccess(c echo.Context, userID, monitorID int64) {
	if h.access == nil {
		return
	}
	if err := h.access.GrantMonitor(c.Request().Context(), userID, monitorID); err != nil {
		slog.ErrorContext(c.Request().Context(), "auto-grant creator view access to new monitor failed",
			"user_id", userID, "monitor_id", monitorID, "error", err)
	}
}

// Create handles POST /api/monitors. Requires the create_monitors capability
// (router-gated); admins always hold it.
//
// The creator becomes the monitor's owner via Monitor.UserID — that is what
// later lets them edit and delete it — and is auto-granted a view of it.
func (h *MonitorHandlers) Create(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	var req CreateMonitorRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Name == "" {
		return badRequest(c, "name is required")
	}
	if req.Type == "" {
		return badRequest(c, "type is required")
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}
	allowed, err := h.access.CanCreateMonitorInGroup(c.Request().Context(), userID, req.GroupID)
	if err != nil {
		return mapMonitorError(c, err)
	}
	if !allowed {
		return c.JSON(http.StatusForbidden, errorBody("monitor creation is limited to an allowed group"))
	}

	if req.Config == nil {
		req.Config = make(map[string]any)
	}

	// Validate the monitor config against the checker.
	if err := validateMonitorConfig(req.Type, req.Config); err != nil {
		return badRequest(c, "invalid config: "+err.Error())
	}

	if req.Interval <= 0 {
		req.Interval = 60
	}
	if req.Timeout <= 0 {
		req.Timeout = 30
	}

	active := true
	if req.Active != nil {
		active = *req.Active
	}

	acceptedCodes := req.AcceptedStatusCodes
	if len(acceptedCodes) == 0 {
		acceptedCodes = []string{"200-299"}
	}

	monitor := &domain.Monitor{
		UserID:              userID,
		Name:                req.Name,
		Description:         req.Description,
		Owner:               req.Owner,
		InheritGroupOwner:   req.InheritGroupOwner,
		Type:                req.Type,
		Active:              active,
		Interval:            req.Interval,
		RetryInterval:       req.RetryInterval,
		MaxRetries:          req.MaxRetries,
		Timeout:             req.Timeout,
		Config:              req.Config,
		AcceptedStatusCodes: acceptedCodes,
		UpsideDown:          req.UpsideDown,
		TLSIgnore:           req.TLSIgnore,
		CertExpiryNotify:    req.CertExpiryNotify,
		ResendInterval:      req.ResendInterval,
		GroupID:             req.GroupID,
		ProxyID:             req.ProxyID,
		Weight:              req.Weight,
	}
	if req.Type == "push" {
		if token, ok := req.Config["push_token"].(string); ok && token != "" {
			monitor.PushToken = token
		}
	}

	if err := h.svc.Create(c.Request().Context(), monitor); err != nil {
		return mapMonitorError(c, err)
	}
	h.grantCreatorAccess(c, userID, monitor.ID)

	// A monitor has no tags the instant it is created, so skip the lookup.
	return c.JSON(http.StatusCreated, toMonitorView(monitor, nil, h.groupsByID(c)))
}

// List handles GET /api/monitors.
//
// RBAC: an admin lists every monitor in the install; a non-admin lists exactly
// the monitors granted to them. The restriction is applied through the repository
// filter, NOT by post-filtering the result, so limit/offset stay meaningful.
//
// Note the deliberate absence of a UserID filter: ownership is no longer the
// visibility rule. An admin must see monitors owned by other users, and a
// non-admin must see monitors owned by the admin who granted them.
func (h *MonitorHandlers) List(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	if h.access == nil {
		return c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}

	all, visibleIDs, err := h.access.VisibleMonitorIDs(c.Request().Context(), userID)
	if err != nil {
		return mapMonitorError(c, err)
	}

	filter := ports.MonitorFilter{
		Search: c.QueryParam("search"),
		Type:   c.QueryParam("type"),
	}
	if !all {
		// RestrictToIDs, not len(visibleIDs) > 0: a user with zero grants must get
		// zero monitors, not every monitor. See ports.MonitorFilter.
		filter.RestrictToIDs = true
		filter.MonitorIDs = visibleIDs
	}

	if activeParam := c.QueryParam("active"); activeParam != "" {
		active := activeParam == "true" || activeParam == "1"
		filter.Active = &active
	}

	monitors, err := h.svc.List(c.Request().Context(), filter)
	if err != nil {
		return mapMonitorError(c, err)
	}

	// One batched tag lookup for the whole page instead of one per monitor.
	tagsByMonitor := map[int64][]services.MonitorTagDetail{}
	if h.tags != nil && len(monitors) > 0 {
		ids := make([]int64, len(monitors))
		for i, m := range monitors {
			ids[i] = m.ID
		}
		if fetched, tagErr := h.tags.TagsForMonitors(c.Request().Context(), ids); tagErr != nil {
			// Degrade to unlabeled monitors rather than failing the list — see
			// monitorTags for the rationale.
			slog.Warn("monitor list: batch tag lookup failed", "error", tagErr)
		} else {
			tagsByMonitor = fetched
		}
	}

	groupsByID := h.groupsByID(c)
	views := make([]*MonitorView, len(monitors))
	for i, m := range monitors {
		views[i] = toMonitorView(m, tagsByMonitor[m.ID], groupsByID)
	}
	return c.JSON(http.StatusOK, views)
}

// GetByID handles GET /api/monitors/:id.
func (h *MonitorHandlers) GetByID(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}

	// 404-not-403 for a monitor the caller cannot see: never confirm it exists.
	if err := requireMonitorViewAccess(c, h.access, id); err != nil {
		return err
	}

	monitor, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapMonitorError(c, err)
	}

	return c.JSON(http.StatusOK, toMonitorView(monitor, h.monitorTags(c, id), h.groupsByID(c)))
}

// Update handles PUT /api/monitors/:id. Admins, or the user who created this
// monitor. Being granted a view of a monitor confers nothing here.
//
// There is no router middleware on this route: whether the caller may edit
// depends on who owns THIS monitor, which only the handler knows. The check
// below is the gate, not a backstop behind one.
func (h *MonitorHandlers) Update(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if err := requireMonitorEditAccess(c, h.access, id); err != nil {
		return err
	}

	existing, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapMonitorError(c, err)
	}

	var req UpdateMonitorRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	// Validate the config if a new one is provided.
	if req.Config != nil {
		if err := validateMonitorConfig(existing.Type, req.Config); err != nil {
			return badRequest(c, "invalid config: "+err.Error())
		}
	}

	// Apply only the fields the client sent. Omitted keys keep the stored
	// value — pause/resume is {active: …} and must not reset placement,
	// flags, retry policy, owner, proxy, or weight.
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.Owner != nil {
		existing.Owner = *req.Owner
	}
	if req.InheritGroupOwner != nil {
		existing.InheritGroupOwner = *req.InheritGroupOwner
	}
	if req.Active != nil {
		existing.Active = *req.Active
	}
	if req.Interval > 0 {
		existing.Interval = req.Interval
	}
	if req.RetryInterval != nil && *req.RetryInterval >= 0 {
		existing.RetryInterval = *req.RetryInterval
	}
	if req.MaxRetries != nil && *req.MaxRetries >= 0 {
		existing.MaxRetries = *req.MaxRetries
	}
	if req.Timeout > 0 {
		existing.Timeout = req.Timeout
	}
	if req.Config != nil {
		existing.Config = req.Config
	}
	if req.AcceptedStatusCodes != nil {
		existing.AcceptedStatusCodes = req.AcceptedStatusCodes
	}
	if req.UpsideDown != nil {
		existing.UpsideDown = *req.UpsideDown
	}
	if req.TLSIgnore != nil {
		existing.TLSIgnore = *req.TLSIgnore
	}
	if req.CertExpiryNotify != nil {
		existing.CertExpiryNotify = *req.CertExpiryNotify
	}
	if req.ResendInterval != nil && *req.ResendInterval >= 0 {
		existing.ResendInterval = *req.ResendInterval
	}
	// group_id: omitted keeps the folder; null clears it. Placement is
	// re-checked only when the client actually asks to move the monitor.
	if req.GroupID.set && !sameOptionalID(existing.GroupID, req.GroupID.value) {
		userID, ok := userIDFromContext(c)
		if !ok {
			return unauthenticated(c)
		}
		allowed, accessErr := h.access.CanPlaceMonitorInGroup(c.Request().Context(), userID, req.GroupID.value)
		if accessErr != nil {
			return mapMonitorError(c, accessErr)
		}
		if !allowed {
			return c.JSON(http.StatusForbidden, errorBody("monitor placement is limited to an allowed group"))
		}
		existing.GroupID = req.GroupID.value
	}
	if req.ProxyID.set {
		existing.ProxyID = req.ProxyID.value
	}
	if req.Weight != nil {
		existing.Weight = *req.Weight
	}

	if err := h.svc.Update(c.Request().Context(), existing); err != nil {
		return mapMonitorError(c, err)
	}

	return c.JSON(http.StatusOK, toMonitorView(existing, h.monitorTags(c, id), h.groupsByID(c)))
}

// Delete handles DELETE /api/monitors/:id. Admins, or the user who created this
// monitor. Gated in the handler for the same reason as Update.
func (h *MonitorHandlers) Delete(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if err := requireMonitorEditAccess(c, h.access, id); err != nil {
		return err
	}

	// Confirm it exists so a delete of an unknown id is still a 404, not a 204.
	if _, err := h.svc.GetByID(c.Request().Context(), id); err != nil {
		return mapMonitorError(c, err)
	}

	if err := h.svc.Delete(c.Request().Context(), id); err != nil {
		return mapMonitorError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}

func sameOptionalID(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Clone handles POST /api/monitors/:id/clone — duplicates monitor config.
//
// Two gates, and note that the second is VIEW, not edit: cloning creates a new
// monitor (the create_monitors capability, checked by the router) out of config
// the caller must be allowed to read (checked here). Requiring ownership of the
// SOURCE would be stricter than the secret it protects — a user who can view a
// monitor can already read its config field-by-field from GET /api/monitors/:id
// and POST it back as a new one, so refusing to do it in one step would buy
// nothing. The clone belongs to the caller, not to the source's owner.
func (h *MonitorHandlers) Clone(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if err := requireMonitorViewAccess(c, h.access, id); err != nil {
		return err
	}

	source, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapMonitorError(c, err)
	}
	allowed, err := h.access.CanCreateMonitorInGroup(c.Request().Context(), userID, source.GroupID)
	if err != nil {
		return mapMonitorError(c, err)
	}
	if !allowed {
		return c.JSON(http.StatusForbidden, errorBody("monitor creation is limited to an allowed group"))
	}

	cloned, err := h.svc.Clone(c.Request().Context(), id, userID)
	if err != nil {
		return mapMonitorError(c, err)
	}
	h.grantCreatorAccess(c, userID, cloned.ID)
	return c.JSON(http.StatusCreated, toMonitorView(cloned, nil, h.groupsByID(c)))
}

// groupsByID loads every monitor group once for EffectiveOwner resolution.
// Failures degrade to nil (own Owner only) rather than failing the request.
func (h *MonitorHandlers) groupsByID(c echo.Context) map[int64]*domain.MonitorGroup {
	if h.groups == nil {
		return nil
	}
	all, err := h.groups.ListAll(c.Request().Context())
	if err != nil {
		slog.WarnContext(c.Request().Context(), "monitor: group lookup for effective owner failed", "error", err)
		return nil
	}
	out := make(map[int64]*domain.MonitorGroup, len(all))
	for _, g := range all {
		out[g.ID] = g
	}
	return out
}

// --- Error translation helper -------------------------------------------

func mapMonitorError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody("monitor not found"))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody(err.Error()))
	default:
		slog.Error("monitor handler error", "error", err)
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
