package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// MonitorConditionHandlers exposes access-scoped latest condition state.
type MonitorConditionHandlers struct {
	svc    *services.MonitorConditionService
	access *services.AccessService
}

// NewMonitorConditionHandlers creates condition handlers.
func NewMonitorConditionHandlers(svc *services.MonitorConditionService, access *services.AccessService) *MonitorConditionHandlers {
	return &MonitorConditionHandlers{svc: svc, access: access}
}

type monitorConditionView struct {
	MonitorID     int64                 `json:"monitor_id"`
	Kind          string                `json:"kind"`
	State         domain.ConditionState `json:"state"`
	Used          *float64              `json:"used"`
	Limit         *float64              `json:"limit"`
	Percent       *float64              `json:"percent"`
	Threshold     *float64              `json:"threshold"`
	Unit          string                `json:"unit"`
	Resource      string                `json:"resource"`
	Scope         string                `json:"scope"`
	Source        string                `json:"source"`
	Message       string                `json:"message"`
	ObservedAt    string                `json:"observed_at"`
	StaleAfter    string                `json:"stale_after"`
	LastSuccessAt *string               `json:"last_success_at"`
}

// List handles GET /api/monitor-conditions. An optional monitor_id narrows the
// response; list responses are always scoped through AccessService.
func (h *MonitorConditionHandlers) List(c echo.Context) error {
	if h == nil || h.svc == nil || h.access == nil {
		return c.JSON(http.StatusInternalServerError, errorBody("monitor conditions unavailable"))
	}
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}
	ctx := c.Request().Context()

	var conditions []*domain.MonitorCondition
	var err error
	if raw := c.QueryParam("monitor_id"); raw != "" {
		monitorID, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || monitorID <= 0 {
			return badRequest(c, "invalid monitor id")
		}
		if allowed, accessErr := h.access.CanViewMonitor(ctx, userID, monitorID); accessErr != nil || !allowed {
			return c.JSON(http.StatusNotFound, errorBody("monitor not found"))
		}
		conditions, err = h.svc.ListByMonitorIDs(ctx, []int64{monitorID})
	} else {
		all, ids, accessErr := h.access.VisibleMonitorIDs(ctx, userID)
		if accessErr != nil {
			return c.JSON(http.StatusInternalServerError, errorBody("failed to resolve monitor access"))
		}
		if all {
			conditions, err = h.svc.ListAll(ctx)
		} else {
			conditions, err = h.svc.ListByMonitorIDs(ctx, ids)
		}
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorBody("failed to list monitor conditions"))
	}

	now := time.Now().UTC()
	views := make([]monitorConditionView, 0, len(conditions))
	for _, condition := range conditions {
		if condition == nil || condition.State == "" {
			continue
		}
		views = append(views, toMonitorConditionView(condition, now))
	}
	return c.JSON(http.StatusOK, views)
}

func toMonitorConditionView(condition *domain.MonitorCondition, now time.Time) monitorConditionView {
	view := monitorConditionView{
		MonitorID:  condition.MonitorID,
		Kind:       condition.Kind,
		State:      condition.DisplayState(now),
		Used:       condition.Used,
		Limit:      condition.Limit,
		Percent:    condition.Percent,
		Threshold:  condition.Threshold,
		Unit:       condition.Unit,
		Resource:   condition.Resource,
		Scope:      condition.Scope,
		Source:     condition.Source,
		Message:    condition.Message,
		ObservedAt: condition.ObservedAt.UTC().Format(time.RFC3339),
		StaleAfter: condition.StaleAfter.UTC().Format(time.RFC3339),
	}
	if condition.LastSuccessAt != nil {
		lastSuccess := condition.LastSuccessAt.UTC().Format(time.RFC3339)
		view.LastSuccessAt = &lastSuccess
	}
	return view
}
