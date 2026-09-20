package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
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

type commandTransportDispatcher struct {
	mu          sync.Mutex
	payload     []byte
	claimed     map[int64]bool
	loseReceipt bool
	results     chan domain.ProbeCommandOutcome
}

func (d *commandTransportDispatcher) NextCommand(_ context.Context, s domain.ProbeReplaySession, _ time.Duration, _ domain.ProbeCommandCapabilities) (*domain.ProbeCommandDispatch, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.claimed[s.ConnectionGeneration] {
		return nil, nil
	}
	d.claimed[s.ConnectionGeneration] = true
	return &domain.ProbeCommandDispatch{Payload: bytes.Clone(d.payload)}, nil
}

func (d *commandTransportDispatcher) RecordCommandResult(_ context.Context, _ domain.ProbeReplaySession, r domain.ProbeCommandOutcome) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.results <- r
	if d.loseReceipt {
		return false, errors.New("injected receipt commit loss")
	}
	return false, nil
}

type interruptedEdgeCommand struct {
	*edge.Store
	beforeApply atomic.Bool
}

func (r *interruptedEdgeCommand) ApplyAlertAcknowledgement(ctx context.Context, a domain.EdgeCommandAuthority, c domain.ProbeAlertAcknowledgement) (domain.ProbeCommandOutcome, error) {
	if r.beforeApply.Swap(false) {
		return domain.ProbeCommandOutcome{}, errors.New("injected request interruption")
	}
	return r.Store.ApplyAlertAcknowledgement(ctx, a, c)
}

