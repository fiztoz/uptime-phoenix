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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestEdgeSchedulerExecutesExtendedPullCheckerType proves a monitor type that
// M2 rejected remotely (websocket) now executes through the production edge
// scheduler and commits a durable UP observation, not just a decode.
func TestEdgeSchedulerExecutesExtendedPullCheckerType(t *testing.T) {
	var handshakes atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		handshakes.Add(1)
		_ = conn.Close(websocket.StatusNormalClosure, "check complete")
	}))
	defer target.Close()
	wsURL := "ws" + strings.TrimPrefix(target.URL, "http") + "/socket"

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
	a.Monitor.Type = "websocket"
	a.Monitor.Interval = 1
	a.Monitor.MaxRetries = 0
	a.Monitor.ResendInterval = 0
	a.Monitor.CertExpiryNotify = false
	a.Monitor.Config = json.RawMessage(`{"url":"` + wsURL + `"}`)
	a.RequiredCapabilities = []string{"checker.websocket.v1"}
	a.ProxyBindingKey = nil
	snapshot.MaintenanceWindows[0].Active = false
	document, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	identity := domain.EdgeIdentity{ProbeID: snapshot.ProbeID, StreamID: uuid.NewString(), Fingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
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
	runtimeToken := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := enrollment.Accept(t.Context(), token, runtimeToken, domain.EdgeEnrollment{HubID: snapshot.HubID, ProbeID: identity.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1}, at); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptConnectionGeneration(t.Context(), snapshot.HubID, 1); err != nil {
		t.Fatal(err)
	}
	protector, err := auth.NewProbeConfigProtector([]byte{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3})
	if err != nil {
		t.Fatal(err)
	}
	configs := services.NewEdgeConfigService(store, store, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	if _, err := configs.Apply(t.Context(), document, 1, at); err != nil {
		t.Fatalf("extended pull-checker config not applied: %v", err)
	}
	s := NewEdgeScheduler(configs, services.NewEdgeRecordingService(store, store, CronEvaluator{}), checker.Get, CronEvaluator{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- s.Run(ctx, func(error) {}) }()
	waitAndStop := func() (map[string]any, bool, int64) {
		t.Helper()
		var running bool
		var revision int64
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			batch, err := store.ReadReplayBatch(t.Context(), 1, 16, 512<<10)
			if err == nil && len(batch.Items) > 0 {
				running, revision = s.Diagnostics()
				var event struct {
					Kind string         `json:"kind"`
					Data map[string]any `json:"data"`
				}
				if err := json.Unmarshal(batch.Items[0].Payload, &event); err != nil {
					t.Fatalf("stored telemetry undecodable: %v", err)
				}
				cancel()
				return map[string]any{"kind": event.Kind, "data": event.Data}, running, revision
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("no durable observation from the websocket check")
		return nil, false, 0
	}
	observation, healthy, revision := waitAndStop()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("scheduler did not stop cleanly: %v", err)
	}
	if observation["kind"] != "observation" {
		t.Fatalf("first durable event is not an observation: %v", observation)
	}
	observed := observation["data"].(map[string]any)
	if observed["status"] != "UP" || observed["monitor_id"] != float64(42) {
		t.Fatalf("websocket check did not record UP for monitor 42: %v", observed)
	}
	if !healthy || revision < 1 {
		t.Fatalf("scheduler unhealthy during extended-type execution: %v %d", healthy, revision)
	}
	if handshakes.Load() < 1 {
		t.Fatal("no websocket handshake reached the target server")
	}
}
