package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// AlertHandlers serves the F2.2 alert lifecycle API: list/get/ack for
// authenticated callers, and token-based ack for deep links.
//
// RBAC: a caller may only see or acknowledge alerts for monitors they can view.
// Denial is 404 (never confirm a monitor or alert the caller cannot see).
type AlertHandlers struct {
	svc        *services.AlertService
	access     *services.AccessService
	escalation escalationStateReader // optional — F2.3
}

// escalationStateReader reports each alert's escalation progress in ONE call.
// Satisfied by *EscalationService. Optional — without it the escalation field
// is simply absent from the wire.
type escalationStateReader interface {
	StatesForAlerts(ctx context.Context, alertIDs []int64) (map[int64]*domain.AlertEscalation, error)
}

// NewAlertHandlers creates handlers bound to the alert and access services.
func NewAlertHandlers(svc *services.AlertService, access *services.AccessService) *AlertHandlers {
	return &AlertHandlers{svc: svc, access: access}
}

// SetEscalationReader wires the F2.3 escalation progress shown alongside each
// alert. Optional.
func (h *AlertHandlers) SetEscalationReader(r escalationStateReader) {
	h.escalation = r
}

// AlertView is the wire shape of a monitor alert. Secrets: AckToken is never
// included on list views; it is only used server-side for deep-link ack.
type AlertView struct {
	ID            int64   `json:"id"`
	MonitorID     int64   `json:"monitor_id"`
	Status        string  `json:"status"`
	Message       string  `json:"message"`
	FiredAt       string  `json:"fired_at"`
	AckedAt       *string `json:"acked_at,omitempty"`
	AckedByUserID *int64  `json:"acked_by_user_id,omitempty"`
	ResolvedAt    *string `json:"resolved_at,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`

	// Escalation is the F2.3 ladder's progress for this alert, absent when no
	// policy applies. It carries no channel names or ids — a caller who cannot
	// see notification configuration still must not learn who gets paged.
	Escalation *AlertEscalationView `json:"escalation,omitempty"`
}

// AlertEscalationView is the wire shape of an alert's escalation progress.
type AlertEscalationView struct {
	Status    string `json:"status"`      // pending | done | canceled
	NextStep  int    `json:"next_step"`   // the step not yet sent; 1-based
	NextRunAt string `json:"next_run_at"` // RFC3339 UTC
}

func toAlertEscalationView(e *domain.AlertEscalation) *AlertEscalationView {
	if e == nil {
		return nil
	}
	return &AlertEscalationView{
		Status:    e.Status,
		NextStep:  e.NextStep,
		NextRunAt: e.NextRunAt.UTC().Format(time.RFC3339),
	}
}

func toAlertView(a *domain.Alert) AlertView {
	v := AlertView{
		ID:        a.ID,
		MonitorID: a.MonitorID,
		Status:    a.Status,
		Message:   a.Message,
		FiredAt:   a.FiredAt.UTC().Format(time.RFC3339),
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: a.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if a.AckedAt != nil {
		s := a.AckedAt.UTC().Format(time.RFC3339)
		v.AckedAt = &s
	}
	if a.AckedByUserID != nil {
		v.AckedByUserID = a.AckedByUserID
	}
	if a.ResolvedAt != nil {
		s := a.ResolvedAt.UTC().Format(time.RFC3339)
		v.ResolvedAt = &s
	}
	return v
}

// List handles GET /api/alerts.
//
// Query: status=firing,acked (comma-separated), open=1, monitor_id=N, limit, offset.
func (h *AlertHandlers) List(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	filter, err := h.buildFilter(c, userID)
	if err != nil {
		return err
	}
	alerts, err := h.svc.List(c.Request().Context(), filter)
	if err != nil {
		return mapAlertError(c, err)
	}
	out := make([]AlertView, 0, len(alerts))
	ids := make([]int64, 0, len(alerts))
	for _, a := range alerts {
		ids = append(ids, a.ID)
	}
	states := h.escalationStates(c.Request().Context(), ids)
	for _, a := range alerts {
		v := toAlertView(a)
		v.Escalation = toAlertEscalationView(states[a.ID])
		out = append(out, v)
	}
	return c.JSON(http.StatusOK, out)
}

// escalationStates resolves every alert's ladder progress in one call. A
// failure degrades to "no escalation shown" rather than failing the list: the
// alerts themselves are the point of the endpoint, and a decorative field must
// not be able to take it down. It is logged, never swallowed silently.
func (h *AlertHandlers) escalationStates(ctx context.Context, alertIDs []int64) map[int64]*domain.AlertEscalation {
	if h.escalation == nil || len(alertIDs) == 0 {
		return nil
	}
	states, err := h.escalation.StatesForAlerts(ctx, alertIDs)
	if err != nil {
		slog.Error("alert handlers: escalation states lookup failed", "error", err)
		return nil
	}
	return states
}

// Get handles GET /api/alerts/:id.
func (h *AlertHandlers) Get(c echo.Context) error {
	if _, ok := userIDFromContext(c); !ok {
		return unauthenticated(c)
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return c.JSON(http.StatusBadRequest, errorBody("invalid alert id"))
	}
	a, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapAlertError(c, err)
	}
	if err := requireMonitorViewAccess(c, h.access, a.MonitorID); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, h.viewWithEscalation(c.Request().Context(), a))
}

