package scheduler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestEdgeSchedulerRealCheckDeliveryAndColdRestart(t *testing.T) {
	var targetDown atomic.Bool
	targetDown.Store(true)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if targetDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer target.Close()
	messages := make(chan map[string]any, 8)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data map[string]any
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		messages <- data
		w.WriteHeader(http.StatusNoContent)
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
	start := func() (context.CancelFunc, <-chan error, <-chan struct{}, *EdgeScheduler) {
		ctx, cancel := context.WithCancel(t.Context())
		cfg := configService()
		s := NewEdgeScheduler(cfg, services.NewEdgeRecordingService(store, store, CronEvaluator{}), checker.Get, CronEvaluator{})
		d := services.NewEdgeDeliveryService(store, cfg, store, CronEvaluator{}, notifier.Get)
		completed := make(chan error, 1)
		deliveryDone := make(chan struct{})
		go func() { completed <- s.Run(ctx, func(err error) { t.Logf("scheduler diagnostic: %v", err) }) }()
		go func() {
			defer close(deliveryDone)
			d.Run(ctx, identity.ProbeID, func(err error) { t.Logf("delivery diagnostic: %v", err) })
		}()
		return cancel, completed, deliveryDone, s
	}
	stop, finished, deliveryDone, s := start()
	defer func() { stop(); <-deliveryDone }()
	waitMessage := func() map[string]any {
		t.Helper()
		select {
		case message := <-messages:
			return message
		case <-time.After(8 * time.Second):
			t.Fatal("no regional provider delivery without a hub")
			return nil
		}
	}
	first := waitMessage()
	if first["status"] != float64(domain.StatusDown) || first["severity"] != "DOWN" {
		t.Fatalf("wrong DOWN payload: %+v", first)
	}
	if healthy, revision := s.Diagnostics(); !healthy || revision != 12 {
		t.Fatalf("offline readiness lost: %v %d", healthy, revision)
	}
	stop()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-deliveryDone
	before, err := store.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.LastCreatedSeq < 2 {
		t.Fatal("recording did not commit durable telemetry")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = edge.Open(t.Context(), dir, identity, edge.WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	targetDown.Store(false)
	stop, finished, deliveryDone, _ = start()
	second := waitMessage()
	if second["status"] != float64(domain.StatusUp) || second["severity"] != "UP" {
		t.Fatalf("wrong recovery payload: %+v", second)
	}
	stop()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-deliveryDone
	after, err := store.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.StreamID != before.StreamID || after.LastCreatedSeq <= before.LastCreatedSeq || after.ConfigRevision != 12 {
		t.Fatalf("restart lost progress: before=%+v after=%+v", before, after)
	}
	evidence, err := store.ReadEdgeEvidence(t.Context(), 42, 3)
	if err != nil || evidence.Incident == nil || evidence.Incident.Status != domain.AlertStatusResolved || evidence.State.Status != domain.StatusUp {
		t.Fatalf("recovery lost: %+v %v", evidence, err)
	}
	select {
	case extra := <-messages:
		t.Fatalf("duplicate delivery: %+v", extra)
	default:
	}
}
