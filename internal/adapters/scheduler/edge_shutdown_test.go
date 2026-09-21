package scheduler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestEdgeSchedulerAndProviderQuiescenceCommitInflightWork(t *testing.T) {
	targetEntered, releaseTarget := make(chan struct{}), make(chan struct{})
	providerEntered, releaseProvider := make(chan struct{}), make(chan struct{})
	var targetOnce, providerOnce sync.Once
	var checks, sends atomic.Int64
	var stall atomic.Bool
	stalledCheck := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		if stall.Load() {
			stalledCheck <- struct{}{}
			<-r.Context().Done()
			return
		}
		targetOnce.Do(func() { close(targetEntered) })
		select {
		case <-releaseTarget:
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}))
	defer target.Close()
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		providerOnce.Do(func() { close(providerEntered) })
		select {
		case <-releaseProvider:
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	defer webhook.Close()
	data, err := os.ReadFile("../probe/testdata/v1/valid/config-snapshot-http.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := probe.DecodeConfigSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Watchdog.Enabled = false
	a := &snapshot.Assignments[0]
	a.Monitor.CertExpiryNotify = false
	a.Monitor.Interval = 1
	a.Monitor.MaxRetries = 0
	a.Monitor.ResendInterval = 0
	a.ProxyBindingKey = nil
	a.Monitor.Config = json.RawMessage(`{"url":"` + target.URL + `"}`)
	snapshot.MaintenanceWindows[0].Active = false
	snapshot.NotificationChannels[0].TemplateID = nil
	snapshot.NotificationChannels[0].Config = json.RawMessage(`{"url":"` + webhook.URL + `"}`)
	document, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity := domain.EdgeIdentity{ProbeID: snapshot.ProbeID, StreamID: uuid.NewString(), Fingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	store, err := edge.Open(t.Context(), dir, identity, edge.WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	enrollment := services.NewEdgeEnrollmentService(store)
	at := time.Now().UTC()
	token, err := enrollment.Issue(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	runtimeToken := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if err := enrollment.Accept(t.Context(), token, runtimeToken, domain.EdgeEnrollment{HubID: snapshot.HubID, ProbeID: identity.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1}, at); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptConnectionGeneration(t.Context(), snapshot.HubID, 1); err != nil {
		t.Fatal(err)
	}
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configService := func() *services.EdgeConfigService {
		return services.NewEdgeConfigService(store, store, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	}
	if _, err := configService().Apply(t.Context(), document, 1, at); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cfg := configService()
	s := NewEdgeScheduler(cfg, services.NewEdgeRecordingService(store, store, CronEvaluator{}), checker.Get, CronEvaluator{})
	d := services.NewEdgeDeliveryService(store, cfg, store, CronEvaluator{}, notifier.Get)
	stopChecks, stopDeliveries := make(chan struct{}), make(chan struct{})
	checksDone, deliveriesDone := make(chan error, 1), make(chan struct{})
	go func() { checksDone <- s.RunUntilQuiesced(ctx, stopChecks, nil) }()
	go func() { defer close(deliveriesDone); d.RunUntilQuiesced(ctx, stopDeliveries, identity.ProbeID, nil) }()
	select {
	case <-targetEntered:
	case <-ctx.Done():
		t.Fatal("check did not start")
	}
	close(stopChecks)
	close(releaseTarget)
	select {
	case err := <-checksDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("check did not join")
	}
	progress, err := store.ReadIdentity(ctx)
	if err != nil || progress.LastCreatedSeq < 2 {
		t.Fatal("completed check and incident not committed", progress, err)
	}
	select {
	case <-providerEntered:
	case <-ctx.Done():
		t.Fatal("provider did not start")
	}
	close(stopDeliveries)
	select {
	case <-deliveriesDone:
		t.Fatal("in-flight provider was not joined")
	default:
	}
	close(releaseProvider)
	select {
	case <-deliveriesDone:
	case <-ctx.Done():
		t.Fatal("provider did not finish")
	}
	after, err := store.ReadIdentity(ctx)
	if err != nil || after.LastCreatedSeq != progress.LastCreatedSeq+1 {
		t.Fatal("completed provider outcome not committed", progress, after, err)
	}
	diagnostic, err := store.ReadDiagnostics(ctx)
	if err != nil || diagnostic.PendingDeliveries != 0 || checks.Load() != 1 || sends.Load() != 1 {
		t.Fatal("shutdown repeated work", diagnostic, checks.Load(), sends.Load(), err)
	}
	if healthy, _ := s.Diagnostics(); healthy {
		t.Fatal("quiesced scheduler reported healthy")
	}
	// The supported HTTP adapter must also honor the end of the grace. A
	// canceled check cannot fabricate a DOWN result or start another request.
	stall.Store(true)
	stalledCtx, cancelStalled := context.WithCancel(ctx)
	defer cancelStalled()
	stalled := NewEdgeScheduler(cfg, services.NewEdgeRecordingService(store, store, CronEvaluator{}), checker.Get, CronEvaluator{})
	stopStalled := make(chan struct{})
	stalledDone := make(chan error, 1)
	go func() { stalledDone <- stalled.RunUntilQuiesced(stalledCtx, stopStalled, nil) }()
	select {
	case <-stalledCheck:
	case <-ctx.Done():
		t.Fatal("stalled request never started")
	}
	close(stopStalled)
	cancelStalled()
	select {
	case <-stalledDone:
	case <-time.After(time.Second):
		t.Fatal("supported HTTP check ignored cancellation")
	}
	final, err := store.ReadIdentity(ctx)
	if err != nil || final.LastCreatedSeq != after.LastCreatedSeq || checks.Load() != 2 {
		t.Fatal("canceled check invented durable result or repeated I/O", final, err)
	}
}
