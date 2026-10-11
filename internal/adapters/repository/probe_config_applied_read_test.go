package repository_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
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
)

func TestRemoteAppliedConfigContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ImmutableReceiptSelection", func(t *testing.T) { testRemoteAppliedSelection(t, newProbeRegistryFixture(t, engine)) })
			t.Run("ConcurrentReceiptRead", func(t *testing.T) { testRemoteAppliedConcurrent(t, newProbeRegistryFixture(t, engine)) })
			for name, mutate := range map[string]string{
				"MissingReceipt":      "DELETE FROM probe_config_applied_receipts",
				"ReceiptHash":         "UPDATE probe_config_applied_receipts SET sha256 = '" + strings.Repeat("0", 64) + "'",
				"ReceiptHub":          "UPDATE probe_config_applied_receipts SET hub_id = '" + probeRegistryID3 + "'",
				"ReceiptCount":        "UPDATE probe_config_applied_receipts SET assignment_count = 2",
				"ReceiptTime":         "UPDATE probe_config_applied_receipts SET applied_at = '2000-01-01 00:00:00'",
				"SnapshotHash":        "UPDATE probe_config_snapshots SET sha256 = '" + strings.Repeat("0", 64) + "'",
				"SnapshotHub":         "UPDATE probe_config_snapshots SET hub_id = '" + probeRegistryID3 + "'",
				"ActiveCount":         "UPDATE probe_active_configs SET assignment_count = 2",
				"ForeignActiveTarget": "UPDATE probe_active_configs SET hub_id = '" + probeRegistryID3 + "'",
			} {
				t.Run(name, func(t *testing.T) { testRemoteAppliedMismatch(t, newProbeRegistryFixture(t, engine), mutate) })
			}
			t.Run("AuthenticationAndValidation", func(t *testing.T) { testRemoteAppliedValidation(t, newProbeRegistryFixture(t, engine)) })
		})
	}
}

func remoteAppliedReceipt(t *testing.T, f probeRegistryFixture, store *repository.RemoteProbeConfigSyncStore, meta domain.ProbeConfigMetadata) (domain.ProbeConnectorLease, domain.ProbeActiveConfig) {
	t.Helper()
	lease, err := repository.NewProbeConnectorStore(f.db).AcquireConnector(t.Context(), probeRegistryID1, probeRegistryID3)
	if err != nil {
		t.Fatal(err)
	}
	receipt := domain.ProbeActiveConfig{ProbeConfigTarget: syncTarget(), Revision: meta.Revision, SHA256: meta.SHA256, AssignmentCount: 1, AppliedAt: time.Now().UTC()}
	if err := store.RecordRemoteApplied(t.Context(), lease, receipt); err != nil {
		t.Fatal(err)
	}
	return lease, receipt
}

func assertRemoteAppliedMode(t *testing.T, config *domain.EdgeResolvedConfig, meta domain.ProbeConfigMetadata, monitor int64, mode domain.AlertDelivery) {
	t.Helper()
	if config == nil || !domain.SameProbeConfigMetadata(config.Metadata, meta) || len(config.Assignments) != 1 {
		t.Fatal("read did not select the exact receipt-backed document")
	}
	a := config.Assignments[0]
	if a.Monitor == nil || a.Monitor.ID != monitor || a.Generation != 1 || a.DesiredAlertDelivery != mode {
		t.Fatal("receipt-backed assignment identity or desired mode changed")
	}
}

