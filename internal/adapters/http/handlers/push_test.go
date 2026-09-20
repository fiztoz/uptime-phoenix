package handlers_test

import (
	"context"
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

type pushMockAssignments struct {
	generations map[int64]int64
}

func (m *pushMockAssignments) InitializeLocal(_ context.Context, _ int64) (*domain.MonitorProbeAssignments, error) {
	return nil, nil
}
func (m *pushMockAssignments) GetByMonitorID(_ context.Context, _ int64) (*domain.MonitorProbeAssignments, error) {
	return nil, nil
}
func (m *pushMockAssignments) Replace(_ context.Context, _ int64, _ int64, _ []string, _ domain.HealthPolicy) (*domain.MonitorProbeAssignments, error) {
	return nil, nil
}
func (m *pushMockAssignments) ExecutableByLocal(_ context.Context, monitorIDs []int64) (map[int64]int64, error) {
	out := make(map[int64]int64, len(monitorIDs))
	for _, id := range monitorIDs {
		if gen, ok := m.generations[id]; ok {
			out[id] = gen
		}
	}
	return out, nil
}
func (m *pushMockAssignments) ListHistory(_ context.Context, _ int64, _, _ time.Time) ([]domain.AssignmentInterval, error) {
	return nil, nil
}

type pushMockActivation struct {
	active  *domain.ProbeActiveConfig
	monitor *domain.Monitor
}

func (m *pushMockActivation) ReadAppliedLocal(context.Context) (*domain.LocalProbeConfigDefinition, error) {
	if m.active == nil {
		return nil, ports.ErrNotFound
	}
	return &domain.LocalProbeConfigDefinition{Revision: m.active.Revision, Assignments: []domain.ProbeConfigAssignment{{Generation: 3, Monitor: m.monitor}}}, nil
}

func (m *pushMockActivation) GetActive(_ context.Context, _ string) (*domain.ProbeActiveConfig, error) {
	if m.active == nil {
		return nil, ports.ErrNotFound
	}
	return m.active, nil
}
func (m *pushMockActivation) GetReceipt(_ context.Context, _ string, _ int64) (*domain.ProbeActiveConfig, error) {
	return nil, ports.ErrNotFound
}
func (m *pushMockActivation) ActivateLocal(_ context.Context, _ ports.LocalActivationParams) (*domain.ProbeActiveConfig, error) {
	return nil, nil
}

type recordingHeartbeatRepo struct {
	ports.HeartbeatRepository
	lastRecorded *domain.Heartbeat
}

func (r *recordingHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.lastRecorded = h
	return nil
}
func (r *recordingHeartbeatRepo) GetLatest(_ context.Context, _ int64) (*domain.Heartbeat, error) {
	return nil, ports.ErrNotFound
}
func (r *recordingHeartbeatRepo) ListByMonitor(_ context.Context, _ int64, _, _ time.Time) ([]*domain.Heartbeat, error) {
	return nil, nil
}

type pushTestBus struct{}

func (s *pushTestBus) Publish(_ context.Context, _ ports.Event) error { return nil }
func (s *pushTestBus) Subscribe(_ string) <-chan ports.Event          { return nil }
func (s *pushTestBus) Close()                                         {}

func TestPushHandler_CapturesAppliedRevisionAndGeneration(t *testing.T) {
	ctx := context.Background()
	monitorRepo := newFakeMonitorRepo()
	token := "valid-push-token-123"
	mon := &domain.Monitor{
		ID:        42,
		Name:      "push-mon",
		Type:      "push",
		Active:    true,
		PushToken: token,
		Interval:  60,
	}
	if err := monitorRepo.Create(ctx, mon); err != nil {
		t.Fatal(err)
	}

	hbRepo := &recordingHeartbeatRepo{}
	bus := &pushTestBus{}
	hbSvc := services.NewHeartbeatService(hbRepo, bus)
	monSvc := services.NewMonitorService(monitorRepo, bus)

	handler := handlers.NewPushHandler(monSvc, hbSvc)
	handler.SetActivationRepo(&pushMockActivation{monitor: mon,
		active: &domain.ProbeActiveConfig{
			ProbeConfigTarget: domain.ProbeConfigTarget{ProbeID: domain.LocalProbeID},
			Revision:          8,
		},
	})
	handler.SetAssignmentRepo(&pushMockAssignments{
		generations: map[int64]int64{mon.ID: 3},
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/push/"+token, strings.NewReader(`{"status":"up","msg":"all good","ping":45}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token")
	c.SetParamValues(token)

	if err := handler.Receive(c); err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	if hbRepo.lastRecorded == nil {
		t.Fatal("expected heartbeat to be recorded")
	}
	if hbRepo.lastRecorded.ConfigRevision != 8 {
		t.Fatalf("expected ConfigRevision 8, got %d", hbRepo.lastRecorded.ConfigRevision)
	}
	if hbRepo.lastRecorded.AssignmentGeneration != 3 {
		t.Fatalf("expected AssignmentGeneration 3, got %d", hbRepo.lastRecorded.AssignmentGeneration)
	}
	if hbRepo.lastRecorded.Ping != 45 {
		t.Fatalf("expected Ping 45, got %d", hbRepo.lastRecorded.Ping)
	}
}
