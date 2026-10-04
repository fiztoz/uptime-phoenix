package handlers

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// RegionalHeartbeatView is one regional history row. It keeps the existing
// heartbeat wire names and adds the regional identity fields from the frozen
// M0 shape (probe.DecodeRegionalHeartbeat): generation and revision are
// decimal strings and message stays message, never Msg.
type RegionalHeartbeatView struct {
	ID                   int64  `json:"id"`
	MonitorID            int64  `json:"monitor_id"`
	Status               string `json:"status"`
	Ping                 int    `json:"ping"`
	Message              string `json:"message"`
	Time                 string `json:"time"`
	Important            bool   `json:"important"`
	ProbeID              string `json:"probe_id"`
	ReceivedAt           string `json:"received_at"`
	AssignmentGeneration string `json:"assignment_generation"`
	ConfigRevision       string `json:"config_revision"`
}

func toRegionalHeartbeatView(row *domain.RegionalObservation) RegionalHeartbeatView {
	return RegionalHeartbeatView{
		ID:                   row.ID,
		MonitorID:            row.MonitorID,
		Status:               regionalStatusName(row.Status),
		Ping:                 row.Ping,
		Message:              row.Message,
		Time:                 row.ObservedAt.Format(time.RFC3339Nano),
		Important:            row.Important,
		ProbeID:              row.ProbeID,
		ReceivedAt:           row.ReceivedAt.Format(time.RFC3339Nano),
		AssignmentGeneration: strconv.FormatInt(row.AssignmentGeneration, 10),
		ConfigRevision:       strconv.FormatInt(row.ConfigRevision, 10),
	}
}

// regionalStatusName renders the lowercase HTTP vocabulary
// (`up`, `down`, `pending`, `maintenance`, `unknown`) for regional evidence.
func regionalStatusName(status domain.Status) string {
	switch status {
	case domain.StatusUp, domain.StatusDown, domain.StatusPending, domain.StatusMaintenance:
		return strings.ToLower(status.String())
	default:
		return "unknown"
	}
}

// ListHistory handles GET /api/monitors/:id/probes/:probe_id/heartbeats.
// It preserves the existing hours/limit/order/important query semantics and
// validates the monitor/probe relationship before reading any evidence.
func (h *MonitorRegionalHandlers) ListHistory(c echo.Context) error {
	userID, id, ok := h.request(c)
	if !ok {
		return nil
	}
	rows, ok := h.regionalHistory(c, userID, id)
	if !ok {
		return nil
	}
	importantOnly := parseImportantOnly(c)
	filtered := make([]*domain.RegionalObservation, 0, len(rows))
	for _, row := range rows {
		if importantOnly != nil && *importantOnly && !row.Important {
			continue
		}
		filtered = append(filtered, row)
	}
	// Same desc-cap-then-reorder shape as the unqualified list: the cap applies
	// to the most recent rows in the window, never the oldest.
	limit := parseHeartbeatLimit(c)
	sortRegionalObservations(filtered, "desc")
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	sortRegionalObservations(filtered, parseHeartbeatOrder(c))
	views := make([]RegionalHeartbeatView, 0, len(filtered))
	for _, row := range filtered {
		views = append(views, toRegionalHeartbeatView(row))
	}
	return c.JSON(http.StatusOK, views)
}

// GetHistoryChart handles GET /api/monitors/:id/probes/:probe_id/heartbeats/chart.
// Latency buckets come only from this probe's measured regional samples; the
// intervals carry downtime and unknown periods, never fabricated latency.
func (h *MonitorRegionalHandlers) GetHistoryChart(c echo.Context) error {
	userID, id, ok := h.request(c)
	if !ok {
		return nil
	}
	rows, ok := h.regionalHistory(c, userID, id)
	if !ok {
		return nil
	}
	const chartRegionalMax = 2000
	sortRegionalObservations(rows, "desc")
	if len(rows) > chartRegionalMax {
		rows = rows[:chartRegionalMax]
	}
	sortRegionalObservations(rows, "asc")
	samples := make([]*domain.Heartbeat, 0, len(rows))
	for _, row := range rows {
		samples = append(samples, &domain.Heartbeat{Status: row.Status, Ping: row.Ping, Time: row.ObservedAt})
	}
	hours := parseHeartbeatHours(c)
	buckets := services.BucketHeartbeats(samples, services.BucketDurationForRange(hours))
	view := chartDataView{
		Buckets:           make([]chartBucketView, 0, len(buckets)),
		DowntimeIntervals: make([]downtimeIntervalView, 0),
		UnknownIntervals:  make([]downtimeIntervalView, 0),
	}
	for _, b := range buckets {
		view.Buckets = append(view.Buckets, chartBucketView{
			Time: b.Time.Format(time.RFC3339),
			Min:  b.Min,
			Avg:  b.Avg,
			Max:  b.Max,
		})
	}
	for _, iv := range services.DetectDowntimeIntervals(samples) {
		view.DowntimeIntervals = append(view.DowntimeIntervals, downtimeIntervalView{
			Start: iv.Start.Format(time.RFC3339), End: iv.End.Format(time.RFC3339),
		})
	}
	for _, iv := range services.DetectUnknownIntervals(samples) {
		view.UnknownIntervals = append(view.UnknownIntervals, downtimeIntervalView{
			Start: iv.Start.Format(time.RFC3339), End: iv.End.Format(time.RFC3339),
		})
	}
	return c.JSON(http.StatusOK, view)
}

// regionalHistory resolves the request window and the relationship-checked
// evidence, writing the typed error response itself and reporting false when
// the response is complete.
func (h *MonitorRegionalHandlers) regionalHistory(c echo.Context, userID, monitorID int64) ([]*domain.RegionalObservation, bool) {
	probeID := c.Param("probe_id")
	if probeID == "" {
		_ = regionalError(c, http.StatusBadRequest, "invalid_probe_id", "invalid probe id")
		return nil, false
	}
	hours := parseHeartbeatHours(c)
	from, to := heartbeatWindow(hours)
	rows, err := h.svc.RegionalHistory(c.Request().Context(), userID, monitorID, probeID, from, to)
	if err != nil {
		_ = regionalHistoryError(c, err)
		return nil, false
	}
	out := make([]*domain.RegionalObservation, 0, len(rows))
	for i := range rows {
		out = append(out, &rows[i])
	}
	return out, true
}

func regionalHistoryError(c echo.Context, err error) error {
	if errors.Is(err, services.ErrProbeNotRelated) {
		return regionalError(c, http.StatusNotFound, "probe_not_found", "probe not found")
	}
	return regionalReadError(c, err)
}

func sortRegionalObservations(rows []*domain.RegionalObservation, order string) {
	sort.SliceStable(rows, func(i, j int) bool {
		if order == "asc" {
			return rows[i].ObservedAt.Before(rows[j].ObservedAt)
		}
		return rows[i].ObservedAt.After(rows[j].ObservedAt)
	})
}
