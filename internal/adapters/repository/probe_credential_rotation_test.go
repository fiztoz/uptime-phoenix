package repository_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

const rotationTestID = "abababab-abab-4bab-8bab-abababababab"

type rotationFixture struct {
	commandFixture
	service     *services.ProbeCredentialRotationService
	connections *repository.ProbeConnectorStore
	current     domain.ProbeConnection
}

func newRotationFixture(t *testing.T, engine string) rotationFixture {
	t.Helper()
	f := newCommandFixture(t, engine)
	connections := repository.NewProbeConnectorStore(f.f.db)
	current, err := connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	// The replay fixture deliberately has opaque placeholder bytes; credential
	// tests need a real authenticated current credential, not a permissive fake.
	protected, err := f.protector.SealCredential(t.Context(), current.ProbeCredentialMetadata, "phx_probe_"+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_connections SET protected_credential = ? WHERE probe_id = ?", protected, current.ProbeID); err != nil {
		t.Fatal(err)
	}
	current.ProtectedCredential = protected
	service, err := services.NewProbeCredentialRotationService(f.commands, connections, f.protector, f.protector, probe.CredentialCommandCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return rotationFixture{commandFixture: f, service: service, connections: connections, current: *current}
}

func (f rotationFixture) issueRotation(t *testing.T) *domain.ProbeCredentialRotation {
	t.Helper()
	r, err := f.service.Issue(t.Context(), domain.ProbeCredentialRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: rotationTestID, CredentialVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f rotationFixture) claimRotation(t *testing.T, kind string) *domain.ProtectedProbeCommand {
	t.Helper()
	c, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{CredentialRotation: true})
	if err != nil || c == nil || c.Kind != kind {
		t.Fatalf("claim %s: %+v %v", kind, c, err)
	}
	return c
}

func (f rotationFixture) prepareRotation(t *testing.T, r *domain.ProbeCredentialRotation) domain.ProbeCommandOutcome {
	t.Helper()
	f.claimRotation(t, "credential.prepare")
	at := r.CreatedAt.Add(time.Millisecond)
	out := domain.ProbeCommandOutcome{CommandID: r.PrepareCommandID, Status: "applied", AppliedAt: &at, CredentialVersion: 2}
	if err := f.commands.CompleteCommand(t.Context(), f.session, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f rotationFixture) rotationStatus(t *testing.T) *domain.ProbeCredentialRotation {
	t.Helper()
	r, err := f.commands.GetCredentialRotation(t.Context(), f.session.HubID, f.session.ProbeID, rotationTestID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProbeCredentialRotation(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, rotationFixture){
				"DurableIssuanceAndPromotion":              testRotationPromotion,
				"PrepareFailureDoesNotFabricateActivation": testRotationPrepareFailure,
				"CandidateAuthenticationIsNotActivation":   testRotationCandidate,
				"FencedSelectionAndConfirmation":           testRotationFences,
				"CreationRollback":                         testRotationCreationRollback,
				"FinalIssuanceWriteRollback":               testRotationFinalIssuanceRollback,
				"PrepareResultRollback":                    testRotationPrepareRollback,
				"ActivateResultRollback":                   testRotationActivateRollback,
				"StrictResultDetails":                      testRotationResultDetails,
				"CapabilityRouting":                        testRotationCapabilities,
				"UnresolvedRetentionDependency":            testRotationRetention,
				"ConcurrentOperatorRetry":                  testRotationConcurrent,
				"MigrationPreservesOperation":              testRotationMigration,
				"LostActivationReceiptAfterDeadline":       testRotationLateReceipt,
			} {
				t.Run(name, func(t *testing.T) { test(t, newRotationFixture(t, engine)) })
			}
		})
	}
}

