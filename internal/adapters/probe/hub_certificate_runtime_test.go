package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
func TestHubCertificateRuntimeLostResultAcrossBothStoresRestartAfterRetirement(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
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
	registration := &domain.Probe{ID: f.id.ProbeID, Key: "certificate-edge", Name: "Certificate Edge", Kind: domain.ProbeKindRemote, Enabled: true}
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
	commands := repository.NewProbeCommandStore(db, key, AcknowledgementCodec{}, key, CredentialCommandCodec{}, CertificateCommandCodec{})
	// Retain the production ten-minute overlap; create the operation near its
	// end so the actual test can cross retirement without a ten-minute sleep.
	created := time.Now().UTC().Truncate(time.Microsecond).Add(-domain.ProbeCredentialOverlap + 6*time.Second)
	pending := domain.ProtectedProbeCertificateRotation{ProbeCertificateRotation: domain.ProbeCertificateRotation{RotationID: uuid.NewString(), Current: metadata, CertificateVersion: 2, PreviousVersion: 1, ValidForDays: 365, PrepareCommandID: uuid.NewString(), ActivateCommandID: uuid.NewString(), CreatedAt: created, OverlapExpiresAt: created.Add(domain.ProbeCredentialOverlap), State: "preparing"}}
	payload, err := (CertificateCommandCodec{}).EncodeCertificateCommand(t.Context(), domain.ProbeCertificateCommand{CommandID: pending.PrepareCommandID, ProbeID: metadata.ProbeID, Kind: CommandCertificatePrepare, CreatedAt: created, ExpiresAt: pending.OverlapExpiresAt, RotationID: pending.RotationID, CertificateVersion: 2, ValidForDays: 365})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	cm := domain.ProbeCommandMetadata{CommandID: pending.PrepareCommandID, HubID: f.hubID, ProbeID: f.id.ProbeID, StreamID: f.id.StreamID, Kind: CommandCertificatePrepare, CreatedAt: created, ExpiresAt: pending.OverlapExpiresAt, PayloadSHA256: hex.EncodeToString(sum[:])}
	protected, err := key.SealCommand(t.Context(), cm, payload)
	if err != nil {
		t.Fatal(err)
	}
	pending.PrepareCommand = domain.ProtectedProbeCommand{ProbeCommandMetadata: cm, ProtectedPayload: protected}
	rotation, err := commands.CreateCertificateRotation(t.Context(), pending)
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
		selected, err := connections.GetConnection(t.Context(), f.id.ProbeID)
		if err != nil {
			t.Fatal(err)
		}
		pins, err := commands.SelectCertificateConnection(t.Context(), session)
		if err != nil {
			t.Fatal(err)
		}
		pin := selected.Fingerprint
		if pins.CandidateFingerprint != "" {
			pin = pins.CandidateFingerprint
		}
		token, err := key.OpenCredential(t.Context(), selected.ProbeCredentialMetadata, selected.ProtectedCredential)
		if err != nil {
			t.Fatal(err)
		}
		dispatcher, err := services.NewProbeCommandService(commands, connections, key, AcknowledgementCodec{}, CredentialCommandCodec{}, CertificateCommandCodec{})
		if err != nil {
			t.Fatal(err)
		}
		transport := NewHubTransport(EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
		transport.SetCommands(dispatcher)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		attempt := func(pin string) error {
			return transport.Run(ctx, domain.ProbeSessionInput{DialFingerprint: pin, OwnerID: owner, Connection: selected.ProbeCredentialMetadata, Token: token, Generation: lease.Generation}, func(ctx context.Context) error {
				if err := connections.SetConnectorConnected(ctx, lease, true); err != nil {
					return err
				}
				if err := commands.ConfirmCredentialConnection(ctx, session, selected.ProbeCredentialMetadata); err != nil {
					return err
				}
				return commands.ConfirmCertificateConnection(ctx, session, selected.ProbeCredentialMetadata, pin)
			}, func(context.Context, domain.ProbeActiveConfig) error { return errors.New("unexpected config receipt") }, func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
				return nil, errors.New("unexpected telemetry in certificate-only fixture")
			})
		}
		err = attempt(pin)
		if errors.Is(err, domain.ErrProbeCertificateMismatch) && pins.AllowCurrentFallback {
			err = attempt(selected.Fingerprint)
		}
		if err == nil || ctx.Err() != nil {
			t.Fatalf("certificate command did not close after receipt or injected loss: %v", err)
		}
	}
	run()
	prepared, err := commands.GetCertificateRotation(t.Context(), f.hubID, f.id.ProbeID, rotation.RotationID)
	if err != nil || prepared.State != "activating" {
		t.Fatal("prepare result did not durably release activation", err)
	}
	activation, err := commands.GetProtectedCommand(t.Context(), f.hubID, f.id.ProbeID, rotation.ActivateCommandID)
	if err != nil {
		t.Fatal(err)
	}
	f.certificateCommands.lose.Store(true)
	run()
	var original domain.ProbeCommandOutcome
	select {
	case original = <-f.certificateCommands.committed:
	case <-time.After(time.Second):
		t.Fatal("activation fault did not run after source commit")
	}
	current, err := connections.GetConnection(t.Context(), f.id.ProbeID)
	if err != nil || current.CertificateVersion != 1 {
		t.Fatal("missing reply fabricated hub activation", err)
	}
	source, err := f.store.ReadActiveCertificate(t.Context())
	if err != nil || source.ActiveVersion != 2 {
		t.Fatal("fault occurred before source certificate activation", err)
	}
	f.stop()
	if err := f.id.Close(); err != nil {
		t.Fatal(err)
	}
	f.id, err = OpenRuntimeIdentityAnchor(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
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
	commands = repository.NewProbeCommandStore(db, key, AcknowledgementCodec{}, key, CredentialCommandCodec{}, CertificateCommandCodec{})
	// Wait for the persisted retry deadline; never edit delivery authority or
	// replace either protected request just to make recovery run.
	state, err := commands.GetCommand(t.Context(), f.hubID, f.id.ProbeID, rotation.ActivateCommandID)
	if err != nil || state.RemoteConfirmed || state.NextAttemptAt.IsZero() {
		t.Fatal("lost reply did not preserve retry", err)
	}
	if wait := max(time.Until(state.NextAttemptAt), time.Until(rotation.OverlapExpiresAt)); wait > 0 {
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
	active, err := commands.GetCertificateRotation(t.Context(), f.hubID, f.id.ProbeID, rotation.RotationID)
	if err != nil || active.State != "active" || !active.OverlapExpiresAt.Equal(rotation.OverlapExpiresAt) {
		t.Fatal("recovery did not promote within original operation", err)
	}
	current, err = connections.GetConnection(t.Context(), f.id.ProbeID)
	if err != nil || current.CertificateVersion != 2 || current.CredentialVersion != 1 || current.Fingerprint != prepared.Fingerprint || current.StreamID != metadata.StreamID {
		t.Fatal("current certificate not promoted or stream reset", err)
	}
	token, err := key.OpenCredential(t.Context(), current.ProbeCredentialMetadata, current.ProtectedCredential)
	if err != nil || token != f.oldToken {
		t.Fatal("promoted pin did not reseal original runtime credential", err)
	}
	if !active.OverlapClosed {
		t.Fatal("after-deadline recovery did not persist retirement")
	}
}
