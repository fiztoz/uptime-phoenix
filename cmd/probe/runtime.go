package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
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

func serveEdge(ctx context.Context, cfg edgeOptions, identity *probe.RuntimeIdentity, store *edge.Store, configs *services.EdgeConfigService, enrollment *services.EdgeEnrollmentService, tlsManager *probe.EdgeTLSManager) error {
	// Termination first quiesces producers; established transport must remain
	// alive long enough to receive ACKs for their last durable prefix.
	sessionCtx, stopSessions := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSessions()
	producerCtx, stopProducers := context.WithCancel(sessionCtx)
	defer stopProducers()
	backgroundCtx, stopBackground := context.WithCancel(sessionCtx)
	defer stopBackground()
	quiesce := make(chan struct{})
	var stopping atomic.Bool
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
		return edgeHealth(state, writable, healthy, revision, stopping.Load()), nil
	}
	state := func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, err := store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, err
	}
	capabilities := []string{"snapshot.v1", "watchdog.v1", probe.AcknowledgementCapability, probe.CredentialRotationCapability, probe.CertificateRotationCapability, "checker.http.v1", "checker.tcp.v1", "checker.dns.v1"}
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
	runtime.SetCommands(store)
	runtime.SetCredentialCommands(store)
	runtime.SetCertificateCommands(tlsManager)
	runtime.SetWatchdog(watchdog)
	defer func() { _ = runtime.Close() }()
	handler, err := probe.NewManagedEdgeHTTPHandler(identity, enrollment, runtime.Handle, func(ctx context.Context) probe.EdgeReadiness {
		h, err := diagnostic(ctx)
		if err != nil {
			return probe.EdgeReadiness{}
		}
		return probe.EdgeReadiness{Ready: h.Ready, DBWritable: h.DBWritable, SchedulerHealthy: *h.SchedulerHealthy, ConfigRevision: h.ConfigRevision}
	}, tlsManager)
	if err != nil {
		return err
	}
	// Track hijacked enrollment requests too: http.Server.Shutdown does not join
	// WebSocket handlers, and they must release storage before serveEdge returns.
	var admission sync.Mutex
	var requests sync.WaitGroup
	admitted := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		admission.Lock()
		if stopping.Load() {
			admission.Unlock()
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		requests.Add(1)
		admission.Unlock()
		defer requests.Done()
		if req.URL.Path != "/ws/probe/v1" {
			requestCtx, cancel := context.WithCancel(req.Context())
			stop := context.AfterFunc(backgroundCtx, cancel)
			defer func() { stop(); cancel() }()
			req = req.WithContext(requestCtx)
		}
		handler.ServeHTTP(w, req)
	})
	server := &http.Server{Addr: cfg.Listen, Handler: admitted, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: tlsManager.TLSConfig(), ConnContext: tlsManager.ConnContext, BaseContext: func(net.Listener) context.Context { return sessionCtx }}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("probe listener unavailable")
	}
	var workers sync.WaitGroup
	workers.Add(4)
	serverDone := make(chan struct{})
	failures := make(chan error, 3)
	report := func(error) { slog.Warn("probe local operation failed; retry pending") }
	go func() { defer workers.Done(); failures <- schedule.RunUntilQuiesced(producerCtx, quiesce, report) }()
	go func() {
		defer workers.Done()
		delivery.RunUntilQuiesced(producerCtx, quiesce, identity.ProbeID, report)
	}()
	go func() { defer workers.Done(); failures <- watchdog.Run(backgroundCtx, report) }()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			opCtx, done := context.WithTimeout(backgroundCtx, 10*time.Second)
			if sweepErr := store.SweepRetention(opCtx, time.Now().UTC()); sweepErr != nil && backgroundCtx.Err() == nil {
				report(sweepErr)
			}
			done()
			select {
			case <-backgroundCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() { defer close(serverDone); failures <- server.ServeTLS(listener, "", "") }()
	slog.Info("probe runtime listening", "probe_id", identity.ProbeID, "address", listener.Addr().String())
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-failures:
	}
	admission.Lock()
	stopping.Store(true)
	admission.Unlock()
	close(quiesce)
	stopBackground()
	_ = listener.Close()
	grace := time.AfterFunc(10*time.Second, stopProducers)
	runtime.Quiesce()
	workers.Wait()
	grace.Stop()
	stopProducers()
	drainCtx, stopDrain := context.WithTimeout(context.Background(), 5*time.Second)
	drain, drainErr := runtime.WaitForReplay(drainCtx)
	stopDrain()
	if drainErr != nil {
		slog.Warn("probe shutdown retains unacknowledged telemetry", "target_seq", drain.TargetSeq, "committed_seq", drain.CommittedSeq)
	} else {
		slog.Info("probe shutdown telemetry flushed", "target_seq", drain.TargetSeq, "committed_seq", drain.CommittedSeq)
	}
	stopSessions()
	_ = runtime.Close()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
	}
	<-serverDone
	requests.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
