package repository_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type preparationFixture struct {
	remoteRegionalFixture
	protector *auth.ProbeConfigProtector
}

func newPreparationFixture(t *testing.T, engine string) preparationFixture {
	t.Helper()
	f := newRemoteRegionalFixture(t, engine, true)
	p := f.syncerProtector(t)
	f.syncer.SetDeliveryPreparations(probe.DeliveryPreparationCodec{}, p)
	return preparationFixture{remoteRegionalFixture: f, protector: p}
}

// rebuildGraph replaces the desired set with mode, republishes the document,
// acknowledges it and stores the source's exact regional report for that
// document. The returned fact is the immutable report binding.
func (f preparationFixture) rebuildGraph(t *testing.T, mode domain.AlertDelivery) domain.ProbeDeliveryModeApplication {
	t.Helper()
	ctx := t.Context()
	assignments := repository.NewProbeAssignmentStore(f.f.db)
	current, err := assignments.GetByMonitorID(ctx, f.monitor)
	if err != nil {
		t.Fatal(err)
	}
	set, err := assignments.ReplaceWithBindings(ctx, f.monitor, current.Revision, []string{f.session.ProbeID}, current.HealthPolicy, mode, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Assignments) != 1 {
		t.Fatalf("unexpected assignment set: %+v", set)
	}
	meta, err := f.syncer.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	lease := domain.ProbeConnectorLease{ProbeID: f.session.ProbeID, OwnerID: f.session.OwnerID, Generation: f.session.ConnectionGeneration}
	if err := f.syncer.RecordRemoteApplied(ctx, lease, domain.ProbeActiveConfig{ProbeConfigTarget: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AssignmentCount: len(set.Assignments), AppliedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	fact := domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  meta.ProbeConfigTarget,
		MonitorID:          f.monitor,
		AssignmentRevision: set.Revision,
		Generation:         set.Assignments[0].Generation,
		ConfigRevision:     meta.Revision,
		SHA256:             meta.SHA256,
		StreamID:           f.session.StreamID,
		AlertDelivery:      domain.AlertDeliveryRegional,
	}
	if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{fact}); err != nil {
		t.Fatal("exact authenticated report refused", err)
	}
	return fact
}

func (f preparationFixture) issue(t *testing.T, facts ...domain.ProbeDeliveryModeApplication) error {
	t.Helper()
	return f.syncer.IssueRemoteDeliveryPreparations(t.Context(), f.session, facts)
}

type preparationCommandRow struct {
	CommandID            string
	Kind                 string
	RequestedBy          int64
	SourceAlertID        *string
	AssignmentGeneration *int64
	HubID                string
	ProbeID              string
	StreamID             string
	PayloadSHA256        string `bun:"payload_sha256"`
	ProtectedPayload     []byte
	Status               string
	RemoteConfirmed      bool
	Attempts             int64
	CreatedAt            time.Time
	ExpiresAt            time.Time
}

func (f preparationFixture) wantRows(t *testing.T, want int) []preparationCommandRow {
	t.Helper()
	var rows []preparationCommandRow
	if err := f.f.db.NewSelect().Table("probe_commands").
		Column("command_id", "kind", "requested_by", "source_alert_id", "assignment_generation", "hub_id", "probe_id", "stream_id", "payload_sha256", "protected_payload", "status", "remote_confirmed", "attempts", "created_at", "expires_at").
		OrderExpr("command_id ASC").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != want {
		t.Fatalf("probe_commands rows = %d, want %d", len(rows), want)
	}
	return rows
}

