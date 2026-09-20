package repository_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type certificateRotationFixture struct {
	rotationFixture
	certificates *services.ProbeCertificateRotationService
}

func newCertificateRotationFixture(t *testing.T, engine string) certificateRotationFixture {
	t.Helper()
	f := newRotationFixture(t, engine)
	s, err := services.NewProbeCertificateRotationService(f.commands, f.connections, f.protector, probe.CertificateCommandCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return certificateRotationFixture{rotationFixture: f, certificates: s}
}
func (f certificateRotationFixture) certificateIssue() domain.ProbeCertificateRotationIssue {
	return domain.ProbeCertificateRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: rotationTestID, CertificateVersion: 2, ValidForDays: 365}
}
func (f certificateRotationFixture) issueCertificate(t *testing.T) *domain.ProbeCertificateRotation {
	t.Helper()
	r, err := f.certificates.Issue(t.Context(), f.certificateIssue())
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f certificateRotationFixture) certificateStatus(t *testing.T) *domain.ProbeCertificateRotation {
	t.Helper()
	r, err := f.commands.GetCertificateRotation(t.Context(), f.session.HubID, f.session.ProbeID, rotationTestID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f certificateRotationFixture) claimCertificate(t *testing.T, kind string) *domain.ProtectedProbeCommand {
	t.Helper()
	c, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{CertificateRotation: true})
	if err != nil || c == nil || c.Kind != kind {
		t.Fatal("unexpected certificate claim", kind, c, err)
	}
	return c
}
func certificatePrepareOutcome(r *domain.ProbeCertificateRotation) domain.ProbeCommandOutcome {
	// Model the permitted source clock skew without assuming the host and DB
	// clocks are synchronized to microseconds.
	at := r.CreatedAt.Add(-time.Second)
	expiry := at.Truncate(time.Second).Add(time.Duration(r.ValidForDays) * 24 * time.Hour)
	sum := sha256.Sum256([]byte("prepared certificate " + r.RotationID))
	return domain.ProbeCommandOutcome{CommandID: r.PrepareCommandID, Status: "applied", AppliedAt: &at, CertificateVersion: r.CertificateVersion, CertificateFingerprint: hex.EncodeToString(sum[:]), CertificateNotAfter: &expiry}
}
func (f certificateRotationFixture) prepareCertificate(t *testing.T, r *domain.ProbeCertificateRotation) domain.ProbeCommandOutcome {
	t.Helper()
	f.claimCertificate(t, "certificate.prepare")
	out := certificatePrepareOutcome(r)
	if err := f.commands.CompleteCommand(t.Context(), f.session, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProbeCertificateRotation(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, certificateRotationFixture){
				"ReceiptDrivenPromotionAndResealing":          testCertificatePromotion,
				"PreparationFailureCancelsReservedActivation": testCertificateFailure,
				"FencedSelectionAndConfirmation":              testCertificateFences,
				"IssuanceRollback":                            testCertificateIssuanceRollback,
				"PrepareRollback":                             testCertificatePrepareRollback,
				"ActivationRollback":                          testCertificateActivationRollback,
				"StrictReceiptDetails":                        testCertificateStrictDetails,
				"CapabilityRouting":                           testCertificateCapabilities,
				"CredentialExclusion":                         testCertificateCredentialExclusion,
				"ConcurrentStableIssuance":                    testCertificateConcurrent,
				"UTCReceiptBoundary":                          testCertificateUTC,
				"CredentialFirstExclusion":                    testCertificateCredentialFirst,
				"RetainedCommandDependency":                   testCertificateRetention,
				"MigrationPreservesState":                     testCertificateMigration,
				"TamperedActivationFailsClosed":               testCertificateTamper,
				"StrictActivationReceipt":                     testCertificateActivationDetails,
				"ActivationCapacityReservation":               testCertificateCapacity,
				"ObservedRetirementIsIrreversible":            testCertificateClosedOverlap,
			} {
				t.Run(name, func(t *testing.T) { test(t, newCertificateRotationFixture(t, engine)) })
			}
		})
	}
}

