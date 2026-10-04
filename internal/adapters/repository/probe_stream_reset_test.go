package repository_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type streamResetFixture struct {
	commandFixture
	resets      *repository.ProbeStreamResetStore
	connections *repository.ProbeConnectorStore
	connection  domain.ProbeConnection
	issue       domain.ProbeStreamResetIssue
	token       string
}

func newStreamResetFixture(t *testing.T, engine string) streamResetFixture {
	t.Helper()
	f := newCommandFixture(t, engine)
	f.issue(t)
	f.ingest(t, f.batch(f.observation(2)))
	c := repository.NewProbeConnectorStore(f.f.db)
	connection, err := c.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	// The replay-only fixture deliberately has opaque placeholder credentials.
	// Reset must authenticate real ciphertext rather than accommodating that stub.
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32))
	protected, err := f.protector.SealCredential(t.Context(), connection.ProbeCredentialMetadata, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_connections SET protected_credential = ? WHERE probe_id = ?", protected, f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	connection.ProtectedCredential = protected
	return streamResetFixture{commandFixture: f, connections: c, connection: *connection, token: token, resets: repository.NewProbeStreamResetStore(f.f.db, f.protector, f.protector, probe.StreamResetCodec{}), issue: domain.ProbeStreamResetIssue{ResetID: "abababab-abab-4bab-8bab-abababababab", HubID: f.session.HubID, ProbeID: f.session.ProbeID, PreviousStreamID: f.session.StreamID, StreamID: "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd"}}
}

func (f streamResetFixture) prepare(t *testing.T) *domain.ProbeStreamResetOperation {
	t.Helper()
	op, err := f.resets.PrepareStreamReset(t.Context(), f.issue)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (f streamResetFixture) receipt(p domain.ProbeStreamResetPlan) domain.EdgeStreamResetRecord {
	// The restored source is behind the hub. Neither side fabricates lost work.
	return domain.EdgeStreamResetRecord{Plan: p, InitialStreamID: f.issue.PreviousStreamID, Source: domain.EdgeIdentity{ProbeID: p.ProbeID, HubID: p.HubID, StreamID: p.PreviousStreamID, Fingerprint: p.Fingerprint, LastCreatedSeq: 1, CommittedSeq: 0, ConnectionGeneration: p.ConnectionGeneration - 1, ConfigRevision: 1}, State: "applied", ReservedAt: p.PreparedAt, AppliedAt: p.PreparedAt.Add(time.Second), ArchiveBytes: 4096, ArchiveSHA256: strings.Repeat("e", 64)}
}

func TestProbeStreamResetStorage(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, test := range []struct {
				name string
				run  func(*testing.T, streamResetFixture)
			}{
				{"PreservesEvidenceAndRequiresPeer", testStreamResetLifecycle},
				{"RevokesRuntimeAndSessionOwners", testStreamResetFences},
				{"LateActivationRollback", testStreamResetRollback},
				{"ConcurrentImmutablePreparation", testStreamResetConcurrent},
				{"MigrationPreservesAndGuards", testStreamResetMigration},
				{"WrongKeyAndReusedEpoch", testStreamResetWrongAuthority},
				{"PreparationFailurePreservesLease", testStreamResetPreparationRollback},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, newStreamResetFixture(t, engine)) })
			}
		})
	}
}

