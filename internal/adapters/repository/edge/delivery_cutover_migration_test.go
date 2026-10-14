package edge

// Migration 017 effects: the receipt ledger keeps every populated column across
// both directions, kind-specific cutover success codes are enforced per kind,
// legacy uncertain availability I/O is retained as ambiguity without inventing
// success, and downgrade refuses to erase live gates or unresolved journals.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func execCutoverMigration(t *testing.T, s *Store, name string) error {
	t.Helper()
	data, err := migrations.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return s.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, string(data))
		return err
	})
}

// TestEdgeDeliveryCutoverMigrationPreservesPopulatedLedger round-trips 017 with
// a populated old ledger and enforces kind-specific success codes afterwards.
func TestEdgeDeliveryCutoverMigrationPreservesPopulatedLedger(t *testing.T) {
	s, _ := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	rec := checkRecord()
	if _, err := s.CommitEdgeCheck(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	authority := domain.EdgeCommandAuthority{HubID: testHubID, ProbeID: testIdentity().ProbeID, StreamID: testIdentity().StreamID, ConnectionGeneration: 1}
	now := time.Now().UTC().Truncate(time.Microsecond)
	note := "migration fixture"
	ack := domain.ProbeAlertAcknowledgement{CommandID: uuid.NewString(), ProbeID: authority.ProbeID,
		SourceAlertID: rec.Incident.SourceAlertID, AssignmentGeneration: 1, CreatedAt: now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Hour), ActorDisplayName: "Operator", Note: &note, PayloadHash: [32]byte{1}}
	if _, err := s.ApplyAlertAcknowledgement(t.Context(), authority, ack); err != nil {
		t.Fatal(err)
	}
	// A populated preparation receipt and tuple, plus a populated certificate
	// receipt, must survive the ledger rebuild untouched.
	prepareID, certID := uuid.NewString(), uuid.NewString()
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_applied_commands
		(command_id, kind, request_hash, status, applied_at, code, message, retain_until)
		VALUES (?, 'delivery.prepare', zeroblob(32), 'applied', 1, 'delivery_prepared', '', 9999999999999999)`, prepareID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_delivery_preparations
		(command_id, hub_id, probe_id, stream_id, monitor_id, assignment_revision, generation, config_revision, sha256, alert_delivery, prepared_at)
		VALUES (?, ?, ?, ?, 17, 9007199254740993, 1, 1, ?, 'aggregate', 1)`,
		prepareID, testHubID, testIdentity().ProbeID, testIdentity().StreamID,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_applied_commands
		(command_id, kind, request_hash, status, applied_at, code, message, retain_until, certificate_version, certificate_fingerprint, certificate_not_after)
		VALUES (?, 'certificate.prepare', zeroblob(32), 'applied', 2, '', '', 9999999999999999, 2, ?, 3)`,
		certID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	var before, preparedBefore, certBefore edgeCommandRow
	for id, row := range map[string]*edgeCommandRow{ack.CommandID: &before, prepareID: &preparedBefore, certID: &certBefore} {
		if err := s.db.NewSelect().Model(row).Where("command_id = ?", id).Scan(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var tupleBefore, tupleAfter struct {
		AssignmentRevision int64
		AlertDelivery      string
		PreparedAt         int64
	}
	if err := s.db.NewRaw("SELECT assignment_revision, alert_delivery, prepared_at FROM edge_delivery_preparations WHERE command_id = ?", prepareID).Scan(t.Context(), &tupleBefore); err != nil {
		t.Fatal(err)
	}

	if err := execCutoverMigration(t, s, "017_delivery_cutover.tx.down.sql"); err != nil {
		t.Fatal(err)
	}
	if err := execCutoverMigration(t, s, "017_delivery_cutover.tx.up.sql"); err != nil {
		t.Fatal(err)
	}

	var after, preparedAfter, certAfter edgeCommandRow
	for id, row := range map[string]*edgeCommandRow{ack.CommandID: &after, prepareID: &preparedAfter, certID: &certAfter} {
		if err := s.db.NewSelect().Model(row).Where("command_id = ?", id).Scan(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if !sameMigrationReceipt(before, after) || !sameMigrationReceipt(preparedBefore, preparedAfter) {
		t.Fatal("migration changed an existing command receipt")
	}
	if certAfter.CertificateVersion == nil || *certAfter.CertificateVersion != 2 || certAfter.CertificateFingerprint == nil ||
		*certAfter.CertificateFingerprint != *certBefore.CertificateFingerprint || certAfter.CertificateNotAfter == nil ||
		*certAfter.CertificateNotAfter != *certBefore.CertificateNotAfter {
		t.Fatalf("migration lost populated certificate columns: %+v", certAfter)
	}
	if err := s.db.NewRaw("SELECT assignment_revision, alert_delivery, prepared_at FROM edge_delivery_preparations WHERE command_id = ?", prepareID).Scan(t.Context(), &tupleAfter); err != nil || tupleAfter != tupleBefore {
		t.Fatalf("migration lost the preparation tuple: %+v vs %+v %v", tupleAfter, tupleBefore, err)
	}
	// Kind-specific success stays bound to its own kind in both directions.
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_applied_commands SET code = 'delivery_quiesced' WHERE command_id = ?", ack.CommandID); err == nil {
		t.Fatal("old command acquired a cutover success code")
	}
	if _, err := s.db.ExecContext(t.Context(), "UPDATE edge_applied_commands SET code = '' WHERE command_id = ?", prepareID); err == nil {
		t.Fatal("preparation success lost its kind-specific code")
	}
	quiesceID := uuid.NewString()
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_applied_commands
		(command_id, kind, request_hash, status, applied_at, code, message, retain_until)
		VALUES (?, 'delivery.quiesce', zeroblob(32), 'applied', 1, 'delivery_quiesced', '', 9999999999999999)`, quiesceID); err != nil {
		t.Fatalf("cutover kind-specific success rejected: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_applied_commands
		(command_id, kind, request_hash, status, applied_at, code, message, retain_until)
		VALUES (?, 'delivery.apply', zeroblob(32), 'applied', 1, '', '', 9999999999999999)`, uuid.NewString()); err == nil {
		t.Fatal("code-empty cutover success accepted")
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_applied_commands
		(command_id, kind, request_hash, status, applied_at, code, message, retain_until)
		VALUES (?, 'delivery.resume', zeroblob(32), 'applied', 1, 'delivery_applied', '', 9999999999999999)`, uuid.NewString()); err == nil {
		t.Fatal("mismatched cutover success code accepted")
	}
}