func testCertificatePromotion(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	activation, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.ActivateCommandID)
	if err != nil || activation.Status != "blocked" || activation.RemoteConfirmed || activation.PayloadSHA256 != "" {
		t.Fatal("activation fabricated payload or success", err)
	}
	before, err := f.commands.GetProtectedCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.PrepareCommandID)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := f.certificates.Issue(t.Context(), f.certificateIssue()); err != nil || !reflect.DeepEqual(same, r) {
		t.Fatal("retry changed issuance", err)
	}
	prepared := f.prepareCertificate(t, r)
	selection, err := f.commands.SelectCertificateConnection(t.Context(), f.session)
	if err != nil || selection.Current != f.current.ProbeCredentialMetadata || selection.CandidateFingerprint != prepared.CertificateFingerprint || !selection.AllowCurrentFallback {
		t.Fatal("candidate did not preserve old credential scope", selection, err)
	}
	if err := f.commands.ConfirmCertificateConnection(t.Context(), f.session, selection.Current, selection.CandidateFingerprint); err != nil {
		t.Fatal(err)
	}
	current, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || current.Fingerprint != f.current.Fingerprint || f.certificateStatus(t).State != "activating" {
		t.Fatal("handshake fabricated promotion", err)
	}
	activate := f.claimCertificate(t, "certificate.activate")
	plain, err := f.protector.OpenCommand(t.Context(), activate.ProbeCommandMetadata, activate.ProtectedPayload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := (probe.CertificateCommandCodec{}).DecodeCertificateCommand(t.Context(), plain)
	clear(plain)
	if err != nil || decoded.ExpectedFingerprint != prepared.CertificateFingerprint || decoded.CommandID != r.ActivateCommandID || !decoded.ExpiresAt.Equal(r.OverlapExpiresAt) {
		t.Fatal("activation was not built from exact prepare receipt", err)
	}
	at := prepared.AppliedAt.Add(time.Millisecond)
	result := domain.ProbeCommandOutcome{CommandID: r.ActivateCommandID, Status: "applied", AppliedAt: &at}
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); err != nil {
		t.Fatal(err)
	}
	current, err = f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || current.Fingerprint != prepared.CertificateFingerprint || current.CertificateVersion != 2 || current.CredentialVersion != f.current.CredentialVersion || current.StreamID != f.current.StreamID || current.CertificateNotAfter == nil || !current.CertificateNotAfter.Equal(*prepared.CertificateNotAfter) {
		t.Fatal("promotion lost scope", err)
	}
	oldToken, err := f.protector.OpenCredential(t.Context(), f.current.ProbeCredentialMetadata, f.current.ProtectedCredential)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.protector.OpenCredential(t.Context(), current.ProbeCredentialMetadata, current.ProtectedCredential)
	if err != nil || token != oldToken || bytes.Equal(current.ProtectedCredential, f.current.ProtectedCredential) {
		t.Fatal("credential not resealed under new pin", err)
	}
	if _, err := f.protector.OpenCredential(t.Context(), f.current.ProbeCredentialMetadata, current.ProtectedCredential); err == nil {
		t.Fatal("promoted ciphertext retained old AEAD pin")
	}
	f.commands = repository.NewProbeCommandStore(f.f.db, f.protector, probe.AcknowledgementCodec{}, f.protector, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
	for _, receipt := range []domain.ProbeCommandOutcome{prepared, result} {
		if err := f.commands.CompleteCommand(t.Context(), f.session, receipt); err != nil {
			t.Fatal("restart lost original receipt", err)
		}
	}
	retained, err := f.commands.GetProtectedCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.PrepareCommandID)
	if err != nil || !reflect.DeepEqual(retained, before) {
		t.Fatal("promotion changed preparation bytes", err)
	}
	if f.certificateStatus(t).State != "active" {
		t.Fatal("activation not confirmed")
	}
}

