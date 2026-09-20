package repository_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type commandFixture struct {
	replayFixture
	commands  *repository.ProbeCommandStore
	protector *auth.ProbeConfigProtector
	request   domain.ProtectedProbeCommand
	ack       domain.ProbeAlertAcknowledgement
}

func newCommandFixture(t *testing.T, engine string) commandFixture {
	t.Helper()
	r := newReplayFixture(t, engine)
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	connections := repository.NewProbeConnectorStore(r.f.db)
	c, err := connections.GetConnection(t.Context(), r.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := connections.ActivateConnection(t.Context(), c.ProbeID, c.EnrollmentID, c.CredentialVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	out := r.ingest(t, r.batch(r.incident(1, 1)))
	if out.AcceptedCount != 1 {
		t.Fatalf("source incident not mirrored: %+v", out)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	note := "Private operator context"
	ack := domain.ProbeAlertAcknowledgement{CommandID: "99999999-9999-4999-8999-999999999999", ProbeID: r.session.ProbeID, SourceAlertID: r.incident(1, 1).Incident.SourceAlertID, AssignmentGeneration: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), ActorDisplayName: "Operator", Note: &note}
	f := commandFixture{replayFixture: r, commands: repository.NewProbeCommandStore(r.f.db, p, probe.AcknowledgementCodec{}, p, probe.CredentialCommandCodec{}), protector: p, ack: ack}
	f.request = f.protect(t, ack)
	return f
}

func (f commandFixture) protect(t *testing.T, c domain.ProbeAlertAcknowledgement) domain.ProtectedProbeCommand {
	t.Helper()
	payload, err := (probe.AcknowledgementCodec{}).EncodeAcknowledgement(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	meta := domain.ProbeCommandMetadata{CommandID: c.CommandID, HubID: f.session.HubID, ProbeID: c.ProbeID, StreamID: f.session.StreamID, Kind: "alert.ack", SourceAlertID: &c.SourceAlertID, AssignmentGeneration: &c.AssignmentGeneration, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, PayloadSHA256: hex.EncodeToString(sum[:])}
	protected, err := f.protector.SealCommand(t.Context(), meta, payload)
	if err != nil {
		t.Fatal(err)
	}
	return domain.ProtectedProbeCommand{ProbeCommandMetadata: meta, ProtectedPayload: protected}
}

func (f commandFixture) issue(t *testing.T) *domain.ProbeCommand {
	t.Helper()
	out, err := f.commands.CreateCommand(t.Context(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || out.RemoteConfirmed || out.Attempts != 0 {
		t.Fatalf("issuance fabricated application: %+v", out)
	}
	return out
}

func TestProbeCommandStorage(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, commandFixture){
				"ImmutableIssuanceAndResults": testCommandIdentity,
				"FencingAndReconnection":      testCommandFences,
				"LateWriteRollback":           testCommandRollback,
				"CompetingClaims":             testCommandConcurrent,
				"ExpiredRequestStillRetries":  testCommandExpiry,
				"MigrationGuards":             testCommandMigration,
			} {
				t.Run(name, func(t *testing.T) { test(t, newCommandFixture(t, engine)) })
			}
		})
	}
}

func testCommandIdentity(t *testing.T, f commandFixture) {
	want := f.issue(t)
	// A fresh AEAD nonce for the same exact request must preserve original bytes.
	duplicate := f.protect(t, f.ack)
	got, err := f.commands.CreateCommand(t.Context(), duplicate)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("duplicate issuance changed metadata", err)
	}
	stored, err := f.commands.GetProtectedCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || !bytes.Equal(stored.ProtectedPayload, f.request.ProtectedPayload) || bytes.Contains(stored.ProtectedPayload, []byte(*f.ack.Note)) {
		t.Fatal("ciphertext replaced or plaintext stored", err)
	}
	changed := f.ack
	changed.ActorDisplayName = "Another operator"
	if _, err := f.commands.CreateCommand(t.Context(), f.protect(t, changed)); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("changed command reused UUID", err)
	}
	if _, err := f.commands.GetCommand(t.Context(), probeRegistryID3, f.session.ProbeID, f.ack.CommandID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("foreign hub read command", err)
	}
	result := domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "applied", AppliedAt: &f.ack.CreatedAt, Message: "Incident acknowledged"}
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("unsent command was confirmed", err)
	}
	claimed, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true})
	if err != nil || claimed == nil || !bytes.Equal(claimed.ProtectedPayload, f.request.ProtectedPayload) {
		t.Fatal("claim lost exact request", err)
	}
	if next, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err != nil || next != nil {
		t.Fatal("backoff was ignored", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); err != nil {
		t.Fatal(err)
	}
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); err != nil {
		t.Fatal("identical result failed", err)
	}
	got, err = f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || !got.RemoteConfirmed || got.Attempts != 1 || got.Outcome == nil || !reflect.DeepEqual(*got.Outcome, result) {
		t.Fatalf("durable result lost: %+v %v", got, err)
	}
	result.Status = "already_resolved"
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("contradictory result replaced history", err)
	}
	if next, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err != nil || next != nil {
		t.Fatal("confirmed request retried", err)
	}
}