func testStreamResetLifecycle(t *testing.T, f streamResetFixture) {
	ctx := t.Context()
	op := f.prepare(t)
	if op.State != "prepared" || op.Plan.HubCommittedSeq != 2 || op.Source != nil || op.ConfirmedAt != nil {
		t.Fatalf("preparation fabricated effects: %+v", op)
	}
	if again := f.prepare(t); !reflect.DeepEqual(again, op) {
		t.Fatal("lost preparation output changed plan")
	}
	for _, change := range []func(*domain.ProbeStreamResetIssue){
		func(i *domain.ProbeStreamResetIssue) { i.StreamID = i.HubID },
		func(i *domain.ProbeStreamResetIssue) { i.PreviousStreamID = i.HubID },
		func(i *domain.ProbeStreamResetIssue) { i.ResetID = i.HubID },
	} {
		bad := f.issue
		change(&bad)
		if _, err := f.resets.PrepareStreamReset(ctx, bad); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("changed/overlapping retry accepted: %v", err)
		}
	}
	receipt := f.receipt(op.Plan)
	activated, err := f.resets.ActivateStreamReset(ctx, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if activated.State != "awaiting_peer" || activated.ConfirmedAt != nil {
		t.Fatal("operator receipt claimed peer confirmation")
	}
	if again, err := f.resets.ActivateStreamReset(ctx, receipt); err != nil || !reflect.DeepEqual(again, activated) {
		t.Fatal("lost activation output changed operation", err)
	}
	bad := receipt
	bad.ArchiveSHA256 = strings.Repeat("f", 64)
	if _, err := f.resets.ActivateStreamReset(ctx, bad); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("changed receipt accepted", err)
	}
	current, err := f.connections.GetConnection(ctx, f.session.ProbeID)
	if err != nil || current.StreamID != f.issue.StreamID {
		t.Fatal("new stream missing", err)
	}
	plain, err := f.protector.OpenCredential(ctx, current.ProbeCredentialMetadata, current.ProtectedCredential)
	if err != nil || plain != f.token {
		t.Fatal("original runtime token lost", err)
	}
	if _, err := f.protector.OpenCredential(ctx, f.connection.ProbeCredentialMetadata, current.ProtectedCredential); err == nil {
		t.Fatal("new ciphertext accepted old stream AAD")
	}
	if cursor, err := f.connections.GetConnectionCursor(ctx, current.ProbeID, current.StreamID); err != nil || cursor != 0 {
		t.Fatal("new cursor not zero", err)
	}
	var old struct {
		CommittedSeq int64
		RetiredAt    *time.Time
	}
	if err := f.f.db.NewRaw("SELECT committed_seq, retired_at FROM probe_streams WHERE probe_id = ? AND stream_id = ?", f.session.ProbeID, f.session.StreamID).Scan(ctx, &old); err != nil || old.CommittedSeq != 2 || old.RetiredAt == nil {
		t.Fatal("old cursor not retained/retired", err)
	}
	for table, want := range map[string]int{"probe_telemetry_receipts": 2, "probe_observations": 1, "probe_incidents": 1, "probe_commands": 1, "probe_delivery_intents": 0, "alerts": 0} {
		if n := replayCount(t, f.f, table); n != want {
			t.Fatalf("%s changed: %d", table, n)
		}
	}
	state, err := repository.NewRegionalCommitStore(f.f.db).GetState(ctx, f.monitor, f.session.ProbeID)
	if err != nil || state.UnknownReason != "stream_reset" || state.Status != domain.StatusUnknown || state.StreamID != f.issue.StreamID {
		t.Fatalf("live projection not UNKNOWN: %+v %v", state, err)
	}
	command, err := f.commands.GetProtectedCommand(ctx, f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || !bytes.Equal(command.ProtectedPayload, f.request.ProtectedPayload) || command.StreamID != f.session.StreamID {
		t.Fatal("historical command body/scope changed", err)
	}
	status, err := f.commands.GetCommand(ctx, f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.Status != "canceled" || status.RemoteConfirmed || status.Outcome != nil || status.LocalCancellationCode != "stream_reset_unconfirmed" {
		t.Fatalf("local cancellation claimed remote application: %+v %v", status, err)
	}
	if _, err := f.commands.CreateCommand(ctx, f.request); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("awaiting peer accepted command issuance", err)
	}
	lease, err := f.connections.AcquireConnector(ctx, f.session.ProbeID, probeRegistryID3)
	if err != nil {
		t.Fatal(err)
	}
	newSession := f.session
	newSession.StreamID = f.issue.StreamID
	newSession.ConnectionGeneration = lease.Generation
	if err := f.resets.ConfirmStreamReset(ctx, newSession, current.ProbeCredentialMetadata, strings.Repeat("b", 64)); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("foreign pin confirmed", err)
	}
	if err := f.resets.ConfirmStreamReset(ctx, f.session, f.connection.ProbeCredentialMetadata, f.connection.Fingerprint); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale peer confirmed", err)
	}
	if err := f.commands.ConfirmCredentialConnection(ctx, newSession, current.ProbeCredentialMetadata); err != nil {
		t.Fatal("new epoch failed credential authentication", err)
	}
	if err := f.commands.ConfirmCertificateConnection(ctx, newSession, current.ProbeCredentialMetadata, current.Fingerprint); err != nil {
		t.Fatal("new epoch failed certificate authentication", err)
	}
	if err := f.resets.ConfirmStreamReset(ctx, newSession, current.ProbeCredentialMetadata, current.Fingerprint); err != nil {
		t.Fatal(err)
	}
	complete, err := f.resets.GetStreamReset(ctx, f.issue.HubID, f.issue.ProbeID, f.issue.ResetID)
	if err != nil || complete.State != "complete" || complete.ConfirmedAt == nil {
		t.Fatal("admitted peer not recorded", err)
	}
	if err := f.resets.ConfirmStreamReset(ctx, newSession, current.ProbeCredentialMetadata, current.Fingerprint); err != nil {
		t.Fatal("lost confirmation retry failed", err)
	}
	// New sequence one is independent of retained old sequence one.
	r := f.replayFixture
	r.session = newSession
	// The database clock authorizes live state. Host/VM clock skew must not turn
	// this lifecycle test into the separately tested future-observation case.
	r.at = complete.ConfirmedAt.UTC()
	if out := r.ingest(t, r.batch(r.observation(1))); out.AcceptedCount != 1 || out.CommittedSeq != 1 {
		t.Fatalf("new sequence collided: %+v", out)
	}
	state, err = repository.NewRegionalCommitStore(f.f.db).GetState(ctx, f.monitor, f.session.ProbeID)
	if err != nil || state.UnknownReason != "" || state.Seq != 1 || state.StreamID != f.issue.StreamID {
		t.Fatalf("fresh evidence did not replace barrier: %+v %v", state, err)
	}
	if _, err := f.store.IngestReplayBatch(ctx, f.session, f.batch(f.observation(3)), &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retired session resurrected evidence", err)
	}
}

