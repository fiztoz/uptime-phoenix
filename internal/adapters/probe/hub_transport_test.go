package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestHubTransportRealEnrollmentRuntimeAndConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = id.Close() }()
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	enrollment := services.NewEdgeEnrollmentService(store)
	localToken, err := enrollment.Issue(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configs := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	runtime, err := NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, err := store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, err
	}, store, configs, EdgeRuntimeConfig{AgentVersion: "test", Capabilities: []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1"}}, func(ctx context.Context) (Health, error) {
		i, err := store.ReadIdentity(ctx)
		healthy, queue := true, int64(0)
		return Health{Role: "probe", Ready: i.ConfigRevision > 0, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: Decimal(i.ConfigRevision), QueueBytes: &queue, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	handler, err := NewEdgeHTTPHandler(id, enrollment, runtime.Handle, func(context.Context) EdgeReadiness { return EdgeReadiness{} })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{id.Certificate}}
	server.StartTLS()
	defer server.Close()
	m := domain.ProbeCredentialMetadata{HubID: uuid.NewString(), ProbeID: id.ProbeID, StreamID: id.StreamID, EnrollmentID: uuid.NewString(), CredentialVersion: 1, Endpoint: "wss" + strings.TrimPrefix(server.URL, "https") + "/ws/probe/v1", Fingerprint: id.Fingerprint}
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32))
	transport := NewHubTransport(EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	wrong := m
	wrong.Fingerprint = strings.Repeat("0", 64)
	if err := transport.Enroll(t.Context(), wrong, localToken, token); err == nil {
		t.Fatal("wrong pin enrolled")
	}
	if err := transport.Enroll(t.Context(), m, localToken, token); err != nil {
		t.Fatal(err)
	}
	if err := transport.Enroll(t.Context(), m, localToken, token); err == nil {
		t.Fatal("replayed enrollment accepted")
	}
	snapshot := m2Config(t)
	snapshot.HubID, snapshot.ProbeID = m.HubID, m.ProbeID
	document := configBytes(t, snapshot)
	in := domain.ProbeSessionInput{Connection: m, Token: token, Generation: 1, ConfigDocument: document}
	wrongInput := in
	wrongInput.Token = "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := transport.Run(t.Context(), wrongInput, func(context.Context) error { t.Fatal("wrong token established"); return nil }, func(context.Context, domain.ProbeActiveConfig) error { t.Fatal("wrong token applied"); return nil }, nil); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("runtime authentication failed open")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var established atomic.Int64
	done := make(chan error, 1)
	receipts := make(chan domain.ProbeActiveConfig, 1)
	go func() {
		done <- transport.Run(ctx, in, func(context.Context) error { established.Add(1); return nil }, func(_ context.Context, receipt domain.ProbeActiveConfig) error { receipts <- receipt; return nil }, nil)
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		progress, err := store.ReadIdentity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if progress.ConfigRevision == int64(snapshot.Revision) {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("runtime ended before config: %v", err)
		case <-ctx.Done():
			t.Fatal("configuration never activated")
		case <-ticker.C:
		}
	}
	if established.Load() != 1 {
		t.Fatal("first validated health did not establish exactly once")
	}
	active, err := store.ReadActiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protector.Open(ctx, active.Snapshot.ProbeConfigMetadata, active.Snapshot.ProtectedPayload)
	if err != nil || !bytes.Equal(plain, document) {
		t.Fatal("transport changed exact configuration bytes")
	}
	select {
	case receipt := <-receipts:
		if receipt.ProbeID != m.ProbeID || receipt.HubID != m.HubID || receipt.Revision != int64(snapshot.Revision) || receipt.SHA256 != active.Snapshot.SHA256 || receipt.AssignmentCount != len(snapshot.Assignments) || receipt.AppliedAt.IsZero() {
			t.Fatal("transport callback lost validated application identity")
		}
	case <-ctx.Done():
		t.Fatal("durable application did not produce a validated receipt callback")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("transport did not join after cancellation")
	}
	// A receipt-store failure must end this connection. Repeating the same
	// snapshot under a newer fence recovers the lost receipt without reapplying it.
	in.Generation++
	receiptCtx, stopReceipt := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopReceipt()
	var rejected atomic.Bool
	err = transport.Run(receiptCtx, in, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error {
		rejected.Store(true)
		return errors.New("private receipt database failure")
	}, nil)
	if err == nil || receiptCtx.Err() != nil || !rejected.Load() || strings.Contains(err.Error(), "private") {
		t.Fatal("receipt storage failure did not close the session with a redacted error", err)
	}
	// Even a stalled receipt commit cannot keep a healthy transport pending
	// forever. Shorten the internal deadline; the public protocol remains 60s.
	in.Generation++
	transport.receiptTimeout = 200 * time.Millisecond
	deadlineCtx, stopDeadline := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopDeadline()
	var entered atomic.Bool
	err = transport.Run(deadlineCtx, in, func(context.Context) error { return nil }, func(ctx context.Context, _ domain.ProbeActiveConfig) error {
		entered.Store(true)
		<-ctx.Done()
		return ctx.Err()
	}, nil)
	if err == nil || deadlineCtx.Err() != nil || !entered.Load() {
		t.Fatal("missing durable receipt did not reach its own bounded deadline", err)
	}
}
