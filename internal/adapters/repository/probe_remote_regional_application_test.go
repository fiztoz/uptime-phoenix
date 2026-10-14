package repository_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type remoteRegionalFixture struct {
	replayFixture
	metadata domain.ProbeConfigMetadata
	fact     domain.ProbeDeliveryModeApplication
}

func newRemoteRegionalFixture(t *testing.T, engine string, acknowledge bool) remoteRegionalFixture {
	t.Helper()
	r := newReplayFixture(t, engine)
	snapshot, err := repository.NewProbeConfigStore(r.f.db).Latest(t.Context(), r.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	f := remoteRegionalFixture{replayFixture: r, metadata: snapshot.ProbeConfigMetadata}
	f.fact = domain.ProbeDeliveryModeApplication{ProbeConfigTarget: snapshot.ProbeConfigTarget, MonitorID: r.monitor, AssignmentRevision: 2, Generation: 1, ConfigRevision: snapshot.Revision, SHA256: snapshot.SHA256, StreamID: r.session.StreamID, AlertDelivery: domain.AlertDeliveryRegional}
	if acknowledge {
		f.acknowledge(t)
	}
	return f
}

func (f remoteRegionalFixture) acknowledge(t *testing.T) {
	t.Helper()
	lease := domain.ProbeConnectorLease{ProbeID: f.session.ProbeID, OwnerID: f.session.OwnerID, Generation: f.session.ConnectionGeneration}
	if err := f.syncer.RecordRemoteApplied(t.Context(), lease, domain.ProbeActiveConfig{ProbeConfigTarget: f.metadata.ProbeConfigTarget, Revision: f.metadata.Revision, SHA256: f.metadata.SHA256, AssignmentCount: 1, AppliedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func (f remoteRegionalFixture) record(t *testing.T) {
	t.Helper()
	if err := f.syncer.RecordRemoteRegionalApplications(t.Context(), f.session, []domain.ProbeDeliveryModeApplication{f.fact}); err != nil {
		t.Fatal("exact authenticated report refused", err)
	}
}

func TestRemoteRegionalApplicationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, test := range []struct {
				name string
				run  func(*testing.T, remoteRegionalFixture)
			}{
				{"ReceiptPrerequisite", testRemoteRegionalPrerequisite},
				{"ImmutableReconnectAndABA", testRemoteRegionalImmutable},
				{"TupleAndBatchRollback", testRemoteRegionalTuple},
				{"CorruptRetainedAndDuplicate", testRemoteRegionalCorruption},
				{"ConcurrentDuplicate", testRemoteRegionalConcurrent},
				{"LeaseExpiresDuringDecode", testRemoteRegionalExpiry},
				{"CancellationAndRedaction", testRemoteRegionalCancellation},
				{"Migration", testRemoteRegionalMigration},
			} {
				t.Run(test.name, func(t *testing.T) {
					test.run(t, newRemoteRegionalFixture(t, engine, test.name != "ReceiptPrerequisite"))
				})
			}
			for _, name := range []string{"owner", "generation", "expiry", "disabled", "revoked", "stream", "retired", "key", "hub"} {
				t.Run("SessionFence/"+name, func(t *testing.T) { testRemoteRegionalFence(t, newRemoteRegionalFixture(t, engine, true), name) })
			}
			t.Run("ResetPending", func(t *testing.T) {
				f := newStreamResetFixture(t, engine)
				meta, err := repository.NewProbeConfigStore(f.f.db).Latest(t.Context(), f.session.ProbeID)
				if err != nil {
					t.Fatal(err)
				}
				r := remoteRegionalFixture{replayFixture: f.replayFixture, metadata: meta.ProbeConfigMetadata, fact: domain.ProbeDeliveryModeApplication{ProbeConfigTarget: meta.ProbeConfigTarget, MonitorID: f.monitor, AssignmentRevision: 2, Generation: 1, ConfigRevision: meta.Revision, SHA256: meta.SHA256, StreamID: f.session.StreamID, AlertDelivery: domain.AlertDeliveryRegional}}
				r.acknowledge(t)
				f.prepare(t)
				if err := r.syncer.RecordRemoteRegionalApplications(t.Context(), r.session, []domain.ProbeDeliveryModeApplication{r.fact}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("reset-pending source reported application", err)
				}
				assertTableCount(t, r.f, "probe_remote_regional_applications", 0)
			})
		})
	}
}