func testCertificateFailure(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	f.claimCertificate(t, "certificate.prepare")
	if err := f.commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: r.PrepareCommandID, Status: "expired"}); err != nil {
		t.Fatal(err)
	}
	c, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.ActivateCommandID)
	if err != nil || c.Status != "canceled" || c.RemoteConfirmed || c.Attempts != 0 || c.Outcome != nil || f.certificateStatus(t).State != "failed" {
		t.Fatal("unsent activation fabricated receipt", err)
	}
	if c, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{CertificateRotation: true}); err != nil || c != nil {
		t.Fatal("failed preparation released activation", err)
	}
}
func testCertificateFences(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	out := f.prepareCertificate(t, r)
	for name, change := range map[string]func(*domain.ProbeReplaySession){"owner": func(s *domain.ProbeReplaySession) { s.OwnerID = s.HubID }, "generation": func(s *domain.ProbeReplaySession) { s.ConnectionGeneration++ }, "stream": func(s *domain.ProbeReplaySession) { s.StreamID = s.HubID }} {
		t.Run(name, func(t *testing.T) {
			bad := f.session
			change(&bad)
			if _, err := f.commands.SelectCertificateConnection(t.Context(), bad); err == nil {
				t.Fatal("stale selection succeeded")
			}
			if err := f.commands.ConfirmCertificateConnection(t.Context(), bad, f.current.ProbeCredentialMetadata, out.CertificateFingerprint); err == nil {
				t.Fatal("stale confirmation succeeded")
			}
		})
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET lease_until = 0 WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commands.SelectCertificateConnection(t.Context(), f.session); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired lease selected", err)
	}
}
func testCertificateIssuanceRollback(t *testing.T, f certificateRotationFixture) {
	cleanup := injectOutboxFailure(t, f.f, "probe_connections", "UPDATE")
	_, err := f.certificates.Issue(t.Context(), f.certificateIssue())
	cleanup()
	if err == nil || replayCount(t, f.f, "probe_commands") != 0 || replayCount(t, f.f, "probe_certificate_rotations") != 0 {
		t.Fatal("failed last issuance write leaked state", err)
	}
	f.issueCertificate(t)
}
func testCertificatePrepareRollback(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	f.claimCertificate(t, "certificate.prepare")
	out := certificatePrepareOutcome(r)
	cleanup := injectOutboxFailure(t, f.f, "probe_certificate_rotations", "UPDATE")
	err := f.commands.CompleteCommand(t.Context(), f.session, out)
	cleanup()
	if err == nil || f.certificateStatus(t).State != "preparing" {
		t.Fatal("prepare rollback failed", err)
	}
	c, err := f.commands.GetProtectedCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.ActivateCommandID)
	if err != nil || c.PayloadSHA256 != "" || len(c.ProtectedPayload) != 0 {
		t.Fatal("failed prepare leaked activation payload", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, out); err != nil {
		t.Fatal(err)
	}
}
func testCertificateActivationRollback(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	prepared := f.prepareCertificate(t, r)
	f.claimCertificate(t, "certificate.activate")
	at := prepared.AppliedAt.Add(time.Millisecond)
	out := domain.ProbeCommandOutcome{CommandID: r.ActivateCommandID, Status: "applied", AppliedAt: &at}
	cleanup := injectOutboxFailure(t, f.f, "probe_commands", "UPDATE")
	err := f.commands.CompleteCommand(t.Context(), f.session, out)
	cleanup()
	if err == nil || f.certificateStatus(t).State != "activating" {
		t.Fatal("late receipt failure advanced operation", err)
	}
	c, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || c.Fingerprint != f.current.Fingerprint || !bytes.Equal(c.ProtectedCredential, f.current.ProtectedCredential) {
		t.Fatal("failed receipt leaked pin or resealed token", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, out); err != nil {
		t.Fatal(err)
	}
}
func testCertificateStrictDetails(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	f.claimCertificate(t, "certificate.prepare")
	for _, mutate := range []func(*domain.ProbeCommandOutcome){func(o *domain.ProbeCommandOutcome) { o.CertificateVersion = 3 }, func(o *domain.ProbeCommandOutcome) { o.CertificateFingerprint = f.current.Fingerprint }, func(o *domain.ProbeCommandOutcome) { o.CertificateNotAfter = nil }, func(o *domain.ProbeCommandOutcome) { o.CredentialVersion = 2 }, func(o *domain.ProbeCommandOutcome) {
		x := o.CertificateNotAfter.Add(time.Second)
		o.CertificateNotAfter = &x
	}, func(o *domain.ProbeCommandOutcome) { o.AppliedAt = &r.OverlapExpiresAt }} {
		out := certificatePrepareOutcome(r)
		mutate(&out)
		if err := f.commands.CompleteCommand(t.Context(), f.session, out); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid certificate receipt accepted", err)
		}
	}
	if f.certificateStatus(t).State != "preparing" {
		t.Fatal("invalid details changed state")
	}
}
func testCertificateCapabilities(t *testing.T, f certificateRotationFixture) {
	f.issueCertificate(t)
	f.issue(t)
	if c, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{CredentialRotation: true}); err != nil || c != nil {
		t.Fatal("certificate sent to credential-only peer", err)
	}
	if c, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err != nil || c == nil || c.Kind != "alert.ack" {
		t.Fatal("unsupported certificate blocked ACK", err)
	}
	f.claimCertificate(t, "certificate.prepare")
}
func testCertificateCredentialExclusion(t *testing.T, f certificateRotationFixture) {
	f.issueCertificate(t)
	if _, err := f.service.Issue(t.Context(), domain.ProbeCredentialRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: uuid.NewString(), CredentialVersion: 2}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("overlapping credential issuance succeeded", err)
	}
}
func testCertificateConcurrent(t *testing.T, f certificateRotationFixture) {
	var workers sync.WaitGroup
	results := make(chan *domain.ProbeCertificateRotation, 4)
	errs := make(chan error, 4)
	for range 4 {
		workers.Go(func() { r, err := f.certificates.Issue(t.Context(), f.certificateIssue()); results <- r; errs <- err })
	}
	workers.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	winner := f.certificateStatus(t)
	for r := range results {
		if !reflect.DeepEqual(r, winner) {
			t.Fatal("concurrent issuance changed identity")
		}
	}
	if replayCount(t, f.f, "probe_commands") != 2 || replayCount(t, f.f, "probe_certificate_rotations") != 1 {
		t.Fatal("concurrent issuance duplicated effects")
	}
}

