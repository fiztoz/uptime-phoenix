package repository_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Issue #195: the response-time charts are labelled with a window (the public
// status page says "24 h") but used to keep only the newest N raw heartbeats
// before bucketing — 500 on the public page (about 8.3 h at a 60 s interval)
// and 2000 on the authenticated detail chart (about 16.7 h at 30 s). These
// contracts drive the real production entry points against each engine and
// assert the chart spans the whole labelled window.

// noIncidents satisfies ports.IncidentRepository for a status page with no
// incidents. Only ListByStatusPage is reached by the public read path.
type noIncidents struct{ ports.IncidentRepository }

func (noIncidents) ListByStatusPage(context.Context, int64) ([]*domain.Incident, error) {
	return nil, nil
}

// seedChartHeartbeats writes one heartbeat every interval from now-span to now.
// A short DOWN run is placed downAgo before now so a test can tell whether the
// downtime intervals, not only the latency buckets, cover the window.
func seedChartHeartbeats(
	t *testing.T, ctx context.Context, repo ports.HeartbeatRepository, monitorID int64,
	now time.Time, span, interval, downAgo time.Duration,
) {
	t.Helper()
	n := int(span / interval)
	for k := n; k >= 0; k-- {
		at := now.Add(-time.Duration(k) * interval)
		hb := &domain.Heartbeat{MonitorID: monitorID, Status: domain.StatusUp, Ping: 10 + k%50, Time: at}
		if age := now.Sub(at); age <= downAgo && age > downAgo-5*interval {
			hb.Status, hb.Ping = domain.StatusDown, 0
		}
		if err := repo.Save(ctx, hb); err != nil {
			t.Fatalf("Save heartbeat %d: %v", k, err)
		}
	}
}

