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

func assertRemoteRegional(t *testing.T, f probeRegistryFixture, target domain.ProbeConfigTarget, stream string, monitor int64, metadata domain.ProbeConfigMetadata, binding int64) {
	t.Helper()
	var row struct {
		AssignmentRevision int64
		Generation         int64
		SHA256             string
		AlertDelivery      string
	}
	err := f.db.NewSelect().Table("probe_remote_regional_applications").Column("assignment_revision", "generation", "sha256", "alert_delivery").Where("probe_id = ? AND config_revision = ? AND monitor_id = ? AND stream_id = ? AND hub_id = ?", target.ProbeID, metadata.Revision, monitor, stream, target.HubID).Scan(t.Context(), &row)
	if err != nil || row.AssignmentRevision != binding || row.Generation != 1 || row.SHA256 != metadata.SHA256 || row.AlertDelivery != string(domain.AlertDeliveryRegional) {
		t.Fatal("authenticated regional report did not retain exact source-local facts", err)
	}
}

func testAssignmentRevisionSession(t *testing.T, f probeRegistryFixture, mode domain.AlertDelivery, preparations ...bool) {
	prepareEnabled := len(preparations) > 0 && preparations[0]
	roundLifecycle := len(preparations) > 1 && preparations[1]
	operativeLifecycle := len(preparations) > 2 && preparations[2]
	operativeCompleted := false
	rearmInSession := false
	sessionBudget := 45 * time.Second
	if operativeLifecycle {
		sessionBudget = 110 * time.Second
	}
	ctx, cancel := context.WithTimeout(t.Context(), sessionBudget)
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
		caps := []string{"snapshot.v1", "checker.http.v1", "paging.aggregate.v1", probe.RegionalApplicationCapability}
		if prepareEnabled {
			configs.SetDeliveryPreparations(store)
			caps = append(caps, probe.DeliveryPreparationCapability)
		}
		var cutoverExecutor ports.EdgeDeliveryCutoverExecutor
		if operativeLifecycle {
			cutoverExecutor, err = services.NewEdgeDeliveryCutoverExecutor(configs)
			if err != nil {
				t.Fatal(err)
			}
			caps = append(caps, probe.DeliveryCutoverCapability)
		}
		runtime, err := probe.NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
			d, err := store.ReadDiagnostics(ctx)
			return d.Identity, d.FirstRetainedSeq, err
		}, store, configs, probe.EdgeRuntimeConfig{
			AgentVersion: "binding-session", Capabilities: caps, DeliveryCutoverExecutor: cutoverExecutor,
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
	connection := domain.ProbeCredentialMetadata{HubID: target.HubID, ProbeID: target.ProbeID, StreamID: identity.StreamID, EnrollmentID: eid, CredentialVersion: 1, Endpoint: "wss" + strings.TrimPrefix(currentEdge.server.URL, "https") + "/ws/probe/v1", Fingerprint: identity.Fingerprint}
	protectedCredential, err := protector.SealCredential(ctx, connection, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connector.PrepareConnection(ctx, domain.ProbeConnection{ProbeCredentialMetadata: connection, ProtectedCredential: protectedCredential, State: "prepared", PreparedAt: at}); err != nil {
		t.Fatal(err)
	}
	transport := probe.NewHubTransport(probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	transport.SetRegionalApplications(syncStore)
	operativeStore := repository.NewOperativeDeliveryStore(f.db, probe.DeliveryCutoverCodec{}, protector, syncStore, probe.LocalConfigEncoder{})
	if operativeLifecycle {
		transport.SetDeliveryCutover(true)
		transport.SetCutoverCapabilities(operativeStore)
	}
	var preparationResults *preparationReceiptRefuser
	if prepareEnabled {
		syncStore.SetDeliveryPreparations(probe.DeliveryPreparationCodec{}, protector)
		transport.SetDeliveryPreparations(syncStore)
		commandStore := repository.NewProbeCommandStore(f.db, protector, probe.AcknowledgementCodec{}, protector, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
		commands, err := services.NewProbeCommandService(commandStore, connector, protector, probe.AcknowledgementCodec{}, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
		if err != nil {
			t.Fatal(err)
		}
		commands.SetDeliveryPreparationsCodec(probe.DeliveryPreparationCodec{})
		if operativeLifecycle {
			commands.SetDeliveryCutoverCodec(probe.DeliveryCutoverCodec{})
		}
		preparationResults = &preparationReceiptRefuser{dispatcher: commands, refused: make(chan domain.ProbeCommandOutcome, 1)}
		transport.SetCommands(preparationResults)
	}
	var lastReceipt domain.ProbeActiveConfig
	runSession := func(document []byte, refuse bool) {
		t.Helper()
		expectPreparationRefusal := preparationResults != nil && preparationResults.refuse.Load()
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
		var ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)
		if prepareEnabled {
			replay := repository.NewProbeReplayStore(f.db, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
			ingest = func(callbackCtx context.Context, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
				return replay.IngestReplayBatch(callbackCtx, domain.ProbeReplaySession{HubID: target.HubID, ProbeID: target.ProbeID, StreamID: identity.StreamID, OwnerID: lease.OwnerID, ConnectionGeneration: lease.Generation}, batch, &services.AccessService{})
			}
		}
		go func() {
			done <- transport.Run(sctx, input, func(callbackCtx context.Context) error {
				if operativeLifecycle {
					_, err := connector.RenewConnector(callbackCtx, lease)
					return err
				}
				return nil
			}, func(ctx context.Context, receipt domain.ProbeActiveConfig) error {
				if refuse {
					received <- result{receipt: receipt, err: domain.ErrInternal}
					return domain.ErrInternal
				}
				err := syncStore.RecordRemoteApplied(ctx, lease, receipt)
				received <- result{receipt: receipt, err: err}
				return err
			}, ingest)
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
		if !refuse {
			reported := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				var n int
				if err := f.db.NewSelect().Table("probe_remote_regional_applications").ColumnExpr("COUNT(*)").Where("probe_id = ? AND config_revision = ? AND sha256 = ?", target.ProbeID, outcome.receipt.Revision, outcome.receipt.SHA256).Scan(ctx, &n); err == nil && n > 0 {
					reported = true
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if !reported {
				t.Fatal("configuration session did not commit regional application report")
			}
			if prepareEnabled && mode != domain.AlertDeliveryRegional && outcome.receipt.Revision == 2 {
				if expectPreparationRefusal {
					select {
					case rejected := <-preparationResults.refused:
						if rejected.Status != "applied" || rejected.Code != "delivery_prepared" {
							t.Fatalf("source did not persist preparation before refusal: %+v", rejected)
						}
					case err := <-done:
						joined = true
						t.Fatal("transport ended before preparation refusal", err)
					case <-ctx.Done():
						t.Fatal("preparation refusal did not arrive")
					}
				} else {
					completed := false
					deadline := time.Now().Add(4 * time.Second)
					for time.Now().Before(deadline) {
						var n int
						if err := f.db.NewSelect().Table("probe_commands").ColumnExpr("COUNT(*)").Where("kind = ? AND remote_confirmed = ?", "delivery.prepare", true).Scan(ctx, &n); err == nil && n == 1 {
							completed = true
							break
						}
						time.Sleep(25 * time.Millisecond)
					}
					if !completed {
						t.Fatal("preparation result did not commit at hub")
					}
					if operativeLifecycle && !operativeCompleted {
						service, err := services.NewOperativeDeliveryService(operativeStore, operativeStore, operativeStore, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return &reviewSender{}, true })
						if err != nil {
							t.Fatal(err)
						}
						deadline := time.Now().Add(22 * time.Second)
						for {
							if _, err := service.ReconcileBatch(ctx); err != nil {
								t.Fatal("live cutover reconcile", err)
							}
							var phase string
							err := f.db.NewSelect().Table("operative_cutover_state").Column("phase").Where("monitor_id = ?", monitor).Scan(ctx, &phase)
							if err == nil && phase == "active" {
								break
							}
							if time.Now().After(deadline) {
								var diagnostic []struct {
									Kind       string
									Status     string
									ResultCode string
									Attempts   int64
								}
								_ = f.db.NewSelect().Table("probe_commands").Column("kind", "status", "result_code", "attempts").Scan(ctx, &diagnostic)
								t.Fatal("real source cutover did not activate", phase, err, diagnostic)
							}
							time.Sleep(50 * time.Millisecond)
						}
						var applied preparationCommandRow
						if err := f.db.NewSelect().Table("probe_commands").Column("command_id", "kind", "assignment_generation", "hub_id", "probe_id", "stream_id", "payload_sha256", "protected_payload", "created_at", "expires_at").Where("kind = ?", domain.DeliveryApplyKind).Scan(ctx, &applied); err != nil {
							t.Fatal(err)
						}
						metadata := domain.ProbeCommandMetadata{CommandID: applied.CommandID, HubID: applied.HubID, ProbeID: applied.ProbeID, StreamID: applied.StreamID, Kind: applied.Kind, AssignmentGeneration: applied.AssignmentGeneration, CreatedAt: applied.CreatedAt.UTC(), ExpiresAt: applied.ExpiresAt.UTC(), PayloadSHA256: applied.PayloadSHA256}
						plain, err := protector.OpenCommand(ctx, metadata, applied.ProtectedPayload)
						if err != nil {
							t.Fatal(err)
						}
						request, err := (probe.DeliveryCutoverCodec{}).DecodeDeliveryCutover(ctx, plain)
						clear(plain)
						if err != nil {
							t.Fatal(err)
						}
						out, err := currentEdge.configs.ExecuteDeliveryCutover(ctx, domain.EdgeCommandAuthority{HubID: target.HubID, ProbeID: target.ProbeID, StreamID: identity.StreamID, ConnectionGeneration: lease.Generation}, request)
						if err != nil || out.Status != "applied" || out.Code != domain.DeliveryAppliedCode {
							t.Fatal("real source did not retain exact effective application receipt", err)
						}
						draining, err := services.NewOperativeDeliveryService(operativeStore, operativeStore, operativeStore, operativeOwner, false, func(string) (ports.NotificationSender, bool) { return &reviewSender{}, true })
						if err != nil {
							t.Fatal(err)
						}
						deadline = time.Now().Add(22 * time.Second)
						for {
							if _, err := draining.ReconcileBatch(ctx); err != nil {
								t.Fatal("live disable drain", err)
							}
							var phase string
							if err := f.db.NewSelect().Table("operative_cutover_state").Column("phase").Where("monitor_id = ?", monitor).Scan(ctx, &phase); err != nil {
								t.Fatal(err)
							}
							if phase == "released" {
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("real source did not resume after disable", phase)
							}
							time.Sleep(50 * time.Millisecond)
						}
						var resumed int
						if err := f.db.NewSelect().Table("probe_commands").ColumnExpr("COUNT(*)").Where("kind = ? AND remote_confirmed = ? AND result_code = ?", domain.DeliveryResumeKind, true, domain.DeliveryResumedCode).Scan(ctx, &resumed); err != nil || resumed != 1 {
							t.Fatal("disable release lacks real exact source receipt", err, resumed)
						}
						operativeCompleted = true
					}
					if rearmInSession {
						var old preparationCommandRow
						if err := f.db.NewSelect().Table("probe_commands").Column("command_id", "kind", "requested_by", "source_alert_id", "assignment_generation", "hub_id", "probe_id", "stream_id", "payload_sha256", "protected_payload", "status", "remote_confirmed", "attempts", "created_at", "expires_at").Where("kind = ?", "delivery.prepare").Scan(ctx, &old); err != nil {
							t.Fatal(err)
						}
						expireRound(t, f.db, monitor)
						deadline := time.Now().Add(8 * time.Second)
						for {
							var count int
							if err := f.db.NewSelect().Table("probe_commands").ColumnExpr("COUNT(*)").Where("remote_confirmed = ?", true).Scan(ctx, &count); err == nil && count == 2 {
								break
							}
							if time.Now().After(deadline) {
								t.Fatal("live session did not rearm expired round")
							}
							time.Sleep(25 * time.Millisecond)
						}
						if latestRound(t, f.db, monitor).RequestRound != 2 || len(roundRequests(t, f.db, monitor, 2)) != 1 {
							t.Fatal("live preparation did not bind new round")
						}
						var retained preparationCommandRow
						if err := f.db.NewSelect().Table("probe_commands").Column("command_id", "kind", "requested_by", "source_alert_id", "assignment_generation", "hub_id", "probe_id", "stream_id", "payload_sha256", "protected_payload", "status", "remote_confirmed", "attempts", "created_at", "expires_at").Where("command_id = ?", old.CommandID).Scan(ctx, &retained); err != nil {
							t.Fatal(err)
						}
						if !bytes.Equal(old.ProtectedPayload, retained.ProtectedPayload) || !old.CreatedAt.Equal(retained.CreatedAt) || !old.ExpiresAt.Equal(retained.ExpiresAt) || old.Status != retained.Status || !retained.RemoteConfirmed {
							t.Fatal("rearm rewrote old request/result")
						}
						meta := domain.ProbeCommandMetadata{CommandID: old.CommandID, HubID: old.HubID, ProbeID: old.ProbeID, StreamID: old.StreamID, Kind: old.Kind, AssignmentGeneration: old.AssignmentGeneration, CreatedAt: old.CreatedAt.UTC(), ExpiresAt: old.ExpiresAt.UTC(), PayloadSHA256: old.PayloadSHA256}
						plain, err := protector.OpenCommand(ctx, meta, old.ProtectedPayload)
						if err != nil {
							t.Fatal(err)
						}
						request, err := (probe.DeliveryPreparationCodec{}).DecodeDeliveryPreparation(ctx, plain)
						clear(plain)
						if err != nil {
							t.Fatal(err)
						}
						out, err := currentEdge.configs.PrepareDelivery(ctx, domain.EdgeCommandAuthority{HubID: target.HubID, ProbeID: target.ProbeID, StreamID: identity.StreamID, ConnectionGeneration: lease.Generation}, request)
						if err != nil || out.CommandID != old.CommandID || out.Status != "applied" || out.Code != "delivery_prepared" {
							t.Fatal("old source receipt recovery changed round authority", err)
						}
					}
				}
			}
		}
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
		active, err := currentEdge.store.ReadActiveConfig(ctx)
		want := domain.ProbeDeliveryModeApplication{ProbeConfigTarget: target, MonitorID: monitor, AssignmentRevision: binding,
			Generation: loaded.Assignments[0].Generation, ConfigRevision: metadata.Revision, SHA256: metadata.SHA256,
			StreamID: identity.StreamID, AlertDelivery: domain.AlertDeliveryRegional}
		if err != nil || len(active.RegionalDeliveryApplications) != 1 || active.RegionalDeliveryApplications[0] != want {
			t.Fatal("real transfer/cold load did not retain honest effective regional state", err)
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
	assertTableCount(t, f, "probe_remote_regional_applications", 0) // Report cannot precede a committed receipt.
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
	assertRemoteRegional(t, f, target, identity.StreamID, monitor, first, originalBinding)
	runSession(firstDocument, false) // Reconnect repeats exact facts without rewriting provenance.
	assertRemoteRegional(t, f, target, identity.StreamID, monitor, first, originalBinding)
	assertCold(first, originalBinding)
	if prepareEnabled && mode != domain.AlertDeliveryRegional {
		preparationResults.refuse.Store(true)
		runSession(secondDocument, false) // source preparation commits, hub result deliberately refuses
		assertTableCount(t, f, "probe_commands", 1)
		var confirmed int
		if err := f.db.NewSelect().Table("probe_commands").ColumnExpr("COUNT(*)").Where("remote_confirmed = ?", true).Scan(ctx, &confirmed); err != nil || confirmed != 0 {
			t.Fatal("refused preparation result became a hub receipt", err)
		}
		closeEdge(currentEdge)
		currentEdge = openEdge(false) // cold source recovers its exact preparation receipt
	}
	runSession(secondDocument, false)
	assertHub(second, originalBinding+2)
	assertRemoteRegional(t, f, target, identity.StreamID, monitor, second, originalBinding+2)
	assertTableCount(t, f, "probe_remote_regional_applications", 2) // Original document facts remain unchanged.
	assertCold(second, originalBinding+2)
	active, err := currentEdge.store.ReadActiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rearmInSession = roundLifecycle
	runSession(secondDocument, false)
	after, err := currentEdge.store.ReadActiveConfig(ctx)
	if err != nil || !bytes.Equal(active.Snapshot.ProtectedPayload, after.Snapshot.ProtectedPayload) || !active.AppliedAt.Equal(after.AppliedAt) {
		t.Fatal("repeated ACK changed immutable edge activation", err)
	}
}