func TestCommandRuntimeExactBytesReceiptLossAndRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = id.Close() }()
	identity := domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint}
	store, err := edge.Open(t.Context(), dir, identity, edge.WithTelemetryEncoder(EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	hubID := uuid.NewString()
	runtimeToken := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{52}, 32))
	enrollment := servicesNewEdgeEnrollmentForCommandTest(t, store, hubID, runtimeToken)
	if err := store.AcceptConnectionGeneration(t.Context(), hubID, 1); err != nil {
		t.Fatal(err)
	}
	snapshot := m2Config(t)
	snapshot.HubID, snapshot.ProbeID = hubID, id.ProbeID
	configs := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	resolved, err := configs.Apply(t.Context(), configBytes(t, snapshot), 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	a := resolved.Assignments[0]
	at := time.Now().UTC().Truncate(time.Microsecond)
	sourceID := uuid.NewString()
	incident := domain.RegionalIncident{SourceAlertID: sourceID, ProbeID: id.ProbeID, MonitorID: a.Monitor.ID, AssignmentGeneration: a.Generation, ConfigRevision: resolved.Metadata.Revision, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectAvailability, Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: at}
	_, err = store.CommitEdgeCheck(t.Context(), domain.EdgeCheckRecord{Observation: domain.RegionalObservation{ProbeID: id.ProbeID, StreamID: id.StreamID, MonitorID: a.Monitor.ID, AssignmentGeneration: a.Generation, ConfigRevision: resolved.Metadata.Revision, Status: domain.StatusDown, RawStatus: domain.StatusDown, DownCount: 1, Important: true, ObservedAt: at, ReceivedAt: at}, Incident: &incident})
	if err != nil {
		t.Fatal(err)
	}
	ack := domain.ProbeAlertAcknowledgement{CommandID: uuid.NewString(), ProbeID: id.ProbeID, SourceAlertID: sourceID, AssignmentGeneration: a.Generation, ActorDisplayName: "Operator", CreatedAt: at, ExpiresAt: at.Add(4 * time.Second)}
	payload, err := (AcknowledgementCodec{}).EncodeAcknowledgement(t.Context(), ack)
	if err != nil {
		t.Fatal(err)
	}
	// Whitespace inside the value is immutable too. Marshal(RawMessage) would
	// compact this payload, changing its source deduplication identity.
	payload = bytes.Replace(payload, []byte(`,"kind"`), []byte(",\n  \"kind\""), 1)
	if !bytes.Contains(payload, []byte("\n")) {
		t.Fatal("fixture lacks significant byte-layout difference")
	}
	if _, err := encodeCommandRequestFrame(2, payload); err != nil {
		t.Fatalf("exact command framing: %v", err)
	}
	dispatch := &commandTransportDispatcher{payload: payload, claimed: map[int64]bool{}, results: make(chan domain.ProbeCommandOutcome, 4)}
	transport := NewHubTransport(EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	transport.SetCommands(dispatch)
	metadata := domain.ProbeCredentialMetadata{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID, EnrollmentID: enrollment.EnrollmentID, CredentialVersion: 1, Fingerprint: id.Fingerprint}
	start := func(fail bool) (*EdgeRuntime, *httptest.Server) {
		t.Helper()
		configService := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
		r, err := NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
			d, e := store.ReadDiagnostics(ctx)
			return d.Identity, d.FirstRetainedSeq, e
		}, store, configService, EdgeRuntimeConfig{AgentVersion: "command-test", Capabilities: []string{"snapshot.v1", AcknowledgementCapability, "checker.http.v1", "notifier.webhook.v1"}}, func(ctx context.Context) (Health, error) {
			i, e := store.ReadIdentity(ctx)
			healthy, q := true, int64(0)
			return Health{Role: "probe", Ready: true, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: Decimal(i.ConfigRevision), QueueBytes: &q, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, e
		})
		if err != nil {
			t.Fatal(err)
		}
		wrapper := &interruptedEdgeCommand{Store: store}
		wrapper.beforeApply.Store(fail)
		r.SetCommands(wrapper)
		handler, err := NewEdgeHTTPHandler(id, services.NewEdgeEnrollmentService(store), r.Handle, func(context.Context) EdgeReadiness { return EdgeReadiness{} })
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(handler)
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{id.Certificate}}
		server.StartTLS()
		metadata.Endpoint = "wss" + strings.TrimPrefix(server.URL, "https") + "/ws/probe/v1"
		return r, server
	}
	runtime, server := start(true)
	defer func() { _ = runtime.Close(); server.Close() }()
	run := func(generation int64, cancelOnResult bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- transport.Run(ctx, domain.ProbeSessionInput{Connection: metadata, Token: runtimeToken, Generation: generation, OwnerID: uuid.NewString(), ConfigDocument: configBytes(t, snapshot)}, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
				return nil, errors.New("unexpected replay in control-only fixture")
			})
		}()
		if cancelOnResult {
			select {
			case result := <-dispatch.results:
				dispatch.results <- result
				cancel()
			case <-ctx.Done():
				t.Fatal("receipt never arrived")
			}
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("connection did not report close")
			}
		case <-ctx.Done():
			if !cancelOnResult {
				t.Fatal("failure failed to close session")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("command sender did not join")
			}
		}
	}
	run(2, false)
	evidence, err := store.ReadEdgeEvidence(t.Context(), a.Monitor.ID, a.Generation)
	if err != nil || evidence.Incident.Status != "firing" {
		t.Fatal("interrupted request applied", err)
	}
	dispatch.loseReceipt = true
	run(3, false)
	var original domain.ProbeCommandOutcome
	select {
	case original = <-dispatch.results:
	case <-time.After(time.Second):
		t.Fatal("source application produced no result")
	}
	if original.Status != "applied" {
		t.Fatal("source did not apply ACK", original.Status)
	}
	evidence, err = store.ReadEdgeEvidence(t.Context(), a.Monitor.ID, a.Generation)
	if err != nil || evidence.Incident.Status != "acked" || evidence.Incident.TransitionVersion != 2 {
		t.Fatal("source ACK absent after result loss", err)
	}
	_ = runtime.Close()
	server.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = edge.Open(t.Context(), dir, identity, edge.WithTelemetryEncoder(EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	runtime, server = start(false)
	if wait := time.Until(ack.ExpiresAt); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	dispatch.loseReceipt = false
	run(4, true)
	recovered := <-dispatch.results
	if !reflect.DeepEqual(recovered, original) {
		t.Fatalf("lost result not recovered exactly: %+v %+v", original, recovered)
	}
	i, err := store.ReadIdentity(t.Context())
	if err != nil || i.LastCreatedSeq != 3 {
		t.Fatal("retry applied more than one transition", i.LastCreatedSeq, err)
	}
	// Directly using the preserved wire value under the new current generation
	// must find the same durable receipt; a compacted value must conflict.
	decoded, err := (AcknowledgementCodec{}).DecodeAcknowledgement(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	authority := domain.EdgeCommandAuthority{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID, ConnectionGeneration: 4}
	got, err := store.ApplyAlertAcknowledgement(t.Context(), authority, decoded)
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatal("runtime changed exact request bytes", err)
	}
	decoded.PayloadHash = sha256.Sum256(bytes.ReplaceAll(payload, []byte("\n  "), nil))
	if _, err := store.ApplyAlertAcknowledgement(t.Context(), authority, decoded); err == nil {
		t.Fatal("changed bytes reused command UUID")
	}
}

func servicesNewEdgeEnrollmentForCommandTest(t *testing.T, store *edge.Store, hubID, token string) domain.EdgeEnrollment {
	t.Helper()
	s := services.NewEdgeEnrollmentService(store)
	now := time.Now().UTC()
	local, err := s.Issue(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	i, err := store.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.EdgeEnrollment{HubID: hubID, ProbeID: i.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1}
	if err := s.Accept(t.Context(), local, token, binding, now); err != nil {
		t.Fatal(err)
	}
	return binding
}
