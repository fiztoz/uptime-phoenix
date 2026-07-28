package handlers

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// HeartbeatHandlers exposes heartbeat query endpoints.
//
// Every route here is scoped to a single monitor, so every route gates on
// "can this caller VIEW that monitor?" via the access service. ClearHistory is
// additionally admin-only (destroying a monitor's history is a monitor mutation)
// and is wrapped in middleware.RequireAdmin by the router.
type HeartbeatHandlers struct {
	svc    *services.HeartbeatService
	access *services.AccessService
}

// NewHeartbeatHandlers creates handlers for heartbeat queries.
func NewHeartbeatHandlers(svc *services.HeartbeatService, access *services.AccessService) *HeartbeatHandlers {
	return &HeartbeatHandlers{svc: svc, access: access}
}

type heartbeatView struct {
	ID        int64  `json:"id"`
	MonitorID int64  `json:"monitor_id"`
	Status    string `json:"status"`
	Ping      int    `json:"ping"`
	Message   string `json:"message"`
	Time      string `json:"time"`
	Important bool   `json:"important"`
}

type chartBucketView struct {
	Time string  `json:"time"`
	Min  int     `json:"min"`
	Avg  float64 `json:"avg"`
	Max  int     `json:"max"`
}

type downtimeIntervalView struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type chartDataView struct {
	Buckets           []chartBucketView      `json:"buckets"`
	DowntimeIntervals []downtimeIntervalView `json:"downtime_intervals"`
}

// ListByMonitor handles GET /api/monitors/:id/heartbeats.
func (h *HeartbeatHandlers) ListByMonitor(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}

	hours := parseHeartbeatHours(c)
	limit := parseHeartbeatLimit(c)
	order := parseHeartbeatOrder(c)
	importantOnly := parseImportantOnly(c)

	// UTC, not local: heartbeats are stored as UTC wall-clock, and a local-zoned
	// bound is rendered as its local wall-clock in the SQL, shifting the window by
	// the server's UTC offset. See heartbeatWindow.
	from, to := heartbeatWindow(hours)
	heartbeats, err := h.svc.ListByMonitor(c.Request().Context(), monitorID, from, to)
	if err != nil {
		return mapMonitorError(c, err)
	}

	filtered := filterHeartbeats(heartbeats, importantOnly)
	// Cap to the MOST RECENT `limit` rows before applying the requested order.
	// Sorting ascending first and then truncating returns the OLDEST rows in the
	// window instead: the "Recent Checks" bar asks for hours=24&limit=60&order=asc,
	// and on a monitor with more than 60 beats in 24h that served the first 60
	// checks of the day — a monitor that had been down for hours still rendered a
	// full row of green. Same desc-limit-then-reorder shape as GetChartData.
	recent := limitHeartbeats(sortHeartbeats(filtered, "desc"), limit)
	sorted := sortHeartbeats(recent, order)
	return c.JSON(http.StatusOK, toHeartbeatViews(sorted))
}

// GetChartData handles GET /api/monitors/:id/heartbeats/chart.
// Returns server-side bucketed latency and downtime intervals (Go chart_aggregate).
func (h *HeartbeatHandlers) GetChartData(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}

	hours := parseHeartbeatHours(c)
	from, to := heartbeatWindow(hours)
	heartbeats, err := h.svc.ListByMonitor(c.Request().Context(), monitorID, from, to)
	if err != nil {
		return mapMonitorError(c, err)
	}

	// Cap raw samples before aggregation (same guard as list, higher ceiling for charts).
	const chartHeartbeatMax = 2000
	recent := limitHeartbeats(sortHeartbeats(heartbeats, "desc"), chartHeartbeatMax)
	heartbeats = sortHeartbeats(recent, "asc")

	bucketDur := services.BucketDurationForRange(hours)
	buckets := services.BucketHeartbeats(heartbeats, bucketDur)
	intervals := services.DetectDowntimeIntervals(heartbeats)

	view := chartDataView{
		Buckets:           make([]chartBucketView, 0, len(buckets)),
		DowntimeIntervals: make([]downtimeIntervalView, 0, len(intervals)),
	}
	for _, b := range buckets {
		view.Buckets = append(view.Buckets, chartBucketView{
			Time: b.Time.Format(time.RFC3339),
			Min:  b.Min,
			Avg:  b.Avg,
			Max:  b.Max,
		})
	}
	for _, iv := range intervals {
		view.DowntimeIntervals = append(view.DowntimeIntervals, downtimeIntervalView{
			Start: iv.Start.Format(time.RFC3339),
			End:   iv.End.Format(time.RFC3339),
		})
	}
	return c.JSON(http.StatusOK, view)
}

