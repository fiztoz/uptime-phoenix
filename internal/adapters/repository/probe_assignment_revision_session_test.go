package repository_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestAssignmentRevisionSessionContract crosses the real pinned TLS/WebSocket
// transports and encrypted stores, including a refused durable hub receipt.
func TestAssignmentRevisionSessionContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) {
					testAssignmentRevisionSession(t, newProbeRegistryFixture(t, engine), mode)
				})
			}
		})
	}
}

func testAssignmentRevisionSession(t *testing.T, f probeRegistryFixture, mode domain.AlertDelivery) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rid, err := probe.InitializeRuntimeIdentity(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rid.Close() }()
	identity := domain.EdgeIdentity{ProbeID: rid.ProbeID, StreamID: rid.StreamID, Fingerprint: rid.Fingerprint}
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if _, err := repository.NewProbeInstallationStore(f.db).Initialize(ctx, domain.ProbeInstallation{
		HubID: probeRegistryID2, KeyHash: protector.KeyHash(probeRegistryID2), ProtocolFloor: 1, AuthorityEpoch: 1, CreatedAt: at, UpdatedAt: at,
	}, nil); err != nil {
		t.Fatal(err)
	}
	f.remote(t, identity.ProbeID, "binding-session")
	monitor := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monitor); err != nil {
		t.Fatal(err)
	}
	assignments := repository.NewProbeAssignmentStore(f.db)
	if _, err := assignments.ReplaceWithBindings(ctx, monitor, 1, []string{identity.ProbeID}, domain.HealthPolicyAnyDown, mode, nil); err != nil {
		t.Fatal(err)
	}
	const originalBinding int64 = 9007199254740993
	if _, err := f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision = ? WHERE monitor_id = ?", originalBinding, monitor); err != nil {
		t.Fatal(err)
	}
	target := domain.ProbeConfigTarget{HubID: probeRegistryID2, ProbeID: identity.ProbeID}
	syncStore := repository.NewRemoteProbeConfigSyncStore(f.db, probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)
	first, err := syncStore.RefreshRemote(ctx, target, at)
	if err != nil || first.Revision != 1 {
		t.Fatal("first real publication failed", err)
	}
	openDocument := func(metadata domain.ProbeConfigMetadata) []byte {
		t.Helper()
		snapshot, err := repository.NewProbeConfigStore(f.db).Latest(ctx, target.ProbeID)
		if err != nil || !domain.SameProbeConfigMetadata(snapshot.ProbeConfigMetadata, metadata) {
			t.Fatal("prepared snapshot identity changed", err)
		}
		plain, err := protector.Open(ctx, metadata, snapshot.ProtectedPayload)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}
	firstDocument := openDocument(first)
	defer clear(firstDocument)
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	tok := "phx_probe_" + base64.RawURLEncoding.EncodeToString(secret[:])
	clear(secret[:])
	eid := uuid.NewString()

	type liveEdge struct {
		store   *edge.Store
		configs *services.EdgeConfigService
		runtime *probe.EdgeRuntime
		server  *httptest.Server
	}
	closeEdge := func(e liveEdge) {
		t.Helper()
		if err := e.runtime.Close(); err != nil {
			t.Error(err)
		}
		e.server.Close()
		if err := e.store.Close(); err != nil {
			t.Error(err)
		}
	}
	openEdge := func(enroll bool) liveEdge {
		t.Helper()
		store, err := edge.Open(ctx, dir, identity)
		if err != nil {
			t.Fatal(err)
		}
		enrollment := services.NewEdgeEnrollmentService(store)
		if enroll {
			boot, err := enrollment.Issue(ctx, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			if err := enrollment.Accept(ctx, boot, tok, domain.EdgeEnrollment{HubID: target.HubID, ProbeID: target.ProbeID, EnrollmentID: eid, CredentialVersion: 1}, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}
		configs := services.NewEdgeConfigService(store, store, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
		runtime, err := probe.NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
			d, err := store.ReadDiagnostics(ctx)
			return d.Identity, d.FirstRetainedSeq, err
		}, store, configs, probe.EdgeRuntimeConfig{
			AgentVersion: "binding-session", Capabilities: []string{"snapshot.v1", "checker.http.v1", "paging.aggregate.v1"},
		}, func(ctx context.Context) (probe.Health, error) {
			i, err := store.ReadIdentity(ctx)
			healthy, queue := true, int64(0)
			return probe.Health{Role: "probe", Ready: i.ConfigRevision > 0, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: probe.Decimal(i.ConfigRevision), QueueBytes: &queue, ClockTime: probe.Timestamp(time.Now().UTC()), Errors: []string{}}, err
		})
		if err != nil {
			t.Fatal(err)
		}
		handler, err := probe.NewEdgeHTTPHandler(rid, enrollment, runtime.Handle, func(context.Context) probe.EdgeReadiness { return probe.EdgeReadiness{} })
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(handler)
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{rid.Certificate}}
		server.StartTLS()
		return liveEdge{store: store, configs: configs, runtime: runtime, server: server}
	}
	currentEdge := openEdge(true)
	defer func() { closeEdge(currentEdge) }()
	connector := repository.NewProbeConnectorStore(f.db)
	transport := probe.NewHubTransport(probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	var lastReceipt domain.ProbeActiveConfig
	runSession := func(document []byte, refuse bool) {
		t.Helper()
		lease, err := connector.AcquireConnector(ctx, target.ProbeID, probeRegistryID3)
		if err != nil {
			t.Fatal(err)
		}
		sctx, stop := context.WithCancel(ctx)
		defer stop()
		input := domain.ProbeSessionInput{OwnerID: lease.OwnerID, Generation: lease.Generation, Token: tok, ConfigDocument: document,
			Connection: domain.ProbeCredentialMetadata{HubID: target.HubID, ProbeID: target.ProbeID, StreamID: identity.StreamID, EnrollmentID: eid, CredentialVersion: 1, Endpoint: "wss" + strings.TrimPrefix(currentEdge.server.URL, "https") + "/ws/probe/v1", Fingerprint: identity.Fingerprint},
		}
		type result struct {
			receipt domain.ProbeActiveConfig
			err     error
		}
		received := make(chan result, 1)
		done := make(chan error, 1)
		go func() {
			done <- transport.Run(sctx, input, func(context.Context) error { return nil }, func(ctx context.Context, receipt domain.ProbeActiveConfig) error {
				if refuse {
					received <- result{receipt: receipt, err: domain.ErrInternal}
					return domain.ErrInternal
				}
				err := syncStore.RecordRemoteApplied(ctx, lease, receipt)
				received <- result{receipt: receipt, err: err}
				return err
			}, nil)
		}()
		joined := false
		defer func() {
			stop()
			if !joined {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("configuration session did not join")
				}
			}
		}()
		var outcome result
		select {
		case outcome = <-received:
		case err := <-done:
			joined = true
			// A refused receipt can publish its observation and end the
			// transport before this goroutine is scheduled. Both are valid.
			select {
			case outcome = <-received:
			default:
				t.Fatal("transport ended before configuration receipt", err)
			}
		case <-ctx.Done():
			t.Fatal("configuration receipt did not arrive")
		}
		if !refuse && outcome.err != nil {
			t.Fatal("real fenced hub receipt failed", outcome.err)
		}
		lastReceipt = outcome.receipt
		stop()
		if !joined {
			select {
			case <-done:
				joined = true
			case <-time.After(3 * time.Second):
				t.Fatal("configuration session did not join")
			}
		}
		if err := connector.ReleaseConnector(ctx, lease); err != nil {
			t.Fatal(err)
		}
	}
	assertCold := func(metadata domain.ProbeConfigMetadata, binding int64) {
		t.Helper()
		loaded, err := currentEdge.configs.Load(ctx)
		if err != nil || !domain.SameProbeConfigMetadata(loaded.Metadata, metadata) || len(loaded.Assignments) != 1 || loaded.Assignments[0].AssignmentRevision != binding || loaded.Assignments[0].DesiredAlertDelivery != mode {
			t.Fatal("wire/cold load changed document binding or desired mode", err)
		}
	}
	assertHub := func(metadata domain.ProbeConfigMetadata, binding int64) {
		t.Helper()
		got, err := syncStore.ReadAcknowledgedRemoteDelivery(ctx, target)
		if err != nil || got == nil || got.Revision != metadata.Revision || len(got.Assignments) != 1 || got.Assignments[0].AssignmentRevision != binding || got.Assignments[0].AlertDelivery != mode {
			t.Fatal("hub receipt substituted current desired data", err)
		}
		if mode != domain.AlertDeliveryRegional {
			set, err := f.assignments.GetByMonitorID(ctx, monitor)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			fact := got.Assignments[0]
			decision, err := services.EvaluateDeliveryCutover(domain.DeliveryCutoverInput{HubID: target.HubID, Now: now, SnapshotAt: now, Current: true, Assignments: *set,
				Sources: []domain.DeliveryCutoverSource{{ProbeID: target.ProbeID, StreamID: identity.StreamID, ModeApplicationSupported: true, AcknowledgedConfig: &lastReceipt, Assignment: &fact}},
			})
			// Even hypothetical stronger support and a real exact document ACK
			// cannot substitute for separate runtime-mode application evidence.
			want := domain.DeliveryCutoverApplicationUnreported
			if binding != set.Revision {
				want = domain.DeliveryCutoverAssignmentRevisionMismatch
			}
			if err != nil || decision.State != domain.DeliveryCutoverPending || len(decision.Sources) != 1 || decision.Sources[0].Reason != want {
				t.Fatal("document ACK became mode authority or failed an unrelated precondition", err)
			}
		}
	}

	// Edge persistence succeeds; the hub deliberately refuses the first receipt.
	runSession(firstDocument, true)
	assertCold(first, originalBinding)
	if _, err := syncStore.ReadAppliedRemote(ctx, target); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("refused receipt became durable hub acknowledgement", err)
	}
	closeEdge(currentEdge)
	currentEdge = openEdge(false)
	assertCold(first, originalBinding)
	// Same source generation/mode/policy after ABA: the binding alone changes.
	if _, err := assignments.ReplaceWithBindings(ctx, monitor, originalBinding, []string{target.ProbeID}, domain.HealthPolicyAllDown, mode, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := assignments.ReplaceWithBindings(ctx, monitor, originalBinding+1, []string{target.ProbeID}, domain.HealthPolicyAnyDown, mode, nil); err != nil {
		t.Fatal(err)
	}
	second, err := syncStore.RefreshRemote(ctx, target, time.Now().UTC())
	if err != nil || second.Revision != 2 || second.SHA256 == first.SHA256 {
		t.Fatal("ABA binding did not change snapshot", err)
	}
	secondDocument := openDocument(second)
	defer clear(secondDocument)
	// Retry the original exact document after desired state and preparation moved.
	runSession(firstDocument, false)
	assertHub(first, originalBinding)
	assertCold(first, originalBinding)
	runSession(secondDocument, false)
	assertHub(second, originalBinding+2)
	assertCold(second, originalBinding+2)
	active, err := currentEdge.store.ReadActiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runSession(secondDocument, false)
	after, err := currentEdge.store.ReadActiveConfig(ctx)
	if err != nil || !bytes.Equal(active.Snapshot.ProtectedPayload, after.Snapshot.ProtectedPayload) || !active.AppliedAt.Equal(after.AppliedAt) {
		t.Fatal("repeated ACK changed immutable edge activation", err)
	}
}
