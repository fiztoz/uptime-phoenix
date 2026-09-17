package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type silentBus struct{}

func (silentBus) Publish(context.Context, ports.Event) error { return nil }
func (silentBus) Subscribe(string) <-chan ports.Event        { return make(chan ports.Event) }
func (silentBus) Close()                                     {}

func TestLocalStreamTwoMonitors(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			svc := services.NewHeartbeatService(newEngineHeartbeatRepo(f), silentBus{})
			svc.SetRegionalRecorder(f.assignments, f.localHeartbeat)
			userID := f.user(t)
			for i := range 2 {
				m := &domain.Monitor{UserID: userID, Name: "stream member", Type: "http", Active: true, Interval: 60, Config: map[string]any{}}
				if err := newEngineMonitorRepo(f).Create(ctx, m); err != nil {
					t.Fatal(err)
				}
				if err := svc.Record(ctx, m, ports.CheckResult{Status: domain.StatusUp}); err != nil {
					t.Fatalf("monitor %d could not join local stream: %v", i+1, err)
				}
			}
		})
	}
}

func TestLocalStreamExplicitSequenceControl(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			at := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
			for seq := int64(1); seq <= 2; seq++ {
				id := f.monitor(t)
				if _, err := f.assignments.InitializeLocal(ctx, id); err != nil {
					t.Fatal(err)
				}
				obs := regionalSample(id, domain.LocalProbeID, domain.LocalStreamID, 1, seq, domain.StatusUp, 0, at)
				if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: obs, State: stateFrom(obs)}); err != nil {
					t.Fatalf("distinct sequence on shared stream/timestamp failed: %v", err)
				}
			}
		})
	}
}

func TestHeartbeatRecordWritesLocalRegionalState(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitors := newEngineMonitorRepo(f)
			m := &domain.Monitor{
				UserID: f.user(t), Name: "local-regional", Type: "http", Active: true,
				Interval: 60, Timeout: 5, MaxRetries: 1, Config: map[string]any{},
			}
			if err := monitors.Create(ctx, m); err != nil {
				t.Fatal(err)
			}
			svc := services.NewHeartbeatService(newEngineHeartbeatRepo(f), silentBus{})
			svc.SetRegionalRecorder(f.assignments, f.localHeartbeat)
			if err := svc.Record(ctx, m, ports.CheckResult{Status: domain.StatusUp, Message: "ok"}); err != nil {
				t.Fatal(err)
			}
			state, err := f.commits.GetState(ctx, m.ID, domain.LocalProbeID)
			if err != nil || state.Status != domain.StatusUp || state.Seq != 1 || state.DownCount != 0 {
				t.Fatalf("local state: %+v %v", state, err)
			}
			if err := svc.Record(ctx, m, ports.CheckResult{Status: domain.StatusDown, Message: "timeout"}); err != nil {
				t.Fatal(err)
			}
			state, err = f.commits.GetState(ctx, m.ID, domain.LocalProbeID)
			if err != nil || state.Status != domain.StatusPending || state.Seq != 2 || state.DownCount != 1 {
				t.Fatalf("retry pending: %+v %v", state, err)
			}
		})
	}
}