func testStreamResetFences(t *testing.T, f streamResetFixture) {
	ctx := t.Context()
	old := domain.ProbeConnectorLease{ProbeID: f.session.ProbeID, OwnerID: f.session.OwnerID, Generation: f.session.ConnectionGeneration}
	if err := f.connections.ReleaseConnector(ctx, old); err != nil {
		t.Fatal(err)
	}
	runtime, err := f.connections.AcquireRuntime(ctx, f.session.ProbeID, f.session.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.connections.AcquireRuntimeConnector(ctx, runtime)
	if err != nil {
		t.Fatal(err)
	}
	f.session.ConnectionGeneration = lease.Generation
	op := f.prepare(t)
	if op.Plan.ConnectionGeneration <= lease.Generation {
		t.Fatal("fence did not advance")
	}
	checks := map[string]func() error{
		"runtime acquire": func() error {
			_, err := f.connections.AcquireRuntime(ctx, f.session.ProbeID, f.session.OwnerID)
			return err
		},
		"runtime renew":     func() error { _, err := f.connections.RenewRuntime(ctx, runtime); return err },
		"runtime connector": func() error { _, err := f.connections.AcquireRuntimeConnector(ctx, runtime); return err },
		"legacy connector": func() error {
			_, err := f.connections.AcquireConnector(ctx, f.session.ProbeID, f.session.OwnerID)
			return err
		},
		"connector renew":    func() error { _, err := f.connections.RenewConnector(ctx, lease); return err },
		"connected callback": func() error { return f.connections.SetConnectorConnected(ctx, lease, true) },
		"close callback":     func() error { return f.connections.SetConnectorConnected(ctx, lease, false) },
		"release":            func() error { return f.connections.ReleaseConnector(ctx, lease) },
		"replay": func() error {
			_, err := f.store.IngestReplayBatch(ctx, f.session, f.batch(f.observation(3)), &services.AccessService{})
			return err
		},
		"command": func() error { _, err := f.commands.CreateCommand(ctx, f.request); return err },
		"enrollment activation": func() error {
			return f.connections.ActivateConnection(ctx, f.session.ProbeID, f.connection.EnrollmentID, 1, time.Now().UTC())
		},
	}
	for name, run := range checks {
		if err := run(); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("%s bypassed prepared fence: %v", name, err)
		}
	}
	if list, err := f.connections.ListConnections(ctx); err != nil || len(list) != 0 {
		t.Fatal("prepared reset still discoverable", err)
	}
	if _, err := f.connections.GetConnectionCursor(ctx, f.session.ProbeID, f.session.StreamID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("prepared stream still negotiable", err)
	}
	if _, err := f.resets.ActivateStreamReset(ctx, f.receipt(op.Plan)); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.connections.AcquireRuntime(ctx, f.session.ProbeID, f.session.OwnerID)
	if err != nil || fresh.Epoch <= runtime.Epoch {
		t.Fatal("new runtime cannot recover", err)
	}
	if _, err := f.connections.AcquireRuntimeConnector(ctx, fresh); err != nil {
		t.Fatal("fresh runtime rejected", err)
	}
}

