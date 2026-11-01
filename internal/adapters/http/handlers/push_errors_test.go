package handlers_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// #164: only a token that genuinely does not exist is 404. A lookup or write
// failure is the server's problem: 5xx, and always logged.

type failingPushLookupRepo struct {
	*fakeMonitorRepo
	err error
}

func (r *failingPushLookupRepo) GetByPushToken(context.Context, string) (*domain.Monitor, error) {
	return nil, r.err
}

type failingSaveHeartbeatRepo struct {
	recordingHeartbeatRepo
	err error
}

func (r *failingSaveHeartbeatRepo) Save(context.Context, *domain.Heartbeat) error { return r.err }

type failingPushAssignments struct{ *pushMockAssignments }

func (failingPushAssignments) ExecutableByLocal(context.Context, []int64) (map[int64]int64, error) {
	return nil, errors.New("dial tcp 172.18.0.4:3306: connect: connection refused")
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func pushRequest(t *testing.T, h *handlers.PushHandler, token string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/push/"+token, strings.NewReader(`{"status":"up"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token")
	c.SetParamValues(token)
	if err := h.Receive(c); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	return rec
}

func pushMonitor(t *testing.T, repo *fakeMonitorRepo) *domain.Monitor {
	t.Helper()
	mon := &domain.Monitor{Name: "push", Type: "push", Active: true, PushToken: "tok-164", Interval: 60, CreatedAt: time.Now().UTC()}
	if err := repo.Create(context.Background(), mon); err != nil {
		t.Fatal(err)
	}
	return mon
}

func TestPushHandler_UnknownTokenIs404(t *testing.T) {
	bus := &pushTestBus{}
	h := handlers.NewPushHandler(services.NewMonitorService(newFakeMonitorRepo(), bus), services.NewHeartbeatService(&recordingHeartbeatRepo{}, bus))
	if rec := pushRequest(t, h, "no-such-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: status %d, want 404", rec.Code)
	}
}

func TestPushHandler_LookupFailureIs503AndLogged(t *testing.T) {
	logs := captureSlog(t)
	bus := &pushTestBus{}
	repo := &failingPushLookupRepo{fakeMonitorRepo: newFakeMonitorRepo(), err: errors.New("dial tcp 172.18.0.4:3306: connect: connection refused")}
	h := handlers.NewPushHandler(services.NewMonitorService(repo, bus), services.NewHeartbeatService(&recordingHeartbeatRepo{}, bus))
	rec := pushRequest(t, h, "tok-164")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup failure: status %d, want 503 (a DB outage is not an unknown token)", rec.Code)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Fatalf("lookup failure was not logged; log = %q", logs.String())
	}
}

func TestPushHandler_AssignmentReadFailureIs503AndLogged(t *testing.T) {
	logs := captureSlog(t)
	bus := &pushTestBus{}
	repo := newFakeMonitorRepo()
	hbRepo := &recordingHeartbeatRepo{}
	pushMonitor(t, repo)
	h := handlers.NewPushHandler(services.NewMonitorService(repo, bus), services.NewHeartbeatService(hbRepo, bus))
	h.SetAssignmentRepo(failingPushAssignments{&pushMockAssignments{}})
	rec := pushRequest(t, h, "tok-164")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("assignment read failure: status %d, want 503", rec.Code)
	}
	if hbRepo.lastRecorded != nil {
		t.Fatalf("recorded a heartbeat without its assignment generation: %+v", hbRepo.lastRecorded)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Fatalf("assignment read failure was not logged; log = %q", logs.String())
	}
}

func TestPushHandler_RecordFailuresAreLogged(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"transient contention is 503", ports.ErrStaleLocalState, http.StatusServiceUnavailable},
		{"other write failure is 500", errors.New("disk I/O error"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			bus := &pushTestBus{}
			repo := newFakeMonitorRepo()
			mon := pushMonitor(t, repo)
			h := handlers.NewPushHandler(services.NewMonitorService(repo, bus), services.NewHeartbeatService(&failingSaveHeartbeatRepo{err: tc.err}, bus))
			rec := pushRequest(t, h, "tok-164")
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), fmt.Sprintf("monitor_id=%d", mon.ID)) {
				t.Fatalf("a %d left no ERROR log naming the monitor; log = %q", rec.Code, logs.String())
			}
		})
	}
}
