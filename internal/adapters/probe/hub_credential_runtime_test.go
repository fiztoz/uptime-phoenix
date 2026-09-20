package probe

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// This connects both durable stores through the production TLS transport. The
// only fault wrapper loses a reply AFTER the edge activation transaction commits.
func TestHubCredentialRuntimeLostResultAcrossBothStoresRestart(t *testing.T) {
	f := newCredentialRuntimeFixture(t)
	dsn := "file:" + filepath.Join(t.TempDir(), "hub.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
	db, err := sqlite.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := repository.RunMigrations(db.DB, "sqlite"); err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{81}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services.NewProbeInstallationService(repository.NewProbeInstallationStore(db)).InitializeOrVerify(t.Context(), key, f.hubID); err != nil {
		t.Fatal(err)
	}
	registration := &domain.Probe{ID: f.id.ProbeID, Key: "credential-edge", Name: "Credential Edge", Kind: domain.ProbeKindRemote, Enabled: true}
	if err := repository.NewProbeRegistryStore(db).Create(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	binding, err := f.store.ReadEnrollment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	metadata := domain.ProbeCredentialMetadata{HubID: f.hubID, ProbeID: f.id.ProbeID, StreamID: f.id.StreamID, EnrollmentID: binding.EnrollmentID, CredentialVersion: 1, Endpoint: f.endpoint, Fingerprint: f.id.Fingerprint}
	cipher, err := key.SealCredential(t.Context(), metadata, f.oldToken)
	if err != nil {
		t.Fatal(err)
	}
	connections := repository.NewProbeConnectorStore(db)
	if _, err := connections.PrepareConnection(t.Context(), domain.ProbeConnection{ProbeCredentialMetadata: metadata, ProtectedCredential: cipher, State: "prepared", PreparedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := connections.ActivateConnection(t.Context(), metadata.ProbeID, metadata.EnrollmentID, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	commands := repository.NewProbeCommandStore(db, key, AcknowledgementCodec{}, key, CredentialCommandCodec{})
	issuer, err := services.NewProbeCredentialRotationService(commands, connections, key, key, CredentialCommandCodec{})
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := issuer.Issue(t.Context(), domain.ProbeCredentialRotationIssue{HubID: f.hubID, ProbeID: f.id.ProbeID, RotationID: uuid.NewString(), CredentialVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := commands.GetProtectedCommand(t.Context(), f.hubID, f.id.ProbeID, rotation.ActivateCommandID)
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.NewString()
	run := func() {
		t.Helper()
		lease, err := connections.AcquireConnector(t.Context(), f.id.ProbeID, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = connections.ReleaseConnector(ctx, lease)
		}()
		session := domain.ProbeReplaySession{HubID: f.hubID, ProbeID: f.id.ProbeID, StreamID: f.id.StreamID, OwnerID: owner, ConnectionGeneration: lease.Generation}
		selection, err := commands.SelectCredentialConnection(t.Context(), session)
		if err != nil {
			t.Fatal(err)
		}
		selected := selection.Current
		if selection.Candidate != nil {
			selected = *selection.Candidate
		}
		token, err := key.OpenCredential(t.Context(), selected.ProbeCredentialMetadata, selected.ProtectedCredential)
		if err != nil {
			t.Fatal(err)
		}
		dispatcher, err := services.NewProbeCommandService(commands, connections, key, AcknowledgementCodec{}, CredentialCommandCodec{})
		if err != nil {
			t.Fatal(err)
		}
		transport := NewHubTransport(EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
		transport.SetCommands(dispatcher)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		err = transport.Run(ctx, domain.ProbeSessionInput{OwnerID: owner, Connection: selected.ProbeCredentialMetadata, Token: token, Generation: lease.Generation}, func(ctx context.Context) error {
			if err := connections.SetConnectorConnected(ctx, lease, true); err != nil {
				return err
			}
			return commands.ConfirmCredentialConnection(ctx, session, selected.ProbeCredentialMetadata)
		}, func(context.Context, domain.ProbeActiveConfig) error { return errors.New("unexpected config receipt") }, func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
			return nil, errors.New("unexpected telemetry in credential-only fixture")
		})
		if err == nil || ctx.Err() != nil {
			t.Fatalf("command did not close session promptly: %v", err)
		}
	}
	run()
	prepared, err := commands.GetCredentialRotation(t.Context(), f.hubID, f.id.ProbeID, rotation.RotationID)
	if err != nil || prepared.State != "activating" {
		t.Fatal("prepare result did not durably release activation", err)
	}
	f.commands.lose.Store(true)
	run()
	var original domain.ProbeCommandOutcome
	select {
	case original = <-f.commands.committed:
	case <-time.After(time.Second):
		t.Fatal("activation fault did not run after source commit")
	}
	current, err := connections.GetConnection(t.Context(), f.id.ProbeID)
	if err != nil || current.CredentialVersion != 1 {
		t.Fatal("missing reply fabricated hub activation", err)
	}
	binding, err = f.store.ReadEnrollment(t.Context())
	if err != nil || binding.CredentialVersion != 2 {
		t.Fatal("fault occurred before source activation", err)
	}
	f.stop()
	f.openStore()
	f.start()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	connections = repository.NewProbeConnectorStore(db)
	commands = repository.NewProbeCommandStore(db, key, AcknowledgementCodec{}, key, CredentialCommandCodec{})
	// Wait for the persisted retry deadline; never edit delivery authority or
	// replace either protected request just to make recovery run.
	state, err := commands.GetCommand(t.Context(), f.hubID, f.id.ProbeID, rotation.ActivateCommandID)
	if err != nil || state.RemoteConfirmed || state.NextAttemptAt.IsZero() {
		t.Fatal("lost reply did not preserve retry", err)
	}
	if wait := time.Until(state.NextAttemptAt); wait > 0 {
		timer := time.NewTimer(wait + 10*time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	run()
	recovered, err := commands.GetCommand(t.Context(), f.hubID, f.id.ProbeID, rotation.ActivateCommandID)
	if err != nil || !recovered.RemoteConfirmed || recovered.Outcome == nil || !reflect.DeepEqual(*recovered.Outcome, original) {
		t.Fatal("restart failed exact activation receipt recovery", err)
	}
	preserved, err := commands.GetProtectedCommand(t.Context(), f.hubID, f.id.ProbeID, rotation.ActivateCommandID)
	if err != nil || !bytes.Equal(preserved.ProtectedPayload, activation.ProtectedPayload) {
		t.Fatal("recovery regenerated protected request", err)
	}
	active, err := commands.GetCredentialRotation(t.Context(), f.hubID, f.id.ProbeID, rotation.RotationID)
	if err != nil || active.State != "active" || !active.OverlapExpiresAt.Equal(rotation.OverlapExpiresAt) {
		t.Fatal("recovery did not promote within original operation", err)
	}
	current, err = connections.GetConnection(t.Context(), f.id.ProbeID)
	if err != nil || current.CredentialVersion != 2 || current.StreamID != metadata.StreamID {
		t.Fatal("current credential not promoted or stream reset", err)
	}
}