func testRotationLateReceipt(t *testing.T, f rotationFixture) {
	created := time.Now().UTC().Truncate(time.Microsecond).Add(-domain.ProbeCredentialOverlap + 3*time.Second)
	metadata := f.current.ProbeCredentialMetadata
	metadata.CredentialVersion = 2
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{98}, 32))
	cipher, err := f.protector.SealCredential(t.Context(), metadata, token)
	if err != nil {
		t.Fatal(err)
	}
	r := domain.ProtectedProbeCredentialRotation{ProbeCredentialRotation: domain.ProbeCredentialRotation{RotationID: rotationTestID, Candidate: metadata, PreviousVersion: 1, PrepareCommandID: uuid.NewString(), ActivateCommandID: uuid.NewString(), CreatedAt: created, OverlapExpiresAt: created.Add(domain.ProbeCredentialOverlap), State: "preparing"}, ProtectedCredential: cipher}
	protect := func(kind, id, plainToken string) domain.ProtectedProbeCommand {
		t.Helper()
		command := domain.ProbeCredentialCommand{CommandID: id, ProbeID: metadata.ProbeID, Kind: kind, CreatedAt: created, ExpiresAt: r.OverlapExpiresAt, RotationID: r.RotationID, CredentialVersion: 2}
		if kind == "credential.prepare" {
			command.TokenHash = sha256.Sum256([]byte(plainToken))
			command.OverlapExpiresAt = r.OverlapExpiresAt
		}
		payload, err := (probe.CredentialCommandCodec{}).EncodeCredentialCommand(t.Context(), command, plainToken)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(payload)
		hash := sha256.Sum256(payload)
		m := domain.ProbeCommandMetadata{CommandID: id, HubID: metadata.HubID, ProbeID: metadata.ProbeID, StreamID: metadata.StreamID, Kind: kind, CreatedAt: created, ExpiresAt: r.OverlapExpiresAt, PayloadSHA256: hex.EncodeToString(hash[:])}
		cipher, err := f.protector.SealCommand(t.Context(), m, payload)
		if err != nil {
			t.Fatal(err)
		}
		return domain.ProtectedProbeCommand{ProbeCommandMetadata: m, ProtectedPayload: cipher}
	}
	r.PrepareCommand = protect("credential.prepare", r.PrepareCommandID, token)
	r.ActivateCommand = protect("credential.activate", r.ActivateCommandID, "")
	if _, err := f.commands.CreateCredentialRotation(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	f.prepareRotation(t, &r.ProbeCredentialRotation)
	f.claimRotation(t, "credential.activate")
	// Model a source commit whose reply is lost until after the original expiry.
	at := r.OverlapExpiresAt.Add(-time.Second)
	timer := time.NewTimer(max(0, time.Until(r.OverlapExpiresAt)) + 10*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	f.commands = repository.NewProbeCommandStore(f.f.db, f.protector, probe.AcknowledgementCodec{}, f.protector, probe.CredentialCommandCodec{})
	selected, err := f.commands.SelectCredentialConnection(t.Context(), f.session)
	if err != nil || selected.Candidate == nil || selected.Current.CredentialVersion != 1 {
		t.Fatal("expired pending rotation discarded recovery candidate", err)
	}
	if err := f.commands.ConfirmCredentialConnection(t.Context(), f.session, r.Candidate); err != nil {
		t.Fatal("late candidate authentication rejected", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: r.ActivateCommandID, Status: "applied", AppliedAt: &at}); err != nil {
		t.Fatal("original source receipt expired on hub", err)
	}
	current, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || current.CredentialVersion != 2 || f.rotationStatus(t).State != "active" {
		t.Fatal("late receipt did not recover", err)
	}
}

func testRotationPromotion(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	retry := f.issueRotation(t)
	if !reflect.DeepEqual(r, retry) || r.State != "preparing" || r.PreparedAt != nil || r.ActivatedAt != nil || r.OverlapExpiresAt.Sub(r.CreatedAt) != domain.ProbeCredentialOverlap {
		t.Fatal("retry changed identity or fabricated effect")
	}
	issue := domain.ProbeCredentialRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: rotationTestID, CredentialVersion: 3}
	if _, err := f.service.Issue(t.Context(), issue); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("version changed under same ID", err)
	}
	prepare, err := f.commands.GetProtectedCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.PrepareCommandID)
	if err != nil {
		t.Fatal(err)
	}
	var candidate []byte
	if err := f.f.db.NewRaw("SELECT protected_credential FROM probe_credential_rotations WHERE rotation_id = ?", rotationTestID).Scan(t.Context(), &candidate); err != nil {
		t.Fatal(err)
	}
	token, err := f.protector.OpenCredential(t.Context(), r.Candidate, candidate)
	if err != nil || bytes.Contains(candidate, []byte(token)) || bytes.Contains(prepare.ProtectedPayload, []byte(token)) {
		t.Fatal("token stored unprotected", err)
	}
	selection, err := f.commands.SelectCredentialConnection(t.Context(), f.session)
	if err != nil || selection.Candidate != nil || selection.Current.CredentialVersion != 1 {
		t.Fatal("unprepared candidate selected", err)
	}
	prepared := f.prepareRotation(t, r)
	selection, err = f.commands.SelectCredentialConnection(t.Context(), f.session)
	if err != nil || selection.Candidate == nil || !bytes.Equal(selection.Candidate.ProtectedCredential, candidate) {
		t.Fatal("candidate not recoverable", err)
	}
	f.claimRotation(t, "credential.activate")
	at := r.CreatedAt.Add(2 * time.Millisecond)
	activated := domain.ProbeCommandOutcome{CommandID: r.ActivateCommandID, Status: "applied", AppliedAt: &at}
	if err := f.commands.CompleteCommand(t.Context(), f.session, activated); err != nil {
		t.Fatal(err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, prepared); err != nil {
		t.Fatal("prepare duplicate failed after activation", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, activated); err != nil {
		t.Fatal("activation duplicate failed", err)
	}
	got := f.rotationStatus(t)
	current, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || got.State != "active" || got.PreparedAt == nil || !got.PreparedAt.Equal(*prepared.AppliedAt) || got.ActivatedAt == nil || !got.ActivatedAt.Equal(at) || current.CredentialVersion != 2 || !bytes.Equal(current.ProtectedCredential, candidate) || current.StreamID != f.current.StreamID {
		t.Fatal("source receipt not promoted atomically", err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_connections SET credential_version = 3 WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, activated); err != nil {
		t.Fatal(err)
	}
	current, err = f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || current.CredentialVersion != 3 {
		t.Fatal("old receipt moved current backward", err)
	}
}

func testRotationPrepareFailure(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	f.claimRotation(t, "credential.prepare")
	if err := f.commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: r.PrepareCommandID, Status: "expired"}); err != nil {
		t.Fatal(err)
	}
	c, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.ActivateCommandID)
	if err != nil || c.Status != "canceled" || c.RemoteConfirmed || c.Attempts != 0 || c.Outcome != nil || f.rotationStatus(t).State != "failed" {
		t.Fatal("unsent activation fabricated source result", err)
	}
	if got, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{CredentialRotation: true}); err != nil || got != nil {
		t.Fatal("failed prepare dispatched activation", err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_credential_rotations SET overlap_expires_at = ?, created_at = ?", time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(-11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	issue := domain.ProbeCredentialRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd", CredentialVersion: 2}
	if _, err := f.service.Issue(t.Context(), issue); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("failed version reused", err)
	}
	issue.CredentialVersion = 3
	if _, err := f.service.Issue(t.Context(), issue); err != nil {
		t.Fatal("higher version could not recover failed operation", err)
	}
}

