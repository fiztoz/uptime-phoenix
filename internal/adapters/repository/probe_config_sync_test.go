package repository_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestRemoteConfigSyncContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"SourceEditsAndRestart":         testRemoteSyncSource,
				"ConcurrentPublication":         testRemoteSyncConcurrent,
				"ReceiptFencingAndLateRollback": testRemoteSyncReceipt,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func remoteSyncFixture(t *testing.T, f probeRegistryFixture) (*repository.RemoteProbeConfigSyncStore, ports.ProbeConfigProtector, int64, int64) {
	t.Helper()
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = repository.NewProbeInstallationStore(f.db).Initialize(t.Context(), domain.ProbeInstallation{HubID: probeRegistryID2, KeyHash: protector.KeyHash(probeRegistryID2), ProtocolFloor: 1, AuthorityEpoch: 1, CreatedAt: now, UpdatedAt: now}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.remote(t, probeRegistryID1, "sync-edge")
	monitor := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(t.Context(), monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(t.Context(), monitor, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	channel := f.notification(t, f.user(t))
	if _, err := f.db.ExecContext(t.Context(), "UPDATE notifications SET include_ack_url = ?, config = ? WHERE id = ?", true, `{"url":"https://example.test/hook","token":"private-sync-fixture"}`, channel); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE monitors SET active = ?, cert_expiry_notify = ? WHERE id = ?", true, false, monitor); err != nil {
		t.Fatal(err)
	}
	insertConfigModel(t, f.db, &repository.MonitorNotificationModel{MonitorID: monitor, NotificationID: channel, IncludeTarget: false})
	return repository.NewRemoteProbeConfigSyncStore(f.db, probe.RemoteConfigEncoder{}, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector), protector, monitor, channel
}

func syncTarget() domain.ProbeConfigTarget {
	return domain.ProbeConfigTarget{HubID: probeRegistryID2, ProbeID: probeRegistryID1}
}

func testRemoteSyncSource(t *testing.T, f probeRegistryFixture) {
	store, protector, monitor, channel := remoteSyncFixture(t, f)
	ctx := t.Context()
	at := time.Now().UTC()
	meta, err := store.RefreshRemote(ctx, syncTarget(), at)
	if err != nil || meta.Revision != 1 {
		t.Fatalf("initial publication: %+v %v", meta, err)
	}
	prepared := repository.NewProbeConfigStore(f.db)
	first, err := prepared.Latest(ctx, probeRegistryID1)
	if err != nil || bytes.Contains(first.ProtectedPayload, []byte("private-sync-fixture")) {
		t.Fatal("desired snapshot missing or not protected")
	}
	plain, err := protector.Open(ctx, meta, first.ProtectedPayload)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := probe.DecodeConfigSnapshot(plain)
	if err != nil || snapshot.ProbeID != probeRegistryID1 || len(snapshot.Assignments) != 1 || snapshot.Assignments[0].MonitorID != monitor || snapshot.NotificationChannels[0].IncludeAckURL || snapshot.Assignments[0].NotificationLinks[0].IncludeTarget {
		t.Fatal("remote graph changed authorization or direct delivery preferences")
	}
	clear(plain)
	reopened := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f), probe.RemoteConfigEncoder{}, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	again, err := reopened.RefreshRemote(ctx, syncTarget(), at.Add(time.Hour))
	if err != nil || !domain.SameProbeConfigMetadata(meta, again) {
		t.Fatal("restart created a spurious revision", err)
	}
	retained, err := prepared.Latest(ctx, probeRegistryID1)
	if err != nil || !bytes.Equal(first.ProtectedPayload, retained.ProtectedPayload) {
		t.Fatal("no-op rewrote ciphertext")
	}
	// Direct saved writes simulate another API process or a lost event-bus hint.
	if _, err := f.db.ExecContext(ctx, "UPDATE notifications SET active = ? WHERE id = ?", false, channel); err != nil {
		t.Fatal(err)
	}
	next, err := reopened.RefreshRemote(ctx, syncTarget(), at.Add(time.Second))
	if err != nil || next.Revision != 2 || next.SHA256 == meta.SHA256 {
		t.Fatal("saved dependency edit did not publish", err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET type = ? WHERE id = ?", "push", monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.RefreshRemote(ctx, syncTarget(), at); err == nil {
		t.Fatal("unsupported assignment silently omitted")
	}
	latest, err := prepared.Latest(ctx, probeRegistryID1)
	if err != nil || latest.Revision != 2 {
		t.Fatal("failed validation changed durable desired state")
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET type = ? WHERE id = ?", "http", monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(ctx, monitor, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	empty, err := store.RefreshRemote(ctx, syncTarget(), at)
	if err != nil || empty.Revision != 3 {
		t.Fatal("removal did not publish empty authorized graph", err)
	}
	latest, err = prepared.Latest(ctx, probeRegistryID1)
	if err != nil {
		t.Fatal(err)
	}
	plain, err = protector.Open(ctx, empty, latest.ProtectedPayload)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = probe.DecodeConfigSnapshot(plain)
	clear(plain)
	if err != nil || len(snapshot.Assignments) != 0 || len(snapshot.NotificationChannels) != 0 {
		t.Fatal("removed work or secrets survived desired snapshot")
	}
	wrong, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{38}, 32))
	if err != nil {
		t.Fatal(err)
	}
	wrongStore := repository.NewRemoteProbeConfigSyncStore(f.db, probe.RemoteConfigEncoder{}, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), wrong)
	if _, err := wrongStore.RefreshRemote(ctx, syncTarget(), at); !errors.Is(err, domain.ErrProbeKeyMismatch) {
		t.Fatal("foreign key published desired state", err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE probes SET enabled = ? WHERE id = ?", false, probeRegistryID1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RefreshRemote(ctx, syncTarget(), at); err == nil {
		t.Fatal("disabled registration published")
	}
}

func testRemoteSyncConcurrent(t *testing.T, f probeRegistryFixture) {
	store, protector, _, _ := remoteSyncFixture(t, f)
	other := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f), probe.RemoteConfigEncoder{}, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	var joined sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, candidate := range []*repository.RemoteProbeConfigSyncStore{store, other} {
		joined.Add(1)
		go func() {
			defer joined.Done()
			<-start
			meta, err := candidate.RefreshRemote(t.Context(), syncTarget(), time.Now().UTC())
			if err == nil && meta.Revision != 1 {
				err = fmt.Errorf("duplicate revision %d", meta.Revision)
			}
			results <- err
		}()
	}
	close(start)
	joined.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	count, err := f.db.NewSelect().Table("probe_config_snapshots").Where("probe_id = ?", probeRegistryID1).Count(t.Context())
	if err != nil || count != 1 {
		t.Fatal("competing workers created duplicate work")
	}
}

