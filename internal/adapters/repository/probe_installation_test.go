package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestProbeInstallationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"InitializationAndIdempotency": testProbeInstallationInitAndIdempotent,
				"ConcurrentInitializers":       testProbeInstallationConcurrency,
				"RetainedSnapshots":            testProbeInstallationRetainedSnapshots,
				"DowngradeGuards":              testProbeInstallationDowngradeGuards,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func installationRepo(f probeRegistryFixture, db *bun.DB) ports.ProbeInstallationRepository {
	if f.engine == "sqlite" {
		return sqlite.NewProbeInstallationRepo(db)
	}
	return mariadb.NewProbeInstallationRepo(db)
}

func testProbeInstallationInitAndIdempotent(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	repo := installationRepo(f, f.db)

	// 1. Initial state: not found
	_, err := repo.Get(ctx)
	if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on empty table, got %v", err)
	}

	// 2. Initialize valid installation
	now := time.Now().UTC().Truncate(time.Microsecond)
	inst := domain.ProbeInstallation{
		HubID:          "11111111-2222-4333-8444-555555555555",
		KeyHash:        "1111111122223333444455556666777788889999aaaabbbbccccddddeeeeffff",
		ProtocolFloor:  1,
		AuthorityEpoch: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	created, err := repo.Initialize(ctx, inst, nil)
	if err != nil {
		t.Fatalf("unexpected error on Initialize: %v", err)
	}
	if !domain.SameProbeInstallation(inst, *created) {
		t.Fatalf("created record mismatch: got %+v, want %+v", created, inst)
	}

	// 3. Get returns the stored record
	got, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("unexpected error on Get: %v", err)
	}
	if !domain.SameProbeInstallation(inst, *got) {
		t.Fatalf("get record mismatch: got %+v, want %+v", got, inst)
	}

	// 4. Idempotent retry with identical record succeeds
	retry, err := repo.Initialize(ctx, inst, nil)
	if err != nil {
		t.Fatalf("unexpected error on idempotent Initialize: %v", err)
	}
	if !domain.SameProbeInstallation(inst, *retry) {
		t.Fatalf("idempotent retry mismatch: got %+v, want %+v", retry, inst)
	}

	// 5. Conflicting record fails with ErrConflict
	conflictInst := inst
	conflictInst.KeyHash = "9999999922223333444455556666777788889999aaaabbbbccccddddeeeeffff"
	_, err = repo.Initialize(ctx, conflictInst, nil)
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict on conflicting Initialize, got %v", err)
	}
}

func testProbeInstallationConcurrency(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()

	db2 := reopenConfigDB(t, f)
	repo1 := installationRepo(f, f.db)
	repo2 := installationRepo(f, db2)

	// Test A: Racing with DIFFERENT records -> exactly one succeeds, one gets conflict
	now := time.Now().UTC().Truncate(time.Microsecond)
	instA := domain.ProbeInstallation{
		HubID:          "11111111-2222-4333-8444-555555555555",
		KeyHash:        "1111111122223333444455556666777788889999aaaabbbbccccddddeeeeffff",
		ProtocolFloor:  1,
		AuthorityEpoch: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	instB := domain.ProbeInstallation{
		HubID:          "22222222-3333-4444-8555-666666666666",
		KeyHash:        "2222222222223333444455556666777788889999aaaabbbbccccddddeeeeffff",
		ProtocolFloor:  1,
		AuthorityEpoch: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error

	go func() {
		defer wg.Done()
		_, errA = repo1.Initialize(ctx, instA, nil)
	}()
	go func() {
		defer wg.Done()
		_, errB = repo2.Initialize(ctx, instB, nil)
	}()
	wg.Wait()

	if !((errA == nil && errors.Is(errB, ports.ErrConflict)) || (errB == nil && errors.Is(errA, ports.ErrConflict))) {
		t.Fatalf("expected exactly one winner and one ErrConflict, got errA=%v, errB=%v", errA, errB)
	}
}

func testProbeInstallationRetainedSnapshots(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	repo := installationRepo(f, f.db)

	has, err := repo.HasSnapshots(ctx)
	if err != nil || has {
		t.Fatalf("expected HasSnapshots=false on empty table, got %v, err=%v", has, err)
	}

	// Insert a snapshot
	configR := configRepo(f, f.db)
	s := protectedConfigService(t, configR)
	doc, target := configDocument(t, "local", 1, "secret-test")
	hubID := target.HubID
	if _, err := s.Prepare(ctx, target, doc, 0); err != nil {
		t.Fatalf("prepare snapshot failed: %v", err)
	}

	has, err = repo.HasSnapshots(ctx)
	if err != nil || !has {
		t.Fatalf("expected HasSnapshots=true after insert, got %v, err=%v", has, err)
	}

	// Verify matching hub_id
	verifiedCount := 0
	err = repo.VerifyRetainedSnapshots(ctx, hubID, func(metadata domain.ProbeConfigMetadata, payload []byte) error {
		verifiedCount++
		if metadata.HubID != hubID {
			t.Errorf("callback received wrong HubID: %s", metadata.HubID)
		}
		if len(payload) == 0 {
			t.Errorf("callback received empty payload")
		}
		return nil
	})
	if err != nil || verifiedCount != 1 {
		t.Fatalf("VerifyRetainedSnapshots failed: count=%d, err=%v", verifiedCount, err)
	}

	// Verify mismatched hub_id fails
	err = repo.VerifyRetainedSnapshots(ctx, "22222222-3333-4444-8555-666666666666", func(metadata domain.ProbeConfigMetadata, payload []byte) error {
		return nil
	})
	if !errors.Is(err, domain.ErrProbeInstallationConflict) {
		t.Fatalf("expected ErrProbeInstallationConflict on mismatched hubID, got %v", err)
	}

	// Verify callback error propagation
	expectedCallbackErr := errors.New("callback failed")
	err = repo.VerifyRetainedSnapshots(ctx, hubID, func(metadata domain.ProbeConfigMetadata, payload []byte) error {
		return expectedCallbackErr
	})
	if !errors.Is(err, expectedCallbackErr) {
		t.Fatalf("expected callback error to propagate, got %v", err)
	}
}

func testProbeInstallationDowngradeGuards(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	repo := installationRepo(f, f.db)

	now := time.Now().UTC().Truncate(time.Microsecond)
	inst := domain.ProbeInstallation{
		HubID:          "11111111-2222-4333-8444-555555555555",
		KeyHash:        "1111111122223333444455556666777788889999aaaabbbbccccddddeeeeffff",
		ProtocolFloor:  1,
		AuthorityEpoch: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if _, err := repo.Initialize(ctx, inst, nil); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	// Attempting downgrade with populated probe_installation must fail guard
	guardSQL := `CREATE TABLE IF NOT EXISTS probe_installation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_installation_downgrade_guard (ok) SELECT 0 FROM probe_installation LIMIT 1;
DROP TABLE probe_installation_downgrade_guard;`

	_, err := f.db.ExecContext(ctx, guardSQL)
	if err == nil {
		t.Fatalf("expected downgrade guard to fail when probe_installation has rows")
	}

	// Delete row, now guard must succeed
	if _, err := f.db.NewDelete().Table("probe_installation").Where("id = 1").Exec(ctx); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if _, err := f.db.ExecContext(ctx, guardSQL); err != nil {
		t.Fatalf("expected downgrade guard to pass when probe_installation is empty: %v", err)
	}
}