type chartWindowView struct {
	Buckets []struct {
		Time string  `json:"time"`
		Min  int     `json:"min"`
		Avg  float64 `json:"avg"`
		Max  int     `json:"max"`
	} `json:"buckets"`
	DowntimeIntervals []struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"downtime_intervals"`
}

// assertChartCoversWindow checks the bucket list spans [from, to] at 15 min
// granularity and that the old DOWN run near the start of the window survived.
func assertChartCoversWindow(t *testing.T, chart chartWindowView, from, to, downAt time.Time) {
	t.Helper()
	const bucket = 15 * time.Minute
	if len(chart.Buckets) < 95 || len(chart.Buckets) > 97 {
		t.Fatalf("chart has %d buckets; want ~96 fifteen-minute buckets covering the 24 h window", len(chart.Buckets))
	}
	first, err := time.Parse(time.RFC3339, chart.Buckets[0].Time)
	if err != nil {
		t.Fatalf("parse first bucket: %v", err)
	}
	last, err := time.Parse(time.RFC3339, chart.Buckets[len(chart.Buckets)-1].Time)
	if err != nil {
		t.Fatalf("parse last bucket: %v", err)
	}
	if first.Before(from.Add(-bucket)) || first.After(from.Add(bucket)) {
		t.Fatalf("first bucket %s; want the start of the window %s (chart truncated to the newest samples)",
			first.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	if last.Before(to.Add(-2 * bucket)) {
		t.Fatalf("last bucket %s; want the end of the window %s", last.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	for i, b := range chart.Buckets {
		if b.Min <= 0 || b.Max < b.Min || b.Avg < float64(b.Min) || b.Avg > float64(b.Max) {
			t.Fatalf("bucket %d has inconsistent latency %+v", i, b)
		}
	}
	found := false
	for _, iv := range chart.DowntimeIntervals {
		start, _ := time.Parse(time.RFC3339, iv.Start)
		end, _ := time.Parse(time.RFC3339, iv.End)
		if !downAt.Before(start.Add(-10*time.Minute)) && !downAt.After(end.Add(10*time.Minute)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("downtime intervals %+v miss the DOWN run at %s near the start of the window",
			chart.DowntimeIntervals, downAt.Format(time.RFC3339))
	}
}

func runPublicChartWindowContract(t *testing.T, factory repositoryFactory) {
	t.Helper()
	ctx := context.Background()
	repos := factory(t)
	user := createUser(t, ctx, repos, "chart-window-public")
	monitor := createMonitor(t, ctx, repos, user.ID, "chart-window-public")
	sp := &domain.StatusPage{Slug: "chart-window", Title: "Chart Window", Theme: "auto", Published: true}
	if err := repos.statusPages.Create(ctx, sp); err != nil {
		t.Fatalf("Create status page: %v", err)
	}
	if err := repos.statusPageMonitors.AddMonitor(ctx, sp.ID, monitor.ID, 1); err != nil {
		t.Fatalf("AddMonitor: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	// 26 h of 60 s heartbeats: far more than the old 500-sample cap.
	seedChartHeartbeats(t, ctx, repos.heartbeats, monitor.ID, now, 26*time.Hour, time.Minute, 20*time.Hour)

	svc := services.NewStatusPageService(repos.statusPages, noIncidents{}, nil,
		repos.statusPageMonitors, repos.monitors, repos.heartbeats, nil)
	resp, err := svc.GetPublicStatus(ctx, sp.Slug)
	if err != nil {
		t.Fatalf("GetPublicStatus: %v", err)
	}
	if len(resp.Monitors) != 1 || resp.Monitors[0].Chart == nil {
		t.Fatalf("public status monitors = %+v; want one monitor with a chart", resp.Monitors)
	}
	raw, err := json.Marshal(resp.Monitors[0].Chart)
	if err != nil {
		t.Fatalf("marshal chart: %v", err)
	}
	var chart chartWindowView
	if err := json.Unmarshal(raw, &chart); err != nil {
		t.Fatalf("unmarshal chart: %v", err)
	}
	assertChartCoversWindow(t, chart, now.Add(-24*time.Hour), now, now.Add(-20*time.Hour))
}

func TestPublicChartCoversFullWindow(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { runPublicChartWindowContract(t, sqliteFactory) })
	t.Run("mariadb", func(t *testing.T) { runPublicChartWindowContract(t, mariadbFactory) })
}

func runAuthenticatedChartWindowContract(t *testing.T, factory repositoryFactory) {
	t.Helper()
	ctx := context.Background()
	repos := factory(t)
	admin := &domain.User{Username: "chart-window-admin", PasswordHash: "hash", Active: true, IsAdmin: true, Timezone: "UTC"}
	if err := repos.users.Create(ctx, admin); err != nil {
		t.Fatalf("Create admin: %v", err)
	}
	monitor := createMonitor(t, ctx, repos, admin.ID, "chart-window-auth")

	now := time.Now().UTC().Truncate(time.Second)
	// 25 h of 30 s heartbeats (3000 rows): more than the old 2000-sample cap,
	// which covered only ~16.7 h of the 24 h range.
	seedChartHeartbeats(t, ctx, repos.heartbeats, monitor.ID, now, 25*time.Hour, 30*time.Second, 23*time.Hour)

	h := handlers.NewHeartbeatHandlers(
		services.NewHeartbeatService(repos.heartbeats, nil),
		services.NewAccessService(repos.users, repos.userPermissions, nil, repos.monitors),
	)
	e := echo.New()
	e.GET("/api/monitors/:id/heartbeats/chart", h.GetChartData, func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, admin.ID)
			return next(c)
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/monitors/"+strconv.FormatInt(monitor.ID, 10)+"/heartbeats/chart?hours=24", nil)
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chart status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var chart chartWindowView
	if err := json.Unmarshal(rec.Body.Bytes(), &chart); err != nil {
		t.Fatalf("unmarshal chart: %v", err)
	}
	assertChartCoversWindow(t, chart, now.Add(-24*time.Hour), now, now.Add(-23*time.Hour))
}

func TestAuthenticatedChartCoversFullWindow(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { runAuthenticatedChartWindowContract(t, sqliteFactory) })
	t.Run("mariadb", func(t *testing.T) { runAuthenticatedChartWindowContract(t, mariadbFactory) })
}