func (f preparationFixture) commands(t *testing.T) *repository.ProbeCommandStore {
	t.Helper()
	return repository.NewProbeCommandStore(f.f.db, f.protector, probe.AcknowledgementCodec{}, f.protector, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
}

// foreignPreparationRow exists only to plant a colliding command ID whose
// protected payload carries a different semantic tuple.
type foreignPreparationRow struct {
	bun.BaseModel        `bun:"table:probe_commands"`
	CommandID            string `bun:"command_id,pk"`
	RequestedBy          int64
	HubID                string `bun:"hub_id,nullzero"`
	ProbeID              string
	StreamID             string `bun:"stream_id,nullzero"`
	Kind                 string
	SourceAlertID        *string
	AssignmentGeneration *int64
	CreatedAt            time.Time
	ExpiresAt            time.Time
	PayloadSHA256        string `bun:"payload_sha256,nullzero"`
	ProtectedPayload     []byte
	Status               string
	RemoteConfirmed      bool
	Attempts             int64
	UpdatedAt            time.Time
}

// delayedPreparationCodec lets a DB-clock lease expire while the transaction is
// still open, exercising the final expiry fence.
type delayedPreparationCodec struct {
	ports.ProbeDeliveryPreparationCodec
	before func(context.Context) error
}

func (d delayedPreparationCodec) EncodeDeliveryPreparation(ctx context.Context, p domain.ProbeDeliveryPreparation) ([]byte, error) {
	if err := d.before(ctx); err != nil {
		return nil, err
	}
	return d.ProbeDeliveryPreparationCodec.EncodeDeliveryPreparation(ctx, p)
}

func (d delayedPreparationCodec) DecodeDeliveryPreparation(ctx context.Context, payload []byte) (domain.ProbeDeliveryPreparation, error) {
	if err := d.before(ctx); err != nil {
		return domain.ProbeDeliveryPreparation{}, err
	}
	return d.ProbeDeliveryPreparationCodec.DecodeDeliveryPreparation(ctx, payload)
}

func TestDeliveryPreparationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, test := range []struct {
				name string
				run  func(*testing.T, preparationFixture)
			}{
				{"Positive", testDeliveryPreparationPositive},
				{"Prerequisites", testDeliveryPreparationPrerequisites},
				{"CurrentABA", testDeliveryPreparationABA},
				{"ImmutableDuplicates", testDeliveryPreparationDuplicates},
				{"CapabilityStarvation", testDeliveryPreparationStarvation},
				{"StaleSessionExpiryRollback", testDeliveryPreparationFences},
				{"ServiceDispatch", testDeliveryPreparationService},
			} {
				t.Run(test.name, func(t *testing.T) { test.run(t, newPreparationFixture(t, engine)) })
			}
		})
	}
}

