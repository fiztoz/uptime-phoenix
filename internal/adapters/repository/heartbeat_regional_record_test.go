package repository_test

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type silentBus struct{}

func (silentBus) Publish(context.Context, ports.Event) error { return nil }
func (silentBus) Subscribe(string) <-chan ports.Event        { return make(chan ports.Event) }
func (silentBus) Close()                                     {}

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
			svc.SetRegionalRecorder(f.assignments, f.commits)
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