func testCommandFences(t *testing.T, f commandFixture) {
	f.issue(t)
	for name, change := range map[string]func(*domain.ProbeReplaySession){
		"owner":      func(s *domain.ProbeReplaySession) { s.OwnerID = s.HubID },
		"generation": func(s *domain.ProbeReplaySession) { s.ConnectionGeneration++ },
		"stream":     func(s *domain.ProbeReplaySession) { s.StreamID = s.HubID },
		"hub":        func(s *domain.ProbeReplaySession) { s.HubID = s.ProbeID },
	} {
		t.Run(name, func(t *testing.T) {
			bad := f.session
			change(&bad)
			if _, err := f.commands.ClaimCommand(t.Context(), bad, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err == nil {
				t.Fatal("foreign session dispatched")
			}
		})
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET lease_until = ? WHERE probe_id = ?", time.Now().Unix()+2, f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commands.ClaimCommand(t.Context(), f.session, 10*time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("insufficient lease dispatched", err)
	}
	status, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.Attempts != 0 {
		t.Fatal("failed authority advanced retry", err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET lease_until = 0 WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired owner dispatched", err)
	}
	connections := repository.NewProbeConnectorStore(f.f.db)
	lease, err := connections.AcquireConnector(t.Context(), f.session.ProbeID, probeRegistryID2)
	if err != nil {
		t.Fatal(err)
	}
	fresh := f.session
	fresh.OwnerID, fresh.ConnectionGeneration = lease.OwnerID, lease.Generation
	if got, err := f.commands.ClaimCommand(t.Context(), fresh, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err != nil || got == nil {
		t.Fatal("new owner did not recover", err)
	}
	result := domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "applied", AppliedAt: &f.ack.CreatedAt, Message: "Incident acknowledged"}
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale result committed", err)
	}
	if err := f.commands.CompleteCommand(t.Context(), fresh, result); err != nil {
		t.Fatal("new owner did not recover result", err)
	}
}

func testCommandRollback(t *testing.T, f commandFixture) {
	cleanup := injectOutboxFailure(t, f.f, "probe_commands", "INSERT")
	if out, err := f.commands.CreateCommand(t.Context(), f.request); err == nil || out != nil {
		t.Fatal("failed issuance reported queued")
	}
	cleanup()
	if _, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("failed issuance survived", err)
	}
	f.issue(t)
	cleanup = injectOutboxFailure(t, f.f, "probe_commands", "UPDATE")
	if out, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err == nil || out != nil {
		t.Fatal("failed claim authorized send")
	}
	cleanup()
	status, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.Attempts != 0 {
		t.Fatal("failed claim advanced attempts", err)
	}
	if _, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err != nil {
		t.Fatal(err)
	}
	cleanup = injectOutboxFailure(t, f.f, "probe_commands", "UPDATE")
	result := domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "applied", AppliedAt: &f.ack.CreatedAt, Message: "Incident acknowledged"}
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); err == nil {
		t.Fatal("failed result reported confirmed")
	}
	cleanup()
	status, err = f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.RemoteConfirmed || status.Outcome != nil {
		t.Fatal("failed result committed", err)
	}
}

func testCommandConcurrent(t *testing.T, f commandFixture) {
	var peerDB *bun.DB
	var err error
	if f.f.engine == "mariadb" {
		peerDB, err = mariadb.NewDB(f.f.dsn)
	} else {
		peerDB, err = sqlite.NewDB(f.f.dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peerDB.Close() }()
	peer := repository.NewProbeCommandStore(peerDB, f.protector, probe.AcknowledgementCodec{}, f.protector, probe.CredentialCommandCodec{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, s := range []*repository.ProbeCommandStore{f.commands, peer} {
		wg.Go(func() { _, err := s.CreateCommand(t.Context(), f.request); results <- err })
	}
	wg.Wait()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("competing identical issuance failed", err)
		}
	}
	claims := make(chan *domain.ProtectedProbeCommand, 2)
	for _, s := range []*repository.ProbeCommandStore{f.commands, peer} {
		wg.Go(func() {
			out, err := s.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true})
			claims <- out
			results <- err
		})
	}
	wg.Wait()
	n := 0
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
		if <-claims != nil {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("same due attempt claimed %d times", n)
	}
	status, err := peer.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.Attempts != 1 {
		t.Fatal("competing claim count wrong", err)
	}
}

func testCommandExpiry(t *testing.T, f commandFixture) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	f.ack.CreatedAt, f.ack.ExpiresAt = now, now.Add(3*time.Second)
	f.request = f.protect(t, f.ack)
	f.issue(t)
	if _, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true}); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(f.ack.ExpiresAt.Add(20 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	claimed, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true})
	if err != nil || claimed == nil || !bytes.Equal(claimed.ProtectedPayload, f.request.ProtectedPayload) {
		t.Fatal("expiry discarded an unconfirmed receipt", err)
	}
	// The edge applied before expiry, but this source confirmation was lost.
	result := domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "applied", AppliedAt: &now, Message: "Incident acknowledged"}
	if err := f.commands.CompleteCommand(t.Context(), f.session, result); err != nil {
		t.Fatal("late durable result lost", err)
	}
	status, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.Status != "applied" || !status.RemoteConfirmed {
		t.Fatal("hub guessed expired instead of recovering receipt", err)
	}
}

func testCommandMigration(t *testing.T, f commandFixture) {
	f.issue(t)
	if err := runNamedMigration(t, f.f, "061_probe_commands", "down"); err == nil {
		t.Fatal("downgrade discarded a protected request")
	}
	status, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
	if err != nil || status.Status != "pending" {
		t.Fatal("failed downgrade damaged ledger", err)
	}
	if _, err := f.f.db.ExecContext(context.Background(), "DELETE FROM probe_commands"); err != nil {
		t.Fatal(err)
	}
	if err := runNamedMigration(t, f.f, "061_probe_commands", "down"); err != nil {
		t.Fatal("empty ledger failed downgrade", err)
	}
	if err := runNamedMigration(t, f.f, "061_probe_commands", "up"); err != nil {
		t.Fatal("ledger failed re-upgrade", err)
	}
	f.issue(t)
}