func testDeliveryPreparationPositive(t *testing.T, f preparationFixture) {
	ctx := t.Context()
	regional := f.rebuildGraph(t, domain.AlertDeliveryRegional)
	if err := f.issue(t, regional); err != nil {
		t.Fatal("regional member issuance failed instead of doing nothing", err)
	}
	f.wantRows(t, 0) // Regional members never receive a preparation request.
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal("current aggregate member did not issue", err)
	}
	rows := f.wantRows(t, 1)
	row := rows[0]
	want := domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  syncTarget(),
		MonitorID:          fact.MonitorID,
		AssignmentRevision: fact.AssignmentRevision,
		Generation:         fact.Generation,
		ConfigRevision:     fact.ConfigRevision,
		SHA256:             fact.SHA256,
		StreamID:           f.session.StreamID,
		AlertDelivery:      domain.AlertDeliveryAggregate,
	}
	if row.Kind != "delivery.prepare" || row.RequestedBy != 0 || row.SourceAlertID != nil || row.AssignmentGeneration == nil || *row.AssignmentGeneration != fact.Generation ||
		row.HubID != want.HubID || row.ProbeID != want.ProbeID || row.StreamID != want.StreamID || len(row.PayloadSHA256) != 64 ||
		row.Status != "pending" || row.RemoteConfirmed || row.Attempts != 0 || !domain.ValidHubID(row.CommandID) {
		t.Fatalf("issued request has the wrong immutable shape: %+v", row)
	}
	if !row.ExpiresAt.Equal(row.CreatedAt.Add(24*time.Hour)) || row.CreatedAt.IsZero() || bytes.Contains(row.ProtectedPayload, []byte("aggregate")) {
		t.Fatal("issued request has the wrong lifetime or unprotected bytes", row.CreatedAt, row.ExpiresAt)
	}
	commands := f.commands(t)
	if err := commands.CompleteCommand(ctx, f.session, domain.ProbeCommandOutcome{CommandID: row.CommandID, Status: "applied", AppliedAt: &row.CreatedAt, Code: "delivery_prepared"}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("unsent preparation was confirmed", err)
	}
	claimed, err := commands.ClaimCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true})
	if err != nil || claimed == nil || claimed.Kind != "delivery.prepare" || claimed.CommandID != row.CommandID {
		t.Fatal("claim lost the exact request", claimed, err)
	}
	plain, err := f.protector.OpenCommand(ctx, claimed.ProbeCommandMetadata, claimed.ProtectedPayload)
	if err != nil {
		t.Fatal(err)
	}
	preparation, err := (probe.DeliveryPreparationCodec{}).DecodeDeliveryPreparation(ctx, plain)
	if err != nil || preparation.ProbeDeliveryModeApplication != want || preparation.CommandID != row.CommandID || hex.EncodeToString(preparation.PayloadHash[:]) != row.PayloadSHA256 {
		t.Fatalf("dispatched request lost its exact binding: %+v %v", preparation, err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	for _, result := range []domain.ProbeCommandOutcome{
		{CommandID: row.CommandID, Status: "applied", AppliedAt: &at},
		{CommandID: row.CommandID, Status: "applied", AppliedAt: &at, Code: "mode_applied"},
		{CommandID: row.CommandID, Status: "applied", AppliedAt: &at, Code: "delivery_prepared", CredentialVersion: 1},
		{CommandID: row.CommandID, Status: "applied", AppliedAt: &at, Code: "delivery_prepared", CertificateVersion: 2, CertificateFingerprint: "a", CertificateNotAfter: &at},
		{CommandID: row.CommandID, Status: "rejected"},
	} {
		if err := commands.CompleteCommand(ctx, f.session, result); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("unbounded preparation receipt accepted: %+v %v", result, err)
		}
	}
	applied := domain.ProbeCommandOutcome{CommandID: row.CommandID, Status: "applied", AppliedAt: &at, Code: "delivery_prepared", Message: "Prepared"}
	if err := commands.CompleteCommand(ctx, f.session, applied); err != nil {
		t.Fatal("delivery_prepared receipt refused", err)
	}
	if err := commands.CompleteCommand(ctx, f.session, applied); err != nil {
		t.Fatal("identical duplicate receipt failed", err)
	}
	contradictory := applied
	contradictory.Status = "already_applied"
	if err := commands.CompleteCommand(ctx, f.session, contradictory); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("contradictory receipt replaced history", err)
	}
	got, err := commands.GetCommand(ctx, f.session.HubID, f.session.ProbeID, row.CommandID)
	if err != nil || !got.RemoteConfirmed || got.Outcome == nil || got.Outcome.Code != "delivery_prepared" || got.Outcome.CredentialVersion != 0 || got.Outcome.CertificateVersion != 0 {
		t.Fatalf("durable receipt lost or overclaimed: %+v %v", got, err)
	}
	if next, err := commands.ClaimCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true}); err != nil || next != nil {
		t.Fatal("confirmed request was retried", err)
	}
}

func testDeliveryPreparationPrerequisites(t *testing.T, f preparationFixture) {
	ctx := t.Context()
	// Negotiation order is not a stored regional report.
	if err := f.issue(t, f.fact); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("unreported member issued a preparation", err)
	}
	f.wantRows(t, 0)
	f.record(t)
	if _, err := f.f.db.ExecContext(ctx, "DELETE FROM probe_config_applied_receipts WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	// An active pointer without its committed receipt is not authority.
	if err := f.issue(t, f.fact); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("active pointer substituted for the committed receipt", err)
	}
	f.wantRows(t, 0)
	f.acknowledge(t)
	var payload []byte
	if err := f.f.db.NewSelect().Table("probe_config_snapshots").Column("protected_payload").Where("probe_id = ? AND revision = ?", f.session.ProbeID, f.fact.ConfigRevision).Scan(ctx, &payload); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(payload)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ? AND revision = ?", corrupt, f.session.ProbeID, f.fact.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	// Corruption is a redacted error, never success and never a repair.
	if err := f.issue(t, f.fact); err == nil {
		t.Fatal("corrupt retained document issued a preparation")
	}
	f.wantRows(t, 0)
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ? AND revision = ?", payload, f.session.ProbeID, f.fact.ConfigRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.f.db.ExecContext(ctx, "DELETE FROM probe_active_configs WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, f.fact); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("missing active pointer issued a preparation", err)
	}
	f.wantRows(t, 0)
}