func testRemoteSyncReceipt(t *testing.T, f probeRegistryFixture) {
	store, _, _, channel := remoteSyncFixture(t, f)
	ctx := t.Context()
	meta, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	leases := repository.NewProbeConnectorStore(f.db)
	lease, err := leases.AcquireConnector(ctx, probeRegistryID1, probeRegistryID3)
	if err != nil {
		t.Fatal(err)
	}
	receipt := domain.ProbeActiveConfig{ProbeConfigTarget: syncTarget(), Revision: meta.Revision, SHA256: meta.SHA256, AssignmentCount: 1, AppliedAt: time.Now().UTC()}
	bad := receipt
	bad.AssignmentCount++
	if err := store.RecordRemoteApplied(ctx, lease, bad); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("wrong assignment count accepted", err)
	}
	active := repository.NewProbeActivationStore(f.db, probe.LocalConfigEncoder{})
	if _, err := active.GetActive(ctx, probeRegistryID1); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("prepared meant applied")
	}
	// Fail the final receipt insert after the active pointer has already changed.
	trigger := "CREATE TRIGGER fail_remote_receipt BEFORE INSERT ON probe_config_applied_receipts BEGIN SELECT RAISE(ABORT, 'private forced receipt failure'); END"
	if f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_remote_receipt BEFORE INSERT ON probe_config_applied_receipts FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'private forced receipt failure'"
	}
	if _, err := f.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_remote_receipt") })
	if err := store.RecordRemoteApplied(ctx, lease, receipt); !errors.Is(err, domain.ErrInternal) {
		t.Fatal("late write failed open or leaked driver error", err)
	}
	if _, err := active.GetActive(ctx, probeRegistryID1); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("late failure left applied pointer")
	}
	if _, err := f.db.ExecContext(ctx, "DROP TRIGGER fail_remote_receipt"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemoteApplied(ctx, lease, receipt); err != nil {
		t.Fatal(err)
	}
	first, err := active.GetReceipt(ctx, probeRegistryID1, 1)
	if err != nil {
		t.Fatal(err)
	}
	receipt.AppliedAt = receipt.AppliedAt.Add(time.Hour)
	if err := store.RecordRemoteApplied(ctx, lease, receipt); err != nil {
		t.Fatal("lost receipt retry failed", err)
	}
	again, err := active.GetReceipt(ctx, probeRegistryID1, 1)
	if err != nil || !first.AppliedAt.Equal(again.AppliedAt) {
		t.Fatal("retry rewrote receipt identity")
	}
	newLease, err := leases.AcquireConnector(ctx, probeRegistryID1, probeRegistryID3)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemoteApplied(ctx, lease, receipt); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale session granted application authority", err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE notifications SET name = ? WHERE id = ?", "new saved label", channel); err != nil {
		t.Fatal(err)
	}
	meta2, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
	if err != nil || meta2.Revision != 2 {
		t.Fatal("changed source not published", err)
	}
	receipt2 := receipt
	receipt2.Revision, receipt2.SHA256 = meta2.Revision, meta2.SHA256
	if err := store.RecordRemoteApplied(ctx, newLease, receipt2); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemoteApplied(ctx, newLease, receipt); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("old receipt regressed applied revision", err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = 0 WHERE probe_id = ?", probeRegistryID1); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemoteApplied(ctx, newLease, receipt2); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired owner accepted even an idempotent receipt", err)
	}
}