func testRemoteRegionalPrerequisite(t *testing.T, f remoteRegionalFixture) {
	ctx := t.Context()
	if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("uncommitted receipt authorized a report", err)
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
	f.acknowledge(t)
	// A pointer alone is not proof that its receipt is committed.
	if _, err := f.f.db.ExecContext(ctx, "DELETE FROM probe_config_applied_receipts WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("active pointer substituted for receipt", err)
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
	f.acknowledge(t)
	f.record(t)
	assertTableCount(t, f.f, "probe_remote_regional_applications", 1)
}

func testRemoteRegionalImmutable(t *testing.T, f remoteRegionalFixture) {
	ctx := t.Context()
	f.record(t)
	var original struct {
		RecordedAt           time.Time
		ConnectionGeneration int64
		AssignmentRevision   int64
	}
	if err := f.f.db.NewSelect().Table("probe_remote_regional_applications").Column("recorded_at", "connection_generation", "assignment_revision").Scan(ctx, &original); err != nil {
		t.Fatal(err)
	}
	assignmentStore := repository.NewProbeAssignmentStore(f.f.db)
	if _, err := assignmentStore.ReplaceWithBindings(ctx, f.monitor, 2, []string{f.session.ProbeID}, domain.HealthPolicyAllDown, domain.AlertDeliveryAggregate, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := assignmentStore.ReplaceWithBindings(ctx, f.monitor, 3, []string{f.session.ProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryRegional, nil); err != nil {
		t.Fatal(err)
	}
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
	if err := reopened.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact}); err != nil {
		t.Fatal("historical document binding lost on reconnect", err)
	}
	var after struct {
		RecordedAt           time.Time
		ConnectionGeneration int64
		AssignmentRevision   int64
	}
	if err := f.f.db.NewSelect().Table("probe_remote_regional_applications").Column("recorded_at", "connection_generation", "assignment_revision").Scan(ctx, &after); err != nil {
		t.Fatal(err)
	}
	if original.RecordedAt.IsZero() || !after.RecordedAt.Equal(original.RecordedAt) || after.ConnectionGeneration != original.ConnectionGeneration || after.AssignmentRevision != 2 || after.ConnectionGeneration == lease.Generation {
		t.Fatal("retry rewrote first time/provenance or borrowed current binding")
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 1)
}

func (f remoteRegionalFixture) syncerProtector(t *testing.T) *auth.ProbeConfigProtector {
	t.Helper()
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testRemoteRegionalTuple(t *testing.T, f remoteRegionalFixture) {
	ctx := t.Context()
	for _, change := range []struct {
		name string
		edit func(*domain.ProbeDeliveryModeApplication)
	}{
		{"monitor", func(a *domain.ProbeDeliveryModeApplication) { a.MonitorID++ }},
		{"binding", func(a *domain.ProbeDeliveryModeApplication) { a.AssignmentRevision++ }},
		{"generation", func(a *domain.ProbeDeliveryModeApplication) { a.Generation++ }},
		{"revision", func(a *domain.ProbeDeliveryModeApplication) { a.ConfigRevision++ }},
		{"hash", func(a *domain.ProbeDeliveryModeApplication) { a.SHA256 = strings.Repeat("a", 64) }},
		{"stream", func(a *domain.ProbeDeliveryModeApplication) { a.StreamID = probeRegistryID3 }},
		{"target", func(a *domain.ProbeDeliveryModeApplication) { a.HubID = probeRegistryID3 }},
		{"aggregate", func(a *domain.ProbeDeliveryModeApplication) { a.AlertDelivery = domain.AlertDeliveryAggregate }},
		{"both", func(a *domain.ProbeDeliveryModeApplication) { a.AlertDelivery = domain.AlertDeliveryBoth }},
		{"unbound", func(a *domain.ProbeDeliveryModeApplication) { a.AssignmentRevision = 0 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			bad := f.fact
			change.edit(&bad)
			if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{bad}); err == nil {
				t.Fatal("mismatched tuple accepted independently", change.name)
			}
			// Validation of the second entry must roll back a first valid insert.
			if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact, bad}); err == nil {
				t.Fatal("incoherent batch accepted")
			}
			assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
		})
	}
	if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact, f.fact}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("duplicate member accepted", err)
	}
	for _, facts := range [][]domain.ProbeDeliveryModeApplication{nil, make([]domain.ProbeDeliveryModeApplication, 257)} {
		if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, facts); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("unbounded report accepted", err)
		}
	}
	clear := injectOutboxFailure(t, f.f, "probe_remote_regional_applications", "INSERT")
	err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact})
	clear()
	if !errors.Is(err, domain.ErrInternal) {
		t.Fatal("failed insert was not redacted/refused", err)
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
	f.record(t)
}