func testDeliveryPreparationABA(t *testing.T, f preparationFixture) {
	ctx := t.Context()
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	assignments := repository.NewProbeAssignmentStore(f.f.db)
	// The desired set moves on after the report: the legitimate historical
	// report issues nothing and its session stays alive.
	if _, err := assignments.ReplaceWithBindings(ctx, f.monitor, fact.AssignmentRevision, []string{f.session.ProbeID}, domain.HealthPolicyAllDown, domain.AlertDeliveryBoth, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, fact); err != nil {
		t.Fatal("stale set closed a legitimate report session", err)
	}
	f.wantRows(t, 0)
	// The same session still issues for the current set/document.
	current := f.rebuildGraph(t, domain.AlertDeliveryBoth)
	if err := f.issue(t, current); err != nil {
		t.Fatal("current member did not issue after the ABA", err)
	}
	rows := f.wantRows(t, 1)
	if rows[0].AssignmentGeneration == nil || *rows[0].AssignmentGeneration != current.Generation {
		t.Fatal("issued request borrowed a stale generation", rows[0])
	}
	// A stale document after the ABA issues nothing instead of failing.
	if err := f.issue(t, fact); err != nil {
		t.Fatal("stale document closed a legitimate report session", err)
	}
	f.wantRows(t, 1)
}

func testDeliveryPreparationDuplicates(t *testing.T, f preparationFixture) {
	ctx := t.Context()
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	rows := f.wantRows(t, 1)
	original := rows[0]
	// A colliding command ID with a different semantic tuple is refused, and the
	// stored request is never rewritten into agreement.
	foreign := fact
	foreign.MonitorID++
	bound := domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  syncTarget(),
		MonitorID:          foreign.MonitorID,
		AssignmentRevision: foreign.AssignmentRevision,
		Generation:         foreign.Generation,
		ConfigRevision:     foreign.ConfigRevision,
		SHA256:             foreign.SHA256,
		StreamID:           f.session.StreamID,
		AlertDelivery:      domain.AlertDeliveryAggregate,
	}
	prepared := domain.ProbeDeliveryPreparation{
		ProbeDeliveryModeApplication: bound,
		CommandID:                    original.CommandID,
		CreatedAt:                    time.Now().UTC().Truncate(time.Microsecond),
	}
	prepared.ExpiresAt = prepared.CreatedAt.Add(time.Hour)
	payload, err := (probe.DeliveryPreparationCodec{}).EncodeDeliveryPreparation(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	generation := prepared.Generation
	meta := domain.ProbeCommandMetadata{CommandID: prepared.CommandID, HubID: syncTarget().HubID, ProbeID: syncTarget().ProbeID, StreamID: f.session.StreamID,
		Kind: "delivery.prepare", AssignmentGeneration: &generation, CreatedAt: prepared.CreatedAt, ExpiresAt: prepared.ExpiresAt, PayloadSHA256: hex.EncodeToString(sum[:])}
	protected, err := f.protector.SealCommand(ctx, meta, payload)
	if err != nil {
		t.Fatal(err)
	}
	// Test-only collision replacement must remove its exact request reference
	// first; production retention deliberately refuses deleting a bound command.
	if _, err := f.f.db.ExecContext(ctx, "DELETE FROM probe_delivery_preparation_requests"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.f.db.ExecContext(ctx, "DELETE FROM probe_commands"); err != nil {
		t.Fatal(err)
	}
	row := foreignPreparationRow{CommandID: meta.CommandID, RequestedBy: 0, HubID: meta.HubID, ProbeID: meta.ProbeID, StreamID: meta.StreamID,
		Kind: meta.Kind, AssignmentGeneration: meta.AssignmentGeneration, CreatedAt: meta.CreatedAt, ExpiresAt: meta.ExpiresAt,
		PayloadSHA256: meta.PayloadSHA256, ProtectedPayload: protected, Status: "pending", UpdatedAt: meta.CreatedAt}
	if _, err := f.f.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, fact); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("foreign semantic tuple reused the deterministic ID", err)
	}
	f.wantRows(t, 1)
	if _, err := f.f.db.ExecContext(ctx, "DELETE FROM probe_commands"); err != nil {
		t.Fatal(err)
	}
	// The deterministic ID is stable across issuances.
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	rows = f.wantRows(t, 1)
	if rows[0].CommandID != original.CommandID {
		t.Fatal("command identity is not bound to the exact tuple", rows[0].CommandID, original.CommandID)
	}
	original = rows[0]
	// A reconnect may re-issue the same request; protected bytes, creation
	// time, expiry and provenance stay immutable.
	connector := repository.NewProbeConnectorStore(f.f.db)
	if err := connector.ReleaseConnector(ctx, domain.ProbeConnectorLease{ProbeID: f.session.ProbeID, OwnerID: f.session.OwnerID, Generation: f.session.ConnectionGeneration}); err != nil {
		t.Fatal(err)
	}
	lease, err := connector.AcquireConnector(ctx, f.session.ProbeID, f.session.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	f.session.ConnectionGeneration = lease.Generation
	reopened := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f.f), probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), f.syncerProtector(t))
	reopened.SetDeliveryPreparations(probe.DeliveryPreparationCodec{}, f.protector)
	if err := reopened.IssueRemoteDeliveryPreparations(ctx, f.session, []domain.ProbeDeliveryModeApplication{fact}); err != nil {
		t.Fatal("reconnect lost the immutable request", err)
	}
	rows = f.wantRows(t, 1)
	if rows[0].CommandID != original.CommandID || rows[0].RequestedBy != 0 || !rows[0].CreatedAt.Equal(original.CreatedAt) || !rows[0].ExpiresAt.Equal(original.ExpiresAt) || !bytes.Equal(rows[0].ProtectedPayload, original.ProtectedPayload) {
		t.Fatal("duplicate issuance rewrote bytes, time, expiry or provenance")
	}
}

