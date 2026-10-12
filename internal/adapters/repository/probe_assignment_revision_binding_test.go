package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestAssignmentRevisionBindingContract exercises real source/build/protection,
// publication and activation paths. Fixtures are disposable; MariaDB execution
// requires explicit configuration and is never inferred from SQLite passes.
func TestAssignmentRevisionBindingContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				run  func(*testing.T, probeRegistryFixture)
			}{
				{"LocalBoundABA", testLocalAssignmentBindingABA},
				{"StalePreparedNoLegacyFallback", testAssignmentBindingStalePrepared},
				{"LegacyLocalExecutionAndRefresh", testLegacyAssignmentBinding},
				{"RemoteReceiptBinding", testRemoteAssignmentBinding},
				{"InvalidSourceRevision", testInvalidAssignmentSourceRevision},
			} {
				t.Run(tc.name, func(t *testing.T) { tc.run(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func replaceBindingMode(t *testing.T, f probeRegistryFixture, monitor, expected int64, probeID string, mode domain.AlertDelivery) *domain.MonitorProbeAssignments {
	t.Helper()
	set, err := repository.NewProbeAssignmentStore(f.db).ReplaceWithBindings(t.Context(), monitor, expected, []string{probeID}, domain.HealthPolicyAnyDown, mode, nil)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func testLocalAssignmentBindingABA(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	replaceBindingMode(t, f, monitor, 1, domain.LocalProbeID, domain.AlertDeliveryAggregate)
	first := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	activateLocalDelivery(t, f, f.db, first, 0)
	reader := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t))
	assertBinding := func(revision int64) {
		t.Helper()
		got := readLocalDelivery(t, reader)
		if got.Revision != first.Revision || len(got.Assignments) != 1 || got.Assignments[0].AssignmentRevision != revision {
			t.Fatal("receipt document was rebound to mutable assignment revision")
		}
	}
	assertBinding(2) // Document revision one is deliberately not assignment revision two.
	replaceBindingMode(t, f, monitor, 2, domain.LocalProbeID, domain.AlertDeliveryRegional)
	replaceBindingMode(t, f, monitor, 3, domain.LocalProbeID, domain.AlertDeliveryAggregate)
	assertBinding(2)
	// Same monitor/generation/mode/settings after ABA must not make the old bound
	// snapshot executable with set revision four, including via legacy retry.
	execution := activationRepo(f, f.db).(ports.LocalAppliedConfigReader)
	if got, err := execution.ReadAppliedLocal(t.Context()); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("bound stale graph passed normal or legacy hash comparison", err)
	}
	second := prepareLocalDeliveryRevision(t, f, f.db, first.Revision, time.Now().UTC())
	if second.SHA256 == first.SHA256 || second.Revision != first.Revision+1 {
		t.Fatal("ABA set-revision change did not change prepared identity")
	}
	assertBinding(2) // Preparation is not acknowledgement.
	activateLocalDelivery(t, f, f.db, second, first.Revision)
	got := readLocalDelivery(t, reader)
	if got.Revision != second.Revision || got.Assignments[0].AssignmentRevision != 4 || got.Assignments[0].Generation != 1 {
		t.Fatal("new local acknowledgement did not retain exact set binding")
	}
	cold := newLocalDeliveryStore(reopenConfigDB(t, f), nil, localDeliveryProtector(t))
	if readLocalDelivery(t, cold).Assignments[0].AssignmentRevision != 4 {
		t.Fatal("cold local reader lost binding")
	}
}

func testAssignmentBindingStalePrepared(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	first := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC())
	// Policy changes affect the set revision even when local execution fields,
	// generation and desired mode are otherwise unchanged.
	if _, err := repository.NewProbeAssignmentStore(f.db).ReplaceWithBindings(t.Context(), monitor, 1, []string{domain.LocalProbeID}, domain.HealthPolicyAllDown, domain.AlertDeliveryRegional, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := activationRepo(f, f.db).ActivateLocal(t.Context(), ports.LocalActivationParams{
		Target: first.ProbeConfigTarget, Revision: first.Revision, SHA256: first.SHA256,
		ExpectedActiveRevision: 0, AppliedAt: time.Now().UTC(), AssignmentCount: -1,
	}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale prepared binding gained authority through legacy fallback", err)
	}
	if _, err := activationRepo(f, f.db).GetActive(t.Context(), domain.LocalProbeID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("failed activation persisted a pointer", err)
	}
}

func testLegacyAssignmentBinding(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	sourceRepo := repository.NewLocalProbeConfigSourceStore(f.db)
	source, err := sourceRepo.ReadLocal(t.Context())
	if err != nil || source.Assignments[0].AssignmentRevision != 1 {
		t.Fatal("fresh source did not bind set revision", err)
	}
	definition, err := services.ResolveLocalProbeConfig(source)
	if err != nil {
		t.Fatal(err)
	}
	definition.Target, definition.Revision = localDeliveryTarget(), 1
	definition.CreatedAt, definition.EffectiveAt = time.Now().UTC().Truncate(time.Microsecond), time.Now().UTC().Truncate(time.Microsecond)
	definition.Assignments[0].AssignmentRevision = 0
	document, err := (probe.LocalConfigEncoder{}).EncodeLocal(definition)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(document)
	prepared := protectedConfigService(t, configRepo(f, f.db))
	meta, err := prepared.Prepare(t.Context(), definition.Target, document, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit pre-upgrade fixture: old code committed pointer+receipt for these
	// exact legacy bytes. Do not invoke today's new-activation freshness path.
	if _, err := f.db.ExecContext(t.Context(), "INSERT INTO probe_active_configs (probe_id, hub_id, revision, sha256, applied_at, assignment_count) VALUES (?, ?, ?, ?, ?, ?)", domain.LocalProbeID, localDeliveryHubID, 1, meta.SHA256, definition.CreatedAt, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "INSERT INTO probe_config_applied_receipts (probe_id, hub_id, revision, sha256, applied_at, assignment_count, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)", domain.LocalProbeID, localDeliveryHubID, 1, meta.SHA256, definition.CreatedAt, 1, definition.CreatedAt); err != nil {
		t.Fatal(err)
	}
	activation := activationRepo(f, f.db)
	reader := activation.(ports.LocalAppliedConfigReader)
	legacy, err := reader.ReadAppliedLocal(t.Context())
	if err != nil || legacy == nil || legacy.Assignments[0].AssignmentRevision != 0 || len(legacy.RegionalDeliveryApplications) != 0 {
		t.Fatal("legacy hash compatibility invented a current binding or stopped execution", err)
	}
	// Legacy compatibility must still match the entire graph, not just mode or
	// generation. A non-binding edit cannot be rescued by omitting revisions.
	if _, err := f.db.ExecContext(t.Context(), "UPDATE monitors SET name = ? WHERE id = ?", "different legacy graph", monitor); err != nil {
		t.Fatal(err)
	}
	if got, err := reader.ReadAppliedLocal(t.Context()); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("legacy retry ignored a complete-graph mismatch", err)
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE monitors SET name = ? WHERE id = ?", source.Assignments[0].Monitor.Name, monitor); err != nil {
		t.Fatal(err)
	}
	ack := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t))
	if readLocalDelivery(t, ack).Assignments[0].AssignmentRevision != 0 {
		t.Fatal("legacy retained document was enriched")
	}
	validation := services.NewLocalProbeConfigValidationService(prepared, probe.NewLocalConfigValidator(checker.Get, notifier.Get))
	activate := services.NewLocalProbeConfigActivationService(validation, activation)
	refresh := services.NewLocalProbeConfigRefreshService(sourceRepo, probe.LocalConfigEncoder{}, prepared, activate, activation, installationRepo(f, f.db))
	current, err := refresh.Refresh(t.Context())
	if err != nil || current == nil || current.Revision != 2 {
		t.Fatal("upgrade refresher did not publish a new bound document", err)
	}
	if got := readLocalDelivery(t, ack); got.Assignments[0].AssignmentRevision != 1 || got.Assignments[0].MonitorID != monitor {
		t.Fatal("upgraded exact document did not preserve assignment binding")
	}
	old, _, err := prepared.Read(t.Context(), localDeliveryTarget(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(old)
	snapshot, err := probe.DecodeLocalConfigSnapshot(old)
	if err != nil || snapshot.Assignments[0].AssignmentRevision != 0 {
		t.Fatal("upgrade rewrote the retained legacy document", err)
	}
}

func testRemoteAssignmentBinding(t *testing.T, f probeRegistryFixture) {
	store, protector, monitor, _ := remoteSyncFixture(t, f)
	first, err := store.RefreshRemote(t.Context(), syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = remoteAppliedReceipt(t, f, store, first)
	// Return to the exact original regional mode/policy/generation without an
	// intermediate publication. The build-time set binding alone must change.
	replaceBindingMode(t, f, monitor, 2, probeRegistryID1, domain.AlertDeliveryAggregate)
	replaceBindingMode(t, f, monitor, 3, probeRegistryID1, domain.AlertDeliveryRegional)
	second, err := store.RefreshRemote(t.Context(), syncTarget(), time.Now().UTC().Add(-time.Hour))
	if err != nil || second.Revision != first.Revision+1 || second.SHA256 == first.SHA256 {
		t.Fatal("remote ABA/clock rollback failed to publish a distinct binding", err)
	}
	cold := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f), probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)
	got, err := cold.ReadAcknowledgedRemoteDelivery(t.Context(), syncTarget())
	if err != nil || got == nil || got.Revision != first.Revision || got.Assignments[0].AssignmentRevision != 2 {
		t.Fatal("cold receipt read used current/prepared set revision instead of original binding", err)
	}
	// Fresh source and original acknowledged document retain separate identity.
	snapshot, err := repository.NewProbeConfigStore(f.db).Latest(t.Context(), probeRegistryID1)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protector.Open(t.Context(), second, snapshot.ProtectedPayload)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plain)
	document, err := probe.DecodeConfigSnapshot(plain)
	if err != nil || document.Assignments[0].AssignmentRevision != 4 {
		t.Fatal("remote newly prepared source binding mismatch", err)
	}
}

func testInvalidAssignmentSourceRevision(t *testing.T, f probeRegistryFixture) {
	monitor := seedLocalDelivery(t, f)
	// Normal schema constraints already reject zero. Deliberately corrupt only
	// this disposable fixture's single session to exercise the reader's guard.
	f.db.SetMaxOpenConns(1)
	disable, enable := "PRAGMA ignore_check_constraints = ON", "PRAGMA ignore_check_constraints = OFF"
	if f.engine == "mariadb" {
		disable, enable = "SET SESSION check_constraint_checks = OFF", "SET SESSION check_constraint_checks = ON"
	}
	if _, err := f.db.ExecContext(t.Context(), disable); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE monitor_probe_assignment_sets SET revision = 0 WHERE monitor_id = ?", monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), enable); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.NewLocalProbeConfigSourceStore(f.db).ReadLocal(t.Context()); got != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatal("invalid set revision was silently encoded or omitted", err)
	}
}
