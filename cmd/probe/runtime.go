package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/scheduler"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func serveEdge(ctx context.Context, cfg edgeOptions, identity *probe.RuntimeIdentity, store *edge.Store, configs *services.EdgeConfigService, enrollment *services.EdgeEnrollmentService) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cron := scheduler.CronEvaluator{}
	recording := services.NewEdgeRecordingService(store, store, cron)
	schedule := scheduler.NewEdgeScheduler(configs, recording, checker.Get, cron)
	delivery := services.NewEdgeDeliveryService(store, configs, store, cron, notifier.Get)
	watchdogAuthority := func(ctx context.Context) (domain.ProbeWatchdogAuthority, error) {
		i, err := store.ReadIdentity(ctx)
		if err != nil {
			return domain.ProbeWatchdogAuthority{}, err
		}
		if i.HubID == "" {
			return domain.ProbeWatchdogAuthority{}, ports.ErrNotFound
		}
		return domain.ProbeWatchdogAuthority{HubID: i.HubID, ProbeID: i.ProbeID, StreamID: i.StreamID}, nil
	}
	watchdog, err := services.NewProbeWatchdogRuntime(store, configs, watchdogAuthority, "hub")
	if err != nil {
		return err
	}
	watchdogDelivery, err := services.NewProbeWatchdogDeliveryService(store, configs, store, watchdogAuthority, notifier.Get)
	if err != nil {
		return err
	}
	delivery.SetWatchdog(watchdogDelivery)
	diagnostic := func(ctx context.Context) (probe.Health, error) {
		state, err := store.ReadDiagnostics(ctx)
		if err != nil {
			return probe.Health{}, err
		}
		writable := store.CheckWritable(ctx) == nil
		healthy, revision := schedule.Diagnostics()
		codes := []string{}
		if !writable {
			codes = append(codes, "storage_unavailable")
		}
		if !healthy {
			codes = append(codes, "scheduler_unavailable")
		}
		if state.QueuePressure {
			codes = append(codes, "queue_pressure")
		}
		if state.GapRanges > 0 {
			codes = append(codes, "telemetry_gap")
		}
		var oldest *probe.Timestamp
		if state.OldestQueuedAt != nil {
			at := probe.Timestamp(*state.OldestQueuedAt)
			oldest = &at
		}
		return probe.Health{Role: "probe", Ready: writable && healthy && revision > 0, DBWritable: writable, SchedulerHealthy: &healthy, ConfigRevision: probe.Decimal(revision), CommittedSeq: probe.Decimal(state.Identity.CommittedSeq), QueueBytes: &state.QueueBytes, OldestQueuedAt: oldest, ClockTime: probe.Timestamp(time.Now().UTC()), Errors: codes}, nil
	}
	state := func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, err := store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, err
	}
	capabilities := []string{"snapshot.v1", "checker.http.v1", "checker.tcp.v1", "checker.dns.v1"}
	for _, name := range []string{"telegram", "discord", "slack", "smtp", "webhook", "teams", "mattermost", "gotify", "bark", "feishu", "line"} {
		if _, ok := notifier.Get(name); ok {
			capabilities = append(capabilities, "notifier."+name+".v1")
		}
	}
	runtime, err := probe.NewEdgeRuntime(state, store, configs, probe.EdgeRuntimeConfig{AgentVersion: "phoenix-m3", Capabilities: capabilities}, diagnostic)
	if err != nil {
		return err
	}
	runtime.SetReplayRepository(store)
	runtime.SetStateRepository(store)
	runtime.SetWatchdog(watchdog)
	defer func() { _ = runtime.Close() }()
	handler, err := probe.NewEdgeHTTPHandler(identity, enrollment, runtime.Handle, func(ctx context.Context) probe.EdgeReadiness {
		h, err := diagnostic(ctx)
		if err != nil {
			return probe.EdgeReadiness{}
		}
		return probe.EdgeReadiness{Ready: h.Ready, DBWritable: h.DBWritable, SchedulerHealthy: *h.SchedulerHealthy, ConfigRevision: h.ConfigRevision}
	})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity.Certificate}}, BaseContext: func(net.Listener) context.Context { return runCtx }}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("probe listener unavailable")
	}
	var workers sync.WaitGroup
	workers.Add(5)
	failures := make(chan error, 3)
	report := func(error) { slog.Warn("probe local operation failed; retry pending") }
	go func() { defer workers.Done(); failures <- schedule.Run(runCtx, report) }()
	go func() { defer workers.Done(); delivery.Run(runCtx, identity.ProbeID, report) }()
	go func() { defer workers.Done(); failures <- watchdog.Run(runCtx, report) }()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			opCtx, done := context.WithTimeout(runCtx, 10*time.Second)
			if sweepErr := store.SweepRetention(opCtx, time.Now().UTC()); sweepErr != nil && runCtx.Err() == nil {
				report(sweepErr)
			}
			done()
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() { defer workers.Done(); failures <- server.ServeTLS(listener, "", "") }()
	slog.Info("probe runtime listening", "probe_id", identity.ProbeID, "address", listener.Addr().String())
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-failures:
	}
	cancel()
	_ = runtime.Close()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
	}
	workers.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