func testDeliveryPreparationStarvation(t *testing.T, f preparationFixture) {
	ctx := t.Context()
	connections := repository.NewProbeConnectorStore(f.f.db)
	connection, err := connections.GetConnection(ctx, f.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := connections.ActivateConnection(ctx, connection.ProbeID, connection.EnrollmentID, connection.CredentialVersion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if out := f.ingest(t, f.batch(f.incident(1, 1))); out.AcceptedCount != 1 {
		t.Fatalf("source incident not mirrored: %+v", out)
	}
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	rows := f.wantRows(t, 1)
	prepareDue := rows[0].CreatedAt
	// A supported alert.ack becomes due after the unsupported-at-claim-time
	// preparation; the claim order must stay capability-gated, not FIFO.
	source := f.incident(1, 1).Incident.SourceAlertID
	now := time.Now().UTC().Truncate(time.Microsecond)
	ack := domain.ProbeAlertAcknowledgement{CommandID: "99999999-9999-4999-8999-999999999999", ProbeID: f.session.ProbeID,
		SourceAlertID: source, AssignmentGeneration: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), ActorDisplayName: "Operator"}
	payload, err := (probe.AcknowledgementCodec{}).EncodeAcknowledgement(ctx, ack)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	generation := ack.AssignmentGeneration
	meta := domain.ProbeCommandMetadata{CommandID: ack.CommandID, HubID: f.session.HubID, ProbeID: ack.ProbeID, StreamID: f.session.StreamID,
		Kind: "alert.ack", SourceAlertID: &source, AssignmentGeneration: &generation, CreatedAt: ack.CreatedAt, ExpiresAt: ack.ExpiresAt, PayloadSHA256: hex.EncodeToString(sum[:])}
	protected, err := f.protector.SealCommand(ctx, meta, payload)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := f.commands(t).CreateCommand(ctx, domain.ProtectedProbeCommand{ProbeCommandMetadata: meta, ProtectedPayload: protected})
	if err != nil {
		t.Fatal(err)
	}
	if prepareDue.After(issued.CreatedAt) {
		t.Fatal("preparation is not the earlier due request", prepareDue, issued.CreatedAt)
	}
	claimed, err := f.commands(t).ClaimCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{AlertAcknowledgement: true})
	if err != nil || claimed == nil || claimed.Kind != "alert.ack" {
		t.Fatal("unsupported pending preparation starved a supported claim", claimed, err)
	}
	prepared, err := f.commands(t).ClaimCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true})
	if err != nil || prepared == nil || prepared.Kind != "delivery.prepare" || prepared.CommandID != rows[0].CommandID {
		t.Fatal("capability-gated preparation claim failed", prepared, err)
	}
	if none, err := f.commands(t).ClaimCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{}); err != nil || none != nil {
		t.Fatal("ungated claim dispatched an unsupported command", none, err)
	}
}