func testRemoteRegionalFence(t *testing.T, f remoteRegionalFixture, name string) {
	ctx := t.Context()
	store := f.syncer
	session := f.session
	var statement string
	switch name {
	case "owner":
		session.OwnerID = probeRegistryID1
	case "generation":
		session.ConnectionGeneration++
	case "expiry":
		statement = "UPDATE probe_sessions SET lease_until = 1 WHERE probe_id = ?"
	case "disabled":
		statement = "UPDATE probes SET enabled = FALSE WHERE id = ?"
	case "revoked":
		statement = "UPDATE probes SET revoked_at = CURRENT_TIMESTAMP WHERE id = ?"
	case "stream":
		session.StreamID = probeRegistryID3
	case "retired":
		statement = "UPDATE probe_streams SET retired_at = CURRENT_TIMESTAMP WHERE probe_id = ?"
	case "key":
		wrong, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{38}, 32))
		if err != nil {
			t.Fatal(err)
		}
		store = repository.NewRemoteProbeConfigSyncStore(f.f.db, probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), wrong)
	case "hub":
		session.HubID = probeRegistryID3
	default:
		t.Fatal("unknown fence")
	}
	if statement != "" {
		if _, err := f.f.db.ExecContext(ctx, statement, f.session.ProbeID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordRemoteRegionalApplications(ctx, session, []domain.ProbeDeliveryModeApplication{f.fact}); err == nil {
		t.Fatal("stored session authority ignored", name)
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
}

func testRemoteRegionalCorruption(t *testing.T, f remoteRegionalFixture) {
	ctx := t.Context()
	var payload []byte
	if err := f.f.db.NewSelect().Table("probe_config_snapshots").Column("protected_payload").Where("probe_id = ? AND revision = ?", f.session.ProbeID, f.metadata.Revision).Scan(ctx, &payload); err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(payload)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ? AND revision = ?", corrupt, f.session.ProbeID, f.metadata.Revision); err != nil {
		t.Fatal(err)
	}
	if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact}); err == nil {
		t.Fatal("corrupt retained bytes reported application")
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ? AND revision = ?", payload, f.session.ProbeID, f.metadata.Revision); err != nil {
		t.Fatal(err)
	}
	f.record(t)
	if _, err := f.f.db.ExecContext(ctx, "UPDATE probe_remote_regional_applications SET assignment_revision = assignment_revision + 1 WHERE probe_id = ?", f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("corrupt duplicate was silently repaired", err)
	}
	var binding int64
	if err := f.f.db.NewSelect().Table("probe_remote_regional_applications").Column("assignment_revision").Scan(ctx, &binding); err != nil || binding != f.fact.AssignmentRevision+1 {
		t.Fatal("duplicate changed existing corrupt row", err)
	}
}