func testCertificateUTC(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	f.claimCertificate(t, "certificate.prepare")
	original := certificatePrepareOutcome(r)
	zoned := original
	at := original.AppliedAt.In(time.FixedZone("Bangkok", 7*60*60))
	expiry := original.CertificateNotAfter.In(time.FixedZone("Bangkok", 7*60*60))
	zoned.AppliedAt, zoned.CertificateNotAfter = &at, &expiry
	if err := f.commands.CompleteCommand(t.Context(), f.session, zoned); err != nil {
		t.Fatal(err)
	}
	saved := f.certificateStatus(t)
	if !saved.PreparedAt.Equal(*original.AppliedAt) || !saved.CertificateNotAfter.Equal(*original.CertificateNotAfter) || saved.PreparedAt.Location() != time.UTC || saved.CertificateNotAfter.Location() != time.UTC {
		t.Fatal("non-UTC receipt shifted stored certificate times", saved.PreparedAt, saved.CertificateNotAfter)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, original); err != nil {
		t.Fatal("same instant was not an immutable duplicate", err)
	}
}

func testCertificateCredentialFirst(t *testing.T, f certificateRotationFixture) {
	f.issueRotation(t)
	if _, err := f.certificates.Issue(t.Context(), f.certificateIssue()); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("overlapping certificate issuance succeeded", err)
	}
	if replayCount(t, f.f, "probe_certificate_rotations") != 0 {
		t.Fatal("rejected issuance wrote state")
	}
}
func testCertificateRetention(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	f.prepareCertificate(t, r)
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_commands SET retain_until = ? WHERE command_id = ?", time.Now().UTC().Add(-time.Hour), r.PrepareCommandID); err != nil {
		t.Fatal(err)
	}
	f.issue(t)
	if selected, err := f.commands.SelectCertificateConnection(t.Context(), f.session); err != nil || selected.CandidateFingerprint == "" {
		t.Fatal("retention discarded unresolved dependency", err)
	}
	if _, err := f.commands.GetProtectedCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.PrepareCommandID); err != nil {
		t.Fatal(err)
	}
}
func testCertificateMigration(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	if err := runNamedMigration(t, f.f, "063_probe_certificate_rotation", "down"); err == nil {
		t.Fatal("downgrade discarded live operation")
	}
	if got := f.certificateStatus(t); !reflect.DeepEqual(got, r) {
		t.Fatal("refused downgrade changed operation")
	}
	for _, table := range []string{"probe_certificate_rotations", "probe_commands"} {
		if _, err := f.f.db.ExecContext(t.Context(), "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	if err := runNamedMigration(t, f.f, "063_probe_certificate_rotation", "down"); err == nil {
		t.Fatal("downgrade discarded high-water")
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_connections SET certificate_high_water = 1"); err != nil {
		t.Fatal(err)
	}
	// Existing credential operations and ACKs must survive removing/reapplying 063.
	credential := f.issueRotation(t)
	f.issue(t)
	before, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runNamedMigration(t, f.f, "063_probe_certificate_rotation", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runNamedMigration(t, f.f, "063_probe_certificate_rotation", "up"); err != nil {
		t.Fatal(err)
	}
	after, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("migration damaged existing connection", err)
	}
	if !reflect.DeepEqual(f.rotationStatus(t), credential) || replayCount(t, f.f, "probe_commands") != 3 {
		t.Fatal("migration damaged credential or ACK state")
	}
}
func testCertificateTamper(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	prepared := f.prepareCertificate(t, r)
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_commands SET protected_payload = ? WHERE command_id = ?", []byte("tampered"), r.ActivateCommandID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commands.SelectCertificateConnection(t.Context(), f.session); err == nil {
		t.Fatal("tampered activation selected")
	}
	if err := f.commands.ConfirmCertificateConnection(t.Context(), f.session, f.current.ProbeCredentialMetadata, prepared.CertificateFingerprint); err == nil {
		t.Fatal("tampered activation admitted")
	}
	if f.certificateStatus(t).State != "activating" {
		t.Fatal("invalid selection changed state")
	}
}
func testCertificateActivationDetails(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	prepared := f.prepareCertificate(t, r)
	f.claimCertificate(t, "certificate.activate")
	at := prepared.AppliedAt.Add(time.Millisecond)
	for _, mutate := range []func(*domain.ProbeCommandOutcome){
		func(o *domain.ProbeCommandOutcome) {
			o.CertificateVersion, o.CertificateFingerprint, o.CertificateNotAfter = prepared.CertificateVersion, prepared.CertificateFingerprint, prepared.CertificateNotAfter
		},
		func(o *domain.ProbeCommandOutcome) { o.CredentialVersion = 2 },
		func(o *domain.ProbeCommandOutcome) { o.Status = "already_resolved" },
		func(o *domain.ProbeCommandOutcome) {
			at := prepared.AppliedAt.Add(-time.Microsecond)
			o.AppliedAt = &at
		},
	} {
		o := domain.ProbeCommandOutcome{CommandID: r.ActivateCommandID, Status: "applied", AppliedAt: &at}
		mutate(&o)
		if err := f.commands.CompleteCommand(t.Context(), f.session, o); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid activation accepted", err)
		}
	}
	current, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || !reflect.DeepEqual(current, &f.current) || f.certificateStatus(t).State != "activating" {
		t.Fatal("rejection leaked promotion", err)
	}
}
func testCertificateClosedOverlap(t *testing.T, f certificateRotationFixture) {
	r := f.issueCertificate(t)
	out := f.prepareCertificate(t, r)
	// Model durable retirement observed before a clock rollback: a future-looking
	// deadline must never reopen the recorded closed window.
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_certificate_rotations SET overlap_closed = ? WHERE rotation_id = ?", true, r.RotationID); err != nil {
		t.Fatal(err)
	}
	restored := repository.NewProbeCommandStore(f.f.db, f.protector, probe.AcknowledgementCodec{}, f.protector, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
	selected, err := restored.SelectCertificateConnection(t.Context(), f.session)
	if err != nil || selected.AllowCurrentFallback || selected.CandidateFingerprint != out.CertificateFingerprint {
		t.Fatal("closed overlap reopened", err)
	}
	if err := restored.ConfirmCertificateConnection(t.Context(), f.session, f.current.ProbeCredentialMetadata, f.current.Fingerprint); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retired pin admitted", err)
	}
	if err := restored.ConfirmCertificateConnection(t.Context(), f.session, f.current.ProbeCredentialMetadata, out.CertificateFingerprint); err != nil {
		t.Fatal("candidate recovery blocked", err)
	}
}