func testRotationCandidate(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	f.prepareRotation(t, r)
	// Construct a late retry through a fresh repository instance. Changing only
	// retention metadata below must not remove the unresolved candidate.
	f.commands = repository.NewProbeCommandStore(f.f.db, f.protector, probe.AcknowledgementCodec{}, f.protector, probe.CredentialCommandCodec{})
	selected, err := f.commands.SelectCredentialConnection(t.Context(), f.session)
	if err != nil || selected.Candidate == nil || selected.RotationID != rotationTestID {
		t.Fatal("restart lost candidate", err)
	}
	if err := f.commands.ConfirmCredentialConnection(t.Context(), f.session, selected.Candidate.ProbeCredentialMetadata); err != nil {
		t.Fatal(err)
	}
	current, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || current.CredentialVersion != 1 || f.rotationStatus(t).State != "activating" {
		t.Fatal("candidate health fabricated activation", err)
	}
	f.claimRotation(t, "credential.activate")
	if err := f.commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: r.ActivateCommandID, Status: "expired"}); err != nil {
		t.Fatal(err)
	}
	selected, err = f.commands.SelectCredentialConnection(t.Context(), f.session)
	if err != nil || selected.Candidate != nil || selected.Current.CredentialVersion != 1 || f.rotationStatus(t).State != "failed" {
		t.Fatal("expired unactivated window lost original credential", err)
	}
}