func testRemoteRegionalConcurrent(t *testing.T, f remoteRegionalFixture) {
	other := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f.f), probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), f.syncerProtector(t))
	start := make(chan struct{})
	results := make(chan error, 2)
	var joined sync.WaitGroup
	for _, store := range []*repository.RemoteProbeConfigSyncStore{f.syncer, other} {
		joined.Add(1)
		go func() {
			defer joined.Done()
			<-start
			results <- store.RecordRemoteRegionalApplications(t.Context(), f.session, []domain.ProbeDeliveryModeApplication{f.fact})
		}()
	}
	close(start)
	joined.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent duplicate failed", err)
		}
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 1)
}

type regionalReportDecoder struct {
	delegate ports.EdgeConfigDecoder
	before   func(context.Context) error
}

func (d regionalReportDecoder) DecodeEdge(ctx context.Context, data []byte, target domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	if err := d.before(ctx); err != nil {
		return nil, err
	}
	return d.delegate.DecodeEdge(ctx, data, target)
}

func testRemoteRegionalExpiry(t *testing.T, f remoteRegionalFixture) {
	query := "UPDATE probe_sessions SET lease_until = unixepoch()+1 WHERE probe_id = ?"
	if f.f.engine == "mariadb" {
		query = "UPDATE probe_sessions SET lease_until = UNIX_TIMESTAMP()+1 WHERE probe_id = ?"
	}
	if _, err := f.f.db.ExecContext(t.Context(), query, f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	decoder := regionalReportDecoder{delegate: probe.NewHubConfigDecoder(checker.Get, notifier.Get), before: func(ctx context.Context) error {
		timer := time.NewTimer(1100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}
	store := repository.NewRemoteProbeConfigSyncStore(f.f.db, probe.RemoteConfigEncoder{}, decoder, f.syncerProtector(t))
	if err := store.RecordRemoteRegionalApplications(t.Context(), f.session, []domain.ProbeDeliveryModeApplication{f.fact}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("lease expired during decode but report committed", err)
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
}

func testRemoteRegionalCancellation(t *testing.T, f remoteRegionalFixture) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.syncer.RecordRemoteRegionalApplications(ctx, f.session, []domain.ProbeDeliveryModeApplication{f.fact}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled report was not refused", err)
	}
	const privateFailure = "private-regional-decoder-detail"
	decoder := regionalReportDecoder{delegate: probe.NewHubConfigDecoder(checker.Get, notifier.Get), before: func(context.Context) error { return errors.New(privateFailure) }}
	store := repository.NewRemoteProbeConfigSyncStore(f.f.db, probe.RemoteConfigEncoder{}, decoder, f.syncerProtector(t))
	err := store.RecordRemoteRegionalApplications(t.Context(), f.session, []domain.ProbeDeliveryModeApplication{f.fact})
	if !errors.Is(err, ports.ErrConflict) || strings.Contains(err.Error(), privateFailure) {
		t.Fatal("semantic failure leaked or succeeded")
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
	f.record(t)
}

func testRemoteRegionalMigration(t *testing.T, f remoteRegionalFixture) {
	for _, direction := range []string{"down", "up"} {
		if err := runEngineMigration(t, f.f.db, f.f.engine, "078_remote_regional_application", direction); err != nil {
			t.Fatal(err)
		}
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 0)
	f.record(t)
	for _, set := range []string{"assignment_revision = 0", "generation = 0", "connection_generation = 0", "config_revision = 0", "monitor_id = 0", "sha256 = 'bad'", "alert_delivery = 'aggregate'", "stream_id = '00000000-0000-4000-8000-000000000001'"} {
		if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_remote_regional_applications SET "+set+" WHERE probe_id = ?", f.session.ProbeID); err == nil {
			t.Fatal("schema accepted invalid report", set)
		}
	}
	if _, err := f.f.db.ExecContext(t.Context(), "DELETE FROM probe_config_snapshots WHERE probe_id = ? AND revision = ?", f.session.ProbeID, f.metadata.Revision); err == nil {
		t.Fatal("snapshot deletion discarded reported application")
	}
	assertTableCount(t, f.f, "probe_remote_regional_applications", 1)
	f.record(t)
}