func testStreamResetRollback(t *testing.T, f streamResetFixture) {
	op := f.prepare(t)
	receipt := f.receipt(op.Plan)
	trigger := "CREATE TRIGGER fail_stream_reset BEFORE UPDATE ON probe_stream_resets WHEN NEW.state = 'awaiting_peer' BEGIN SELECT RAISE(ABORT, 'reset fault'); END"
	if f.f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_stream_reset BEFORE UPDATE ON probe_stream_resets FOR EACH ROW BEGIN IF NEW.state = 'awaiting_peer' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'reset fault'; END IF; END"
	}
	if _, err := f.f.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_stream_reset") })
	if _, err := f.resets.ActivateStreamReset(t.Context(), receipt); err == nil {
		t.Fatal("late failed activation reported success")
	}
	current, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || current.StreamID != f.session.StreamID || !bytes.Equal(current.ProtectedCredential, f.connection.ProtectedCredential) {
		t.Fatal("late failure changed binding/ciphertext", err)
	}
	if replayCount(t, f.f, "probe_streams") != 1 || replayCount(t, f.f, "probe_missing_state") != 0 || replayCount(t, f.f, "monitor_probe_state") != 1 {
		t.Fatal("late failure leaked partial projection/stream writes")
	}
	status, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.Status != "pending" {
		t.Fatal("late failure canceled command", err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "DROP TRIGGER fail_stream_reset"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resets.ActivateStreamReset(t.Context(), receipt); err != nil {
		t.Fatal("retry failed", err)
	}
}

func testStreamResetConcurrent(t *testing.T, f streamResetFixture) {
	var wg sync.WaitGroup
	ops := make([]*domain.ProbeStreamResetOperation, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); ops[i], errs[i] = f.resets.PrepareStreamReset(t.Context(), f.issue) }()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || !reflect.DeepEqual(ops[0], ops[1]) {
		t.Fatalf("competing same request diverged: %v", errs)
	}
	if replayCount(t, f.f, "probe_stream_resets") != 1 {
		t.Fatal("duplicate operation")
	}
}

func testStreamResetMigration(t *testing.T, f streamResetFixture) {
	op := f.prepare(t)
	if err := runEngineMigration(t, f.f.db, f.f.engine, "064_probe_stream_reset", "down"); err == nil {
		t.Fatal("downgrade discarded reset reservation")
	}
	if retry := f.prepare(t); !reflect.DeepEqual(retry, op) {
		t.Fatal("failed downgrade changed preparation")
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_stream_resets SET state = 'awaiting_peer', activated_at = ? WHERE reset_id = ?", time.Now().UTC(), f.issue.ResetID); err == nil {
		t.Fatal("SQL NULL receipt bypassed phase constraint")
	}
}