func testRotationFences(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	f.prepareRotation(t, r)
	for name, alter := range map[string]func(*domain.ProbeReplaySession){"owner": func(s *domain.ProbeReplaySession) { s.OwnerID = s.HubID }, "generation": func(s *domain.ProbeReplaySession) { s.ConnectionGeneration++ }, "stream": func(s *domain.ProbeReplaySession) { s.StreamID = s.HubID }, "hub": func(s *domain.ProbeReplaySession) { s.HubID = s.ProbeID }} {
		t.Run(name, func(t *testing.T) {
			bad := f.session
			alter(&bad)
			if _, err := f.commands.SelectCredentialConnection(t.Context(), bad); err == nil {
				t.Fatal("stale selection succeeded")
			}
			if err := f.commands.ConfirmCredentialConnection(t.Context(), bad, r.Candidate); err == nil {
				t.Fatal("stale confirmation succeeded")
			}
		})
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET lease_until = 0 WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commands.SelectCredentialConnection(t.Context(), f.session); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired lease selected", err)
	}
	if err := f.commands.ConfirmCredentialConnection(t.Context(), f.session, r.Candidate); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired lease confirmed", err)
	}
}

func testRotationCreationRollback(t *testing.T, f rotationFixture) {
	cleanup := injectOutboxFailure(t, f.f, "probe_credential_rotations", "INSERT")
	_, err := f.service.Issue(t.Context(), domain.ProbeCredentialRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: rotationTestID, CredentialVersion: 2})
	cleanup()
	if err == nil || replayCount(t, f.f, "probe_commands") != 0 || replayCount(t, f.f, "probe_credential_rotations") != 0 {
		t.Fatal("failed issuance leaked requests", err)
	}
	f.issueRotation(t)
}

func testRotationPrepareRollback(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	f.claimRotation(t, "credential.prepare")
	cleanup := injectOutboxFailure(t, f.f, "probe_credential_rotations", "UPDATE")
	out := domain.ProbeCommandOutcome{CommandID: r.PrepareCommandID, Status: "applied", AppliedAt: &r.CreatedAt, CredentialVersion: 2}
	err := f.commands.CompleteCommand(t.Context(), f.session, out)
	cleanup()
	if err == nil || f.rotationStatus(t).State != "preparing" {
		t.Fatal("prepare rollback lost", err)
	}
	c, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.ActivateCommandID)
	if err != nil || c.Status != "blocked" || c.Attempts != 0 {
		t.Fatal("failed prepare released activation", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, out); err != nil {
		t.Fatal(err)
	}
}

func testRotationActivateRollback(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	f.prepareRotation(t, r)
	f.claimRotation(t, "credential.activate")
	cleanup := injectOutboxFailure(t, f.f, "probe_commands", "UPDATE")
	out := domain.ProbeCommandOutcome{CommandID: r.ActivateCommandID, Status: "applied", AppliedAt: &r.CreatedAt}
	err := f.commands.CompleteCommand(t.Context(), f.session, out)
	cleanup()
	if err == nil || f.rotationStatus(t).State != "activating" {
		t.Fatal("activation failure advanced rotation", err)
	}
	current, err := f.connections.GetConnection(t.Context(), f.session.ProbeID)
	if err != nil || current.CredentialVersion != 1 {
		t.Fatal("late failure leaked promoted credential", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, out); err != nil {
		t.Fatal(err)
	}
}

func testRotationResultDetails(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	f.claimRotation(t, "credential.prepare")
	for _, version := range []int64{0, 1, 3} {
		err := f.commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: r.PrepareCommandID, Status: "applied", AppliedAt: &r.CreatedAt, CredentialVersion: version})
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatal("wrong prepared version accepted", err)
		}
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: r.PrepareCommandID, Status: "applied", AppliedAt: &r.OverlapExpiresAt, CredentialVersion: 2}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("expired source application accepted", err)
	}
	if f.rotationStatus(t).State != "preparing" {
		t.Fatal("invalid receipt changed operation")
	}
}

