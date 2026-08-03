package handlers

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// InsightsHandlers exposes the authenticated reliability read model. The result
// is scoped inside the service to the monitors the caller may see, so there is
// no per-monitor middleware here — a middleware can reject a request but cannot
// narrow a result set (same rationale as the monitor list route).
type InsightsHandlers struct {
	svc *services.InsightsService
}

// NewInsightsHandlers creates the reliability endpoint handlers.
func NewInsightsHandlers(svc *services.InsightsService) *InsightsHandlers {
	return &InsightsHandlers{svc: svc}
}

// insightsRowView is the wire shape of one ranked monitor. Every field carries
// an explicit snake_case json tag; no domain type is ever serialized directly
// (rule 5). Nullable numbers are pointers so "unknown" is null, never a
// fabricated zero or 100.
type insightsRowView struct {
	MonitorID           int64    `json:"monitor_id"`
	MonitorName         string   `json:"monitor_name"`
	MonitorType         string   `json:"monitor_type"`
	GroupID             *int64   `json:"group_id"`
	AvailabilityPercent *float64 `json:"availability_percent"`
	OutageCount         int      `json:"outage_count"`
	DowntimeSeconds     int64    `json:"downtime_seconds"`
	FlapCount           int      `json:"flap_count"`
	LatencyAvgMs        *float64 `json:"latency_avg_ms"`
	LatencySampleCount  int64    `json:"latency_sample_count"`
	CoveragePercent     float64  `json:"coverage_percent"`
	Qualification       string   `json:"qualification"`
}

type insightsView struct {
	From          string            `json:"from"`
	To            string            `json:"to"`
	Period        string            `json:"period"`
	Metric        string            `json:"metric"`
	CoverageBasis string            `json:"coverage_basis"`
	Rows          []insightsRowView `json:"rows"`
}

// GetInsights handles GET /api/insights.
//
// Query params: period (24h|7d|30d|90d), metric (availability|outages|downtime|
// latency|flapping), type (monitor type filter), group_id (int, includes
// descendant groups). All are optional; unknown values fall back to safe
// defaults inside the service.
func (h *InsightsHandlers) GetInsights(c echo.Context) error {
	userID, ok := userIDFromContext(c)
	if !ok {
		return unauthenticated(c)
	}

	q := services.InsightsQuery{
		UserID: userID,
		Period: services.ParsePeriod(c.QueryParam("period")),
		Metric: services.ParseMetric(c.QueryParam("metric")),
		Type:   c.QueryParam("type"),
	}
	if raw := c.QueryParam("group_id"); raw != "" {
		gid, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return badRequest(c, "invalid group_id")
		}
		q.GroupID = &gid
	}

	res, err := h.svc.GetInsights(c.Request().Context(), q)
	if err != nil {
		if errors.Is(err, services.ErrInsightsLatencyTypeRequired) {
			return badRequest(c, err.Error())
		}
		return mapMonitorError(c, err)
	}

	view := insightsView{
		From:          res.From.UTC().Format(time.RFC3339),
		To:            res.To.UTC().Format(time.RFC3339),
		Period:        string(res.Period),
		Metric:        string(res.Metric),
		CoverageBasis: res.CoverageBasis,
		Rows:          make([]insightsRowView, 0, len(res.Rows)),
	}
	for _, r := range res.Rows {
		view.Rows = append(view.Rows, insightsRowView{
			MonitorID:           r.MonitorID,
			MonitorName:         r.MonitorName,
			MonitorType:         r.MonitorType,
			GroupID:             r.GroupID,
			AvailabilityPercent: round2Ptr(r.AvailabilityPercent),
			OutageCount:         r.OutageCount,
			DowntimeSeconds:     r.DowntimeSeconds,
			FlapCount:           r.FlapCount,
			LatencyAvgMs:        round2Ptr(r.LatencyAvgMs),
			LatencySampleCount:  r.LatencySampleN,
			CoveragePercent:     round2(r.CoveragePercent),
			Qualification:       string(r.Qualification),
		})
	}
	return c.JSON(http.StatusOK, view)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func round2Ptr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := round2(*v)
	return &r
}