func testRemoteAppliedSelection(t *testing.T, f probeRegistryFixture) {
	store, protector, monitor, channel := remoteSyncFixture(t, f)
	ctx := t.Context()
	at := time.Now().UTC()
	first, err := store.RefreshRemote(ctx, syncTarget(), at)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadAppliedRemote(ctx, syncTarget()); got != nil || !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("prepared snapshot was treated as receipt-backed")
	}
	lease, receipt := remoteAppliedReceipt(t, f, store, first)
	// Publication and a desired edit do not change the acknowledged document.
	assignments := repository.NewProbeAssignmentStore(f.db)
	set, err := assignments.ReplaceWithBindings(ctx, monitor, 2, []string{probeRegistryID1}, domain.HealthPolicyAnyDown, domain.AlertDeliveryAggregate, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RefreshRemote(ctx, syncTarget(), at.Add(time.Second))
	if err != nil || second.Revision != first.Revision+1 {
		t.Fatal("mode edit failed to publish a distinct snapshot", err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE notifications SET name = ? WHERE id = ?", "unpublished desired label", channel); err != nil {
		t.Fatal(err)
	}
	reopened := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f), probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)
	config, err := reopened.ReadAppliedRemote(ctx, syncTarget())
	if err != nil {
		t.Fatal(err)
	}
	assertRemoteAppliedMode(t, config, first, monitor, domain.AlertDeliveryRegional)
	if config.Channels[channel].Notification.Name != "Test Notif" {
		t.Fatal("mutable desired settings replaced the receipt-backed graph")
	}
	// The exact newer receipt selects aggregate as DESIRED only.
	receipt.Revision, receipt.SHA256 = second.Revision, second.SHA256
	if err := reopened.RecordRemoteApplied(ctx, lease, receipt); err != nil {
		t.Fatal(err)
	}
	config, err = reopened.ReadAppliedRemote(ctx, syncTarget())
	if err != nil {
		t.Fatal(err)
	}
	assertRemoteAppliedMode(t, config, second, monitor, domain.AlertDeliveryAggregate)
	// Publish both and verify one more exact transition.
	if _, err := assignments.ReplaceWithBindings(ctx, monitor, set.Revision, []string{probeRegistryID1}, domain.HealthPolicyAnyDown, domain.AlertDeliveryBoth, nil); err != nil {
		t.Fatal(err)
	}
	third, err := store.RefreshRemote(ctx, syncTarget(), at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	receipt.Revision, receipt.SHA256 = third.Revision, third.SHA256
	if err := store.RecordRemoteApplied(ctx, lease, receipt); err != nil {
		t.Fatal(err)
	}
	config, err = reopened.ReadAppliedRemote(ctx, syncTarget())
	if err != nil {
		t.Fatal(err)
	}
	assertRemoteAppliedMode(t, config, third, monitor, domain.AlertDeliveryBoth)
}

func testRemoteAppliedMismatch(t *testing.T, f probeRegistryFixture, mutation string) {
	store, _, _, _ := remoteSyncFixture(t, f)
	meta, err := store.RefreshRemote(t.Context(), syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	remoteAppliedReceipt(t, f, store, meta)
	if _, err := f.db.ExecContext(t.Context(), mutation); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadAppliedRemote(t.Context(), syncTarget()); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("inconsistent receipt/pointer/snapshot returned a configuration", err)
	}
}

type failingAppliedDecoder struct{}

func (failingAppliedDecoder) DecodeEdge(context.Context, []byte, domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	return nil, errors.New("private-sync-fixture: validator details")
}

type mutatedAppliedDecoder struct {
	decoder ports.EdgeConfigDecoder
	mutate  func(*domain.EdgeResolvedConfig) *domain.EdgeResolvedConfig
}

func (d mutatedAppliedDecoder) DecodeEdge(ctx context.Context, document []byte, target domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	config, err := d.decoder.DecodeEdge(ctx, document, target)
	if err != nil {
		return nil, err
	}
	return d.mutate(config), nil
}