func testCertificateCapacity(t *testing.T, f certificateRotationFixture) {
	seed := f.issue(t)
	// Only occupancy is under test here. Copy bounded storage rows without
	// dispatching them; each receives a distinct primary key.
	tx, err := f.f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var last string
	for range 1022 {
		last = uuid.NewString()
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO probe_commands
   (command_id,hub_id,probe_id,stream_id,kind,source_alert_id,assignment_generation,created_at,expires_at,payload_sha256,protected_payload,status,remote_confirmed,attempts,next_attempt_at,retain_until,updated_at)
   SELECT ?,hub_id,probe_id,stream_id,kind,source_alert_id,assignment_generation,created_at,expires_at,payload_sha256,protected_payload,status,remote_confirmed,attempts,next_attempt_at,retain_until,updated_at FROM probe_commands WHERE command_id = ?`, last, seed.CommandID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.certificates.Issue(t.Context(), f.certificateIssue()); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("missing reservation admitted two commands into one slot", err)
	}
	if replayCount(t, f.f, "probe_certificate_rotations") != 0 || replayCount(t, f.f, "probe_commands") != 1023 {
		t.Fatal("capacity rejection leaked state")
	}
	if _, err := f.f.db.ExecContext(t.Context(), "DELETE FROM probe_commands WHERE command_id = ?", last); err != nil {
		t.Fatal(err)
	}
	r := f.issueCertificate(t)
	ack := f.ack
	ack.CommandID = uuid.NewString()
	if _, err := f.commands.CreateCommand(t.Context(), f.protect(t, ack)); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("blocked activation did not reserve pending capacity", err)
	}
	f.prepareCertificate(t, r)
	if _, err := f.commands.CreateCommand(t.Context(), f.protect(t, ack)); err != nil {
		t.Fatal("completed preparation failed to release its own slot", err)
	}
	collision := f.ack
	collision.CommandID = r.ActivateCommandID
	if _, err := f.commands.CreateCommand(t.Context(), f.protect(t, collision)); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("reserved global activation ID was reused", err)
	}
}
