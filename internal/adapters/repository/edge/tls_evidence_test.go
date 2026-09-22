package edge

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestEdgeTLSCheckSurvivesRestartAndPruning(t *testing.T) {
	s, dir := setupEdgeDeliveryStore(t)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	monitor := &domain.Monitor{ID: 17, Type: "http", Active: true, Timeout: 3, Config: map[string]any{"url": target.URL, "tls_ignore": true}}
	c, ok := checker.Get("http")
	if !ok {
		t.Fatal("HTTP checker missing")
	}
	result, err := c.Check(t.Context(), monitor.Config)
	if err != nil || result.Status != domain.StatusUp {
		t.Fatal("real HTTPS fixture failed", result.Message)
	}
	id := testIdentity()
	config := &domain.EdgeResolvedConfig{Metadata: domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: testHubID, ProbeID: id.ProbeID}, Revision: 1}}
	assignment := domain.EdgeResolvedAssignment{Monitor: monitor, Generation: 1}
	observation, err := services.NewEdgeRecordingService(s, s, nil).Record(t.Context(), config, assignment, result, time.Now())
	if err != nil || observation.TLS == nil || !observation.TLS.NotAfter.Equal(target.Certificate().NotAfter) {
		t.Fatalf("source lost real checker certificate: %+v %v", observation.TLS, err)
	}
	exact, err := (probe.EdgeTelemetryEncoder{}).EncodeObservation(observation)
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadReplayBatch(t.Context(), 0, 256, defaultMaxBatchBytes)
	if err != nil || len(before.Items) != 1 || !bytes.Equal(before.Items[0].Payload, exact) {
		t.Fatal("TLS source event missing", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, testIdentity(), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	after, err := s.ReadReplayBatch(t.Context(), 0, 256, defaultMaxBatchBytes)
	if err != nil || len(after.Items) != 1 || !bytes.Equal(after.Items[0].Payload, exact) {
		t.Fatal("restart changed certificate history", err)
	}
	if err := s.CommitReplayACK(t.Context(), validFence(), domain.ProbeReplayResult{StreamID: id.StreamID, CommittedSeq: 1, AcceptedCount: 1}); err != nil {
		t.Fatal(err)
	}
	current, err := s.ReadCurrentSnapshot(t.Context(), validFence(), time.Now().UTC())
	if err != nil || len(current.States) != 1 || !bytes.Equal(current.States[0].Payload, exact) {
		t.Fatal("history pruning removed current TLS", err)
	}
	var sends int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox").Scan(t.Context(), &sends); err != nil || sends != 0 {
		t.Fatal("TLS metadata created provider work", err)
	}
}