// viewWithEscalation decorates a single alert with its ladder progress.
func (h *AlertHandlers) viewWithEscalation(ctx context.Context, a *domain.Alert) AlertView {
	v := toAlertView(a)
	v.Escalation = toAlertEscalationView(h.escalationStates(ctx, []int64{a.ID})[a.ID])
	return v
}

// Acknowledge handles POST /api/alerts/:id/ack.
func (h *AlertHandlers) Acknowledge(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return c.JSON(http.StatusBadRequest, errorBody("invalid alert id"))
	}
	a, err := h.svc.GetByID(c.Request().Context(), id)
	if err != nil {
		return mapAlertError(c, err)
	}
	if err := requireMonitorViewAccess(c, h.access, a.MonitorID); err != nil {
		return err
	}
	uid := userID
	acked, err := h.svc.Acknowledge(c.Request().Context(), id, &uid)
	if err != nil {
		return mapAlertError(c, err)
	}
	return c.JSON(http.StatusOK, h.viewWithEscalation(c.Request().Context(), acked))
}

// AckByTokenRequest is the body of POST /api/alerts/ack-by-token.
type AckByTokenRequest struct {
	Token string `json:"token"`
}

// AcknowledgeByToken handles POST /api/alerts/ack-by-token (public deep-link).
// No session required; the high-entropy token is the credential.
func (h *AlertHandlers) AcknowledgeByToken(c echo.Context) error {
	var req AckByTokenRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorBody("invalid request body"))
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		return c.JSON(http.StatusBadRequest, errorBody("token is required"))
	}
	acked, err := h.svc.AcknowledgeByToken(c.Request().Context(), req.Token)
	if err != nil {
		return mapAlertError(c, err)
	}
	return c.JSON(http.StatusOK, toAlertView(acked))
}

func (h *AlertHandlers) buildFilter(c echo.Context, userID int64) (ports.AlertFilter, error) {
	filter := ports.AlertFilter{
		Limit:  100,
		Offset: 0,
	}
	if s := c.QueryParam("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 500 {
			return filter, c.JSON(http.StatusBadRequest, errorBody("invalid limit"))
		}
		filter.Limit = n
	}
	if s := c.QueryParam("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return filter, c.JSON(http.StatusBadRequest, errorBody("invalid offset"))
		}
		filter.Offset = n
	}
	if c.QueryParam("open") == "1" || strings.EqualFold(c.QueryParam("open"), "true") {
		filter.OpenOnly = true
	}
	if s := c.QueryParam("status"); s != "" {
		parts := strings.Split(s, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			switch p {
			case domain.AlertStatusFiring, domain.AlertStatusAcked, domain.AlertStatusResolved:
				filter.Statuses = append(filter.Statuses, p)
			case "":
				continue
			default:
				return filter, c.JSON(http.StatusBadRequest, errorBody("invalid status filter"))
			}
		}
	}
	if s := c.QueryParam("monitor_id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return filter, c.JSON(http.StatusBadRequest, errorBody("invalid monitor_id"))
		}
		if err := requireMonitorViewAccess(c, h.access, id); err != nil {
			return filter, err
		}
		filter.MonitorID = &id
	}

	// Scope the list to monitors the caller may view.
	if h.access == nil {
		return filter, c.JSON(http.StatusForbidden, errorBody("access control unavailable"))
	}
	all, ids, err := h.access.VisibleMonitorIDs(c.Request().Context(), userID)
	if err != nil {
		return filter, mapAlertError(c, err)
	}
	if !all {
		filter.RestrictToMonitorIDs = true
		filter.MonitorIDs = ids
	}
	return filter, nil
}

func mapAlertError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound), errors.Is(err, domain.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorBody("alert not found"))
	case errors.Is(err, domain.ErrConflict):
		return c.JSON(http.StatusConflict, errorBody("alert cannot be acknowledged"))
	case errors.Is(err, domain.ErrValidation):
		return c.JSON(http.StatusBadRequest, errorBody("invalid alert request"))
	default:
		return c.JSON(http.StatusInternalServerError, errorBody("internal error"))
	}
}