// ClearHistory handles DELETE /api/monitors/:id/heartbeats — removes all heartbeat
// rows. Admin-only: wiping a monitor's history is a destructive monitor mutation,
// and non-admins are read-only on monitors.
func (h *HeartbeatHandlers) ClearHistory(c echo.Context) error {
	monitorID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return badRequest(c, "invalid monitor id")
	}
	// View check FIRST, admin check second: a caller who cannot see the monitor
	// must get a 404 (it does not exist, as far as they are concerned) rather than
	// a 403 that confirms it does.
	if err := requireMonitorViewAccess(c, h.access, monitorID); err != nil {
		return err
	}
	if err := requireAdminAccess(c, h.access); err != nil {
		return err
	}
	if err := h.svc.ClearHistory(c.Request().Context(), monitorID); err != nil {
		return mapMonitorError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// heartbeatWindow returns the [from, to] query window for the last `hours`, in UTC.
//
// The UTC part is load-bearing, not cosmetic. Heartbeats are written with
// time.Now().UTC(), so the stored wall-clock is UTC. A local-zoned bound
// (time.Now(), which carries the server's zone) gets rendered into SQL as its
// LOCAL wall-clock, so the comparison window is silently shifted by the server's
// UTC offset. On a UTC+7 host that made every heartbeat from the last 7 hours
// invisible: the 1h/3h/6h chart ranges returned zero rows forever, while 24h
// still worked because its window was wide enough to reach back past the skew.
func heartbeatWindow(hours int) (from, to time.Time) {
	to = time.Now().UTC()
	from = to.Add(-time.Duration(hours) * time.Hour)
	return from, to
}

func parseHeartbeatHours(c echo.Context) int {
	hours := 24
	if h := c.QueryParam("hours"); h != "" {
		if v, parseErr := strconv.Atoi(h); parseErr == nil && v > 0 && v <= 720 {
			hours = v
		}
	}
	return hours
}

func parseHeartbeatLimit(c echo.Context) int {
	limit := 100
	if l := c.QueryParam("limit"); l != "" {
		if v, parseErr := strconv.Atoi(l); parseErr == nil && v > 0 {
			limit = v
			if limit > 500 {
				limit = 500
			}
		}
	}
	return limit
}

func parseHeartbeatOrder(c echo.Context) string {
	order := strings.ToLower(c.QueryParam("order"))
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		order = "desc"
	}
	return order
}

func parseImportantOnly(c echo.Context) *bool {
	if imp := c.QueryParam("important"); imp != "" {
		v := strings.EqualFold(imp, "true") || imp == "1"
		return &v
	}
	return nil
}

func toHeartbeatViews(heartbeats []*domain.Heartbeat) []heartbeatView {
	views := make([]heartbeatView, 0, len(heartbeats))
	for _, hb := range heartbeats {
		views = append(views, heartbeatView{
			ID:        hb.ID,
			MonitorID: hb.MonitorID,
			Status:    strings.ToLower(hb.Status.String()),
			Ping:      hb.Ping,
			Message:   hb.Msg,
			Time:      hb.Time.Format(time.RFC3339Nano),
			Important: hb.Important,
		})
	}
	return views
}

func filterHeartbeats(heartbeats []*domain.Heartbeat, importantOnly *bool) []*domain.Heartbeat {
	if importantOnly == nil || !*importantOnly {
		return heartbeats
	}
	out := make([]*domain.Heartbeat, 0, len(heartbeats))
	for _, hb := range heartbeats {
		if hb.Important {
			out = append(out, hb)
		}
	}
	return out
}

func sortHeartbeats(heartbeats []*domain.Heartbeat, order string) []*domain.Heartbeat {
	out := make([]*domain.Heartbeat, len(heartbeats))
	copy(out, heartbeats)
	sort.Slice(out, func(i, j int) bool {
		if order == "asc" {
			return out[i].Time.Before(out[j].Time)
		}
		return out[i].Time.After(out[j].Time)
	})
	return out
}

func limitHeartbeats(heartbeats []*domain.Heartbeat, limit int) []*domain.Heartbeat {
	if limit <= 0 || len(heartbeats) <= limit {
		return heartbeats
	}
	return heartbeats[:limit]
}
