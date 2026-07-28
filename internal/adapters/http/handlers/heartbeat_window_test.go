package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// hbWindowRepo records the [from, to] bounds it was queried with, and filters on
// the WALL-CLOCK fields rather than the instant.
//
// That wall-clock comparison is the whole point. A normal in-memory fake compares
// time.Time instants, which are zone-independent, so it happily returns the right
// rows even when the handler passes a local-zoned bound — and the bug stays
// invisible. A SQL driver does not do that: it renders the bound into the query as
// its wall-clock text. Heartbeats are stored as UTC wall-clock, so a local-zoned
// bound shifts the window by the server's UTC offset. This fake reproduces that.
type hbWindowRepo struct {
	heartbeats []*domain.Heartbeat
	gotFrom    time.Time
	gotTo      time.Time
}

// wallClock strips the zone, keeping the displayed date/time — what a driver writes.
func wallClock(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

func (r *hbWindowRepo) ListByMonitor(_ context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	r.gotFrom, r.gotTo = from, to
	fromWall, toWall := wallClock(from), wallClock(to)

	var out []*domain.Heartbeat
	for _, h := range r.heartbeats {
		stored := wallClock(h.Time) // rows hold a UTC wall-clock
		if h.MonitorID == monitorID && !stored.Before(fromWall) && !stored.After(toWall) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (r *hbWindowRepo) Save(_ context.Context, hb *domain.Heartbeat) error {
	r.heartbeats = append(r.heartbeats, hb)
	return nil
}

func (r *hbWindowRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	for i := len(r.heartbeats) - 1; i >= 0; i-- {
		if r.heartbeats[i].MonitorID == monitorID {
			return r.heartbeats[i], nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *hbWindowRepo) DeleteByMonitor(_ context.Context, _ int64) error { return nil }
func (r *hbWindowRepo) DeleteOlderThan(_ context.Context, _ time.Time) error {
	return nil
}
func (r *hbWindowRepo) SaveAggregate1m(_ context.Context, _ *ports.Aggregate1m) error { return nil }
func (r *hbWindowRepo) SaveAggregate1h(_ context.Context, _ *ports.Aggregate1h) error { return nil }
func (r *hbWindowRepo) SaveAggregate1d(_ context.Context, _ *ports.Aggregate1d) error { return nil }
func (r *hbWindowRepo) GetAggregate1m(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *hbWindowRepo) GetAggregate1h(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *hbWindowRepo) GetAggregate1d(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

// setLocalZone points time.Local at a UTC+7 zone for the duration of the test, so
// time.Now() is local-zoned the way it is on the user's server.
func setLocalZone(t *testing.T) {
	t.Helper()
	prev := time.Local
	// Fixed offset, so the test doesn't depend on the tzdata database being present.
	time.Local = time.FixedZone("UTC+7", 7*60*60)
	t.Cleanup(func() { time.Local = prev })
}

// A fresh heartbeat must be visible in the short chart ranges.
//
// Regression guard: the handler built its window from time.Now() (local-zoned)
// while heartbeats are written with time.Now().UTC(). The driver rendered the
// local bound as its local wall-clock, shifting the window forward by the server's
// UTC offset — on a UTC+7 host the 1h/3h/6h ranges queried a window 7 hours in the
// FUTURE and so returned zero rows forever, permanently blanking the chart. The
// 24h range still worked, which is why this looked like a rendering glitch.
func TestHeartbeatHandlers_ShortRangesSeeRecentHeartbeats_UnderNonUTCServerZone(t *testing.T) {
	setLocalZone(t)

	monRepo := newFakeMonitorRepo()
	bus := newFakeMonitorBus()

	owner := &domain.Monitor{UserID: 1, Name: "m", Type: "http", Active: true, Interval: 60}
	if err := monRepo.Create(context.Background(), owner); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	hbRepo := &hbWindowRepo{}
	hbSvc := services.NewHeartbeatService(hbRepo, bus)
	// Stored the way the scheduler stores them: a UTC wall-clock, a few seconds ago.
	_ = hbRepo.Save(context.Background(), &domain.Heartbeat{
		MonitorID: owner.ID, Status: domain.StatusUp, Ping: 42,
		Time: time.Now().UTC().Add(-30 * time.Second),
	})

	// User 1 is an admin here: this test is about the UTC query window, not about
	// RBAC, so give the caller unrestricted visibility and change nothing else.
	userRepo := memory.NewUserRepo()
	_ = userRepo.Create(context.Background(), &domain.User{Username: "admin", Active: true, IsAdmin: true})
	accessSvc := services.NewAccessService(userRepo, memory.NewUserPermissionRepo(), nil, monRepo)

	h := handlers.NewHeartbeatHandlers(hbSvc, accessSvc)
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	g := e.Group("/api/monitors/:id/heartbeats", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(handlers.ContextUserIDKey, int64(1))
			return next(c)
		}
	})
	g.GET("", h.ListByMonitor)
	g.GET("/chart", h.GetChartData)

	for _, hours := range []int{1, 3, 6, 24} {
		t.Run("list_hours_"+itoa(hours), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/monitors/1/heartbeats?hours="+itoa(hours), nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET heartbeats?hours=%d returned %d (body: %s)", hours, rec.Code, rec.Body.String())
			}
			var got []map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("hours=%d returned %d heartbeats; want 1. The query window was [%s, %s] "+
					"— a 30-second-old heartbeat must fall inside every range.",
					hours, len(got), hbRepo.gotFrom.Format(time.RFC3339), hbRepo.gotTo.Format(time.RFC3339))
			}
		})
	}

	// The invariant behind the fix: bounds reaching the repository are always UTC.
	if loc := hbRepo.gotFrom.Location(); loc != time.UTC {
		t.Errorf("from bound location = %v; want UTC", loc)
	}
	if loc := hbRepo.gotTo.Location(); loc != time.UTC {
		t.Errorf("to bound location = %v; want UTC", loc)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