func testDeliveryPreparationFences(t *testing.T, f preparationFixture) {
	ctx := t.Context()
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	for name, mutate := range map[string]func(*domain.ProbeReplaySession){
		"owner":      func(s *domain.ProbeReplaySession) { s.OwnerID = probeRegistryID1 },
		"generation": func(s *domain.ProbeReplaySession) { s.ConnectionGeneration++ },
		"hub":        func(s *domain.ProbeReplaySession) { s.HubID = probeRegistryID3 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := f.session
			mutate(&bad)
			if err := f.syncer.IssueRemoteDeliveryPreparations(ctx, bad, []domain.ProbeDeliveryModeApplication{fact}); err == nil {
				t.Fatal("stored session authority ignored", name)
			}
			f.wantRows(t, 0)
		})
	}
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = 1 WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, fact); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired owner issued a preparation", err)
	}
	f.wantRows(t, 0)
	// The final DB-clock fence commits nothing when the lease expires mid-batch.
	query := "UPDATE probe_sessions SET lease_until = unixepoch()+1 WHERE probe_id = ?"
	if f.f.engine == "mariadb" {
		query = "UPDATE probe_sessions SET lease_until = UNIX_TIMESTAMP()+1 WHERE probe_id = ?"
	}
	if _, err := f.f.db.ExecContext(ctx, query, f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	delayed := delayedPreparationCodec{ProbeDeliveryPreparationCodec: probe.DeliveryPreparationCodec{}, before: func(ctx context.Context) error {
		timer := time.NewTimer(1100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}
	expiring := repository.NewRemoteProbeConfigSyncStore(f.f.db, probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), f.syncerProtector(t))
	expiring.SetDeliveryPreparations(delayed, f.protector)
	if err := expiring.IssueRemoteDeliveryPreparations(ctx, f.session, []domain.ProbeDeliveryModeApplication{fact}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("lease expired during encode but a request committed", err)
	}
	f.wantRows(t, 0)
	// A failing member rolls back the whole batch, including earlier inserts.
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = ? WHERE probe_id = ?", time.Now().Unix()+3600, f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	missing := fact
	missing.MonitorID++
	if err := f.issue(t, fact, missing); err == nil {
		t.Fatal("incoherent batch issued a partial request set")
	}
	f.wantRows(t, 0)
	clear := injectOutboxFailure(t, f.f, "probe_commands", "INSERT")
	err := f.issue(t, fact)
	clear()
	if !errors.Is(err, domain.ErrInternal) {
		t.Fatal("failed insert was not redacted/refused", err)
	}
	f.wantRows(t, 0)
	if err := f.issue(t, fact); err != nil {
		t.Fatal("rollback damaged later issuance", err)
	}
	f.wantRows(t, 1)
}

func testDeliveryPreparationService(t *testing.T, f preparationFixture) {
	ctx := t.Context()
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	rows := f.wantRows(t, 1)
	connections := repository.NewProbeConnectorStore(f.f.db)
	bare, err := services.NewProbeCommandService(f.commands(t), connections, f.protector, probe.AcknowledgementCodec{}, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.NextCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true}); !errors.Is(err, domain.ErrValidation) {
		t.Fatal("unconfigured codec dispatched a preparation", err)
	}
	// The refused dispatch consumed its retry attempt without changing the
	// immutable request; make it due again instead of rewriting it.
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_commands SET next_attempt_at = created_at WHERE command_id = ?", rows[0].CommandID); err != nil {
		t.Fatal(err)
	}
	bare.SetDeliveryPreparationsCodec(probe.DeliveryPreparationCodec{})
	dispatch, err := bare.NextCommand(ctx, f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true})
	if err != nil || dispatch == nil || dispatch.Metadata.Kind != "delivery.prepare" || dispatch.Metadata.CommandID != rows[0].CommandID {
		t.Fatal("dispatch lost the preparation request", dispatch, err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	reconnect, err := bare.RecordCommandResult(ctx, f.session, domain.ProbeCommandOutcome{CommandID: rows[0].CommandID, Status: "applied", AppliedAt: &at, Code: "delivery_prepared", Message: "Prepared"})
	if err != nil || reconnect {
		t.Fatal("preparation result forced reauthentication or failed", reconnect, err)
	}
	got, err := f.commands(t).GetCommand(ctx, f.session.HubID, f.session.ProbeID, rows[0].CommandID)
	if err != nil || !got.RemoteConfirmed || got.Outcome == nil || got.Outcome.Code != "delivery_prepared" {
		t.Fatalf("durable result lost: %+v %v", got, err)
	}
}