func testStreamResetWrongAuthority(t *testing.T, f streamResetFixture) {
	ctx := t.Context()
	key, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{91}, 32))
	if err != nil {
		t.Fatal(err)
	}
	bad := repository.NewProbeStreamResetStore(f.f.db, key, key, probe.StreamResetCodec{})
	if _, err := bad.PrepareStreamReset(ctx, f.issue); !errors.Is(err, domain.ErrProbeKeyMismatch) {
		t.Fatal("wrong installation key prepared reset", err)
	}
	issue := f.issue
	issue.StreamID = issue.PreviousStreamID
	if _, err := f.resets.PrepareStreamReset(ctx, issue); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("current epoch reused", err)
	}
	if _, err := f.f.db.ExecContext(ctx, "INSERT INTO probe_streams (probe_id,stream_id,committed_seq,created_at,updated_at,retired_at) VALUES (?,?,0,?,?,?)", f.issue.ProbeID, f.issue.StreamID, f.at, f.at, f.at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resets.PrepareStreamReset(ctx, f.issue); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retired epoch reused", err)
	}
	if replayCount(t, f.f, "probe_stream_resets") != 0 {
		t.Fatal("rejected request reserved operation")
	}
	lease := domain.ProbeConnectorLease{ProbeID: f.session.ProbeID, OwnerID: f.session.OwnerID, Generation: f.session.ConnectionGeneration}
	if _, err := f.connections.RenewConnector(ctx, lease); err != nil {
		t.Fatal("rejected request revoked current owner", err)
	}
}

func testStreamResetPreparationRollback(t *testing.T, f streamResetFixture) {
	trigger := "CREATE TRIGGER fail_reset_prepare BEFORE INSERT ON probe_stream_resets BEGIN SELECT RAISE(ABORT, 'reset prepare fault'); END"
	if f.f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_reset_prepare BEFORE INSERT ON probe_stream_resets FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'reset prepare fault'"
	}
	if _, err := f.f.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_reset_prepare") })
	if _, err := f.resets.PrepareStreamReset(t.Context(), f.issue); err == nil {
		t.Fatal("failed preparation reported success")
	}
	lease := domain.ProbeConnectorLease{ProbeID: f.session.ProbeID, OwnerID: f.session.OwnerID, Generation: f.session.ConnectionGeneration}
	if _, err := f.connections.RenewConnector(t.Context(), lease); err != nil {
		t.Fatal("failed preparation leaked lease revocation", err)
	}
	if replayCount(t, f.f, "probe_stream_resets") != 0 {
		t.Fatal("failed preparation left operation")
	}
}

func TestProbeStreamResetRejectsUnresolvedRotations(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		for _, kind := range []string{"credential", "certificate"} {
			t.Run(engine+"/"+kind, func(t *testing.T) {
				f := newStreamResetFixture(t, engine)
				if kind == "credential" {
					s, err := services.NewProbeCredentialRotationService(f.commands, f.connections, f.protector, f.protector, probe.CredentialCommandCodec{})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.Issue(t.Context(), domain.ProbeCredentialRotationIssue{HubID: f.issue.HubID, ProbeID: f.issue.ProbeID, RotationID: f.issue.ResetID, CredentialVersion: 2}); err != nil {
						t.Fatal(err)
					}
				} else {
					s, err := services.NewProbeCertificateRotationService(f.commands, f.connections, f.protector, probe.CertificateCommandCodec{})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.Issue(t.Context(), domain.ProbeCertificateRotationIssue{HubID: f.issue.HubID, ProbeID: f.issue.ProbeID, RotationID: f.issue.ResetID, CertificateVersion: 2, ValidForDays: 365}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.resets.PrepareStreamReset(t.Context(), f.issue); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("uncertain rotation overlapped reset", err)
				}
				if replayCount(t, f.f, "probe_stream_resets") != 0 {
					t.Fatal("rejected rotation left reset operation")
				}
			})
		}
	}
}