func testRotationCapabilities(t *testing.T, f rotationFixture) {
	f.issueRotation(t)
	f.issue(t)
	if c, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{}); err != nil || c != nil {
		t.Fatal("unsupported command dispatched", err)
	}
	c, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true})
	if err != nil || c == nil || c.Kind != "alert.ack" {
		t.Fatal("unsupported pending rotation starved ACK", err)
	}
	f.claimRotation(t, "credential.prepare")
}

func testRotationRetention(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	f.prepareRotation(t, r)
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_commands SET retain_until = ? WHERE command_id = ?", time.Now().UTC().Add(-time.Hour), r.PrepareCommandID); err != nil {
		t.Fatal(err)
	}
	f.issue(t) // Normal ledger reservation runs retention cleanup.
	selected, err := f.commands.SelectCredentialConnection(t.Context(), f.session)
	if err != nil || selected.Candidate == nil {
		t.Fatal("retention discarded unresolved rotation dependency", err)
	}
	if _, err := f.commands.GetProtectedCommand(t.Context(), f.session.HubID, f.session.ProbeID, r.PrepareCommandID); err != nil {
		t.Fatal("unresolved original prepare lost", err)
	}
}

func testRotationConcurrent(t *testing.T, f rotationFixture) {
	var wg sync.WaitGroup
	results := make(chan *domain.ProbeCredentialRotation, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.service.Issue(t.Context(), domain.ProbeCredentialRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: rotationTestID, CredentialVersion: 2})
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *domain.ProbeCredentialRotation
	for r := range results {
		if first == nil {
			first = r
		} else if !reflect.DeepEqual(first, r) {
			t.Fatal("parallel retry generated different operation")
		}
	}
	if replayCount(t, f.f, "probe_commands") != 2 || replayCount(t, f.f, "probe_credential_rotations") != 1 {
		t.Fatal("parallel issue duplicated effect")
	}
}

func testRotationMigration(t *testing.T, f rotationFixture) {
	r := f.issueRotation(t)
	if err := runNamedMigration(t, f.f, "062_probe_credential_rotation", "down"); err == nil {
		t.Fatal("downgrade discarded candidate")
	}
	if got := f.rotationStatus(t); !reflect.DeepEqual(got, r) {
		t.Fatal("guard damaged rotation")
	}
	for _, table := range []string{"probe_credential_rotations", "probe_commands"} {
		if _, err := f.f.db.ExecContext(context.Background(), "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	if err := runNamedMigration(t, f.f, "062_probe_credential_rotation", "down"); err == nil {
		t.Fatal("downgrade discarded failed version high-water")
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_connections SET credential_high_water = credential_version"); err != nil {
		t.Fatal(err)
	}
	if err := runNamedMigration(t, f.f, "062_probe_credential_rotation", "down"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runNamedMigration(t, f.f, "062_probe_credential_rotation", "up"); err != nil {
			t.Error(err)
		}
	})
}

func testRotationFinalIssuanceRollback(t *testing.T, f rotationFixture) {
	cleanup := injectOutboxFailure(t, f.f, "probe_connections", "UPDATE")
	_, err := f.service.Issue(t.Context(), domain.ProbeCredentialRotationIssue{HubID: f.session.HubID, ProbeID: f.session.ProbeID, RotationID: rotationTestID, CredentialVersion: 2})
	cleanup()
	if err == nil || replayCount(t, f.f, "probe_commands") != 0 || replayCount(t, f.f, "probe_credential_rotations") != 0 {
		t.Fatal("final issuance failure leaked operation", err)
	}
	f.issueRotation(t)
}