func testRemoteAppliedValidation(t *testing.T, f probeRegistryFixture) {
	store, protector, _, _ := remoteSyncFixture(t, f)
	meta, err := store.RefreshRemote(t.Context(), syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	remoteAppliedReceipt(t, f, store, meta)
	wrongKey, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{38}, 32))
	if err != nil {
		t.Fatal(err)
	}
	wrong := repository.NewRemoteProbeConfigSyncStore(f.db, nil, probe.NewHubConfigDecoder(checker.Get, notifier.Get), wrongKey)
	if got, err := wrong.ReadAppliedRemote(t.Context(), syncTarget()); got != nil || err == nil {
		t.Fatal("foreign key authenticated the retained snapshot")
	}
	invalid := repository.NewRemoteProbeConfigSyncStore(f.db, nil, failingAppliedDecoder{}, protector)
	if got, err := invalid.ReadAppliedRemote(t.Context(), syncTarget()); got != nil || !errors.Is(err, domain.ErrInternal) || strings.Contains(err.Error(), "private-sync-fixture") {
		t.Fatal("semantic validation failed open or exposed confidential details")
	}
	for name, mutate := range map[string]func(*domain.EdgeResolvedConfig) *domain.EdgeResolvedConfig{
		"nil": func(*domain.EdgeResolvedConfig) *domain.EdgeResolvedConfig { return nil },
		"metadata": func(c *domain.EdgeResolvedConfig) *domain.EdgeResolvedConfig {
			c.Metadata.Revision++
			return c
		},
		"count": func(c *domain.EdgeResolvedConfig) *domain.EdgeResolvedConfig {
			c.Assignments = nil
			return c
		},
	} {
		decoder := mutatedAppliedDecoder{decoder: probe.NewHubConfigDecoder(checker.Get, notifier.Get), mutate: mutate}
		reader := repository.NewRemoteProbeConfigSyncStore(f.db, nil, decoder, protector)
		if got, err := reader.ReadAppliedRemote(t.Context(), syncTarget()); got != nil || !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("decoded %s was not bound to the exact receipt", name)
		}
	}
	foreign := syncTarget()
	foreign.HubID = probeRegistryID3
	if got, err := store.ReadAppliedRemote(t.Context(), foreign); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("foreign installation accepted the active pointer")
	}
	local := syncTarget()
	local.ProbeID = domain.LocalProbeID
	if got, err := store.ReadAppliedRemote(t.Context(), local); got != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatal("remote reader accepted the local dialect")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := store.ReadAppliedRemote(ctx, syncTarget()); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read returned a configuration")
	}
	if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ?", bytes.Repeat([]byte{1}, 80), probeRegistryID1); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ReadAppliedRemote(t.Context(), syncTarget()); got != nil || err == nil {
		t.Fatal("corrupt protected bytes authenticated")
	}
}

type appliedReadBarrier struct {
	once    sync.Once
	reached chan struct{}
	done    chan struct{}
}

func (*appliedReadBarrier) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *appliedReadBarrier) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	query := strings.NewReplacer("`", "", `"`, "").Replace(event.Query)
	if event.Err != nil || !strings.HasPrefix(query, "SELECT ") || !strings.Contains(query, "FROM probe_active_configs AS active") {
		return
	}
	h.once.Do(func() {
		close(h.reached)
		select {
		case <-h.done:
		case <-ctx.Done():
		}
	})
}

func testRemoteAppliedConcurrent(t *testing.T, f probeRegistryFixture) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store, protector, monitor, _ := remoteSyncFixture(t, f)
	first, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	lease, receipt := remoteAppliedReceipt(t, f, store, first)
	if _, err := repository.NewProbeAssignmentStore(f.db).ReplaceWithBindings(ctx, monitor, 2, []string{probeRegistryID1}, domain.HealthPolicyAnyDown, domain.AlertDeliveryAggregate, nil); err != nil {
		t.Fatal(err)
	}
	second, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.db.SetMaxOpenConns(1)
	if f.engine == "sqlite" {
		if _, err := f.db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := f.db.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			t.Fatal(err)
		}
	}
	peer := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f), probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)
	barrier := &appliedReadBarrier{reached: make(chan struct{}), done: make(chan struct{})}
	attachInPlaceQueryHook(f.db, barrier)
	writer := make(chan error, 1)
	go func() {
		defer close(barrier.done)
		select {
		case <-barrier.reached:
		case <-ctx.Done():
			writer <- ctx.Err()
			return
		}
		receipt.Revision, receipt.SHA256 = second.Revision, second.SHA256
		writer <- peer.RecordRemoteApplied(ctx, lease, receipt)
	}()
	config, readErr := store.ReadAppliedRemote(ctx, syncTarget())
	writeErr := <-writer
	if readErr != nil || writeErr != nil {
		t.Fatalf("concurrent receipt read/write failed: read %v write %v", readErr, writeErr)
	}
	assertRemoteAppliedMode(t, config, first, monitor, domain.AlertDeliveryRegional)
	config, err = store.ReadAppliedRemote(ctx, syncTarget())
	if err != nil {
		t.Fatal(err)
	}
	assertRemoteAppliedMode(t, config, second, monitor, domain.AlertDeliveryAggregate)
}