// cloneDelivery copies one availability work row into another lifecycle state so
// a migration can be driven against the exact legacy shapes upgrade must read.
func cloneDelivery(t *testing.T, s *Store, from, to, status, errorCode string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO edge_delivery_outbox
		(delivery_id, source_alert_id, source_transition_version, notification_id, notification_version, event_kind,
		 monitor_id, generation, source_seq, config_revision, check_status, check_output, observed_at, incident_status,
		 started_at, resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at,
		 created_at, escalation_policy_id, escalation_step)
		SELECT ?, source_alert_id, source_transition_version, notification_id, notification_version, event_kind,
		 monitor_id, generation, source_seq, config_revision, check_status, check_output, observed_at, incident_status,
		 started_at, resolved_at, available_at, ?, 1, '', NULL, NULL, ?, 1, created_at, escalation_policy_id, escalation_step
		FROM edge_delivery_outbox WHERE delivery_id = ?`, to, status, errorCode, from); err != nil {
		t.Fatal(err)
	}
}

// TestEdgeDeliveryCutoverMigrationLegacyAmbiguity proves upgrade retains legacy
// attempted/leased availability I/O as ambiguity, never as fabricated success,
// and that downgrade then refuses to erase it.
func TestEdgeDeliveryCutoverMigrationLegacyAmbiguity(t *testing.T) {
	s, _ := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	if err := s.ActivateConfig(t.Context(), protectedConfig(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
		t.Fatal(err)
	}
	leased := cutoverClaim(t, s, testIdentity().ProbeID) // unfinished leased attempt.
	retrying, failed, sent, superseded := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	cloneDelivery(t, s, leased.DeliveryID, retrying, domain.DeliveryStatusRetrying, domain.ErrCodeProviderError)
	cloneDelivery(t, s, leased.DeliveryID, failed, domain.DeliveryStatusFailed, domain.ErrCodeProviderError)
	cloneDelivery(t, s, leased.DeliveryID, sent, domain.DeliveryStatusSent, "")
	cloneDelivery(t, s, leased.DeliveryID, superseded, domain.DeliveryStatusSuperseded, "")

	if err := execCutoverMigration(t, s, "017_delivery_cutover.tx.down.sql"); err != nil {
		t.Fatal(err)
	}
	if err := execCutoverMigration(t, s, "017_delivery_cutover.tx.up.sql"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{leased.DeliveryID, retrying, failed} {
		var state string
		if err := s.db.NewRaw("SELECT state FROM edge_provider_attempts WHERE delivery_id = ?", id).Scan(t.Context(), &state); err != nil || state != string(domain.ProviderAttemptAmbiguous) {
			t.Fatalf("legacy uncertain I/O not retained as ambiguity for %s: %q %v", id, state, err)
		}
	}
	if n := certCount(t, s, "SELECT COUNT(*) FROM edge_provider_attempts"); n != 3 {
		t.Fatalf("migration invented provider outcomes: %d", n)
	}
	for _, id := range []string{sent, superseded} {
		if n := certCount(t, s, "SELECT COUNT(*) FROM edge_provider_attempts WHERE delivery_id = '"+id+"'"); n != 0 {
			t.Fatalf("proven outcome reinterpreted as ambiguity for %s: %d", id, n)
		}
	}
	// Ambiguity is unresolved history: downgrade must refuse to erase it.
	if err := execCutoverMigration(t, s, "017_delivery_cutover.tx.down.sql"); err == nil {
		t.Fatal("downgrade erased unresolved provider ambiguity")
	}
}

// TestEdgeDeliveryCutoverMigrationDowngradeRefusals proves downgrade refuses
// live source gates and durable cutover receipts, and only then restores the
// older ledger.
func TestEdgeDeliveryCutoverMigrationDowngradeRefusals(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	if err := execCutoverMigration(t, h.store, "017_delivery_cutover.tx.down.sql"); err == nil {
		t.Fatal("downgrade erased a live source gate")
	}
	requireCutover(t, h.execute(h.command(domain.DeliveryResumeKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryResumedCode)
	if err := execCutoverMigration(t, h.store, "017_delivery_cutover.tx.down.sql"); err == nil {
		t.Fatal("downgrade erased durable cutover receipts")
	}
	if _, err := h.store.db.ExecContext(t.Context(), "DELETE FROM edge_applied_commands WHERE kind IN ('delivery.quiesce','delivery.apply','delivery.resume')"); err != nil {
		t.Fatal(err)
	}
	if err := execCutoverMigration(t, h.store, "017_delivery_cutover.tx.down.sql"); err != nil {
		t.Fatalf("downgrade refused an unbound ledger: %v", err)
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'edge_delivery_cutover_gates'"); n != 0 {
		t.Fatal("released gate table survived a clean downgrade")
	}
}
