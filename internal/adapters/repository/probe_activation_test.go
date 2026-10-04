package repository_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestProbeActivationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"FirstValidAndIdempotentRetry":  testProbeActivationFirstValidAndIdempotentRetry,
				"OlderRevisionAndHashConflict":  testProbeActivationOlderRevisionAndHashConflict,
				"WrongTargetAndDisabledProbe":   testProbeActivationWrongTargetAndDisabledProbe,
				"SourceFreshnessTwoConnections": testProbeActivationSourceFreshnessTwoConnections,
				"ConcurrentRaces":               testProbeActivationConcurrentRaces,
				"EmptyConfiguration":            testProbeActivationEmptyConfiguration,
				"DowngradeGuards":               testProbeActivationDowngradeGuards,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func activationRepo(f probeRegistryFixture, db *bun.DB) ports.ProbeConfigActivationRepository {
	if f.engine == "sqlite" {
		return sqlite.NewProbeActivationRepo(db, probe.LocalConfigEncoder{})
	}
	return mariadb.NewProbeActivationRepo(db, probe.LocalConfigEncoder{})
}

func seedInstallation(t *testing.T, f probeRegistryFixture) {
	const hubID = "11111111-2222-4333-8444-555555555555"
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	instRepo := installationRepo(f, f.db)
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, err = instRepo.Initialize(context.Background(), domain.ProbeInstallation{
		HubID:          hubID,
		KeyHash:        protector.KeyHash(hubID),
		ProtocolFloor:  1,
		AuthorityEpoch: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func testProbeActivationFirstValidAndIdempotentRetry(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	seedInstallation(t, f)

	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}
	monID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monID); err != nil {
		t.Fatal(err)
	}

	builder, _ := sourceConfigBuilder(t, f, f.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	meta, err := builder.Prepare(ctx, hubID, 0, now, now)
	if err != nil {
		t.Fatal(err)
	}

	repo := activationRepo(f, f.db)

	// 1. Initial state: not found
	if _, err := repo.GetActive(ctx, domain.LocalProbeID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for initial active, got %v", err)
	}
	if _, err := repo.GetReceipt(ctx, domain.LocalProbeID, 1); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for initial receipt, got %v", err)
	}

	// 2. First valid activation
	applied, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               meta.Revision,
		SHA256:                 meta.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	})
	if err != nil {
		t.Fatalf("unexpected error on ActivateLocal: %v", err)
	}
	if applied.Revision != 1 || applied.SHA256 != meta.SHA256 || applied.AssignmentCount != 1 {
		t.Fatalf("unexpected applied result: %+v", applied)
	}

	// 3. GetActive returns durable active state
	active, err := repo.GetActive(ctx, domain.LocalProbeID)
	if err != nil || !domain.SameProbeActiveConfig(*applied, *active) {
		t.Fatalf("GetActive mismatch: got %+v, want %+v", active, applied)
	}

	// 4. GetReceipt returns durable receipt
	receipt, err := repo.GetReceipt(ctx, domain.LocalProbeID, 1)
	if err != nil || !domain.SameProbeActiveConfig(*applied, *receipt) {
		t.Fatalf("GetReceipt mismatch: got %+v, want %+v", receipt, applied)
	}

	// 5. Same-revision same-hash retry returns identical receipt without modifying anything
	retried, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               meta.Revision,
		SHA256:                 meta.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now.Add(time.Minute),
		AssignmentCount:        -1,
	})
	if err != nil {
		t.Fatalf("unexpected error on retry: %v", err)
	}
	if !domain.SameProbeActiveConfig(*applied, *retried) {
		t.Fatalf("retried result changed: got %+v, want %+v", retried, applied)
	}
}

func testProbeActivationOlderRevisionAndHashConflict(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	seedInstallation(t, f)

	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}
	monID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monID); err != nil {
		t.Fatal(err)
	}

	builder, _ := sourceConfigBuilder(t, f, f.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	meta1, err := builder.Prepare(ctx, hubID, 0, now, now)
	if err != nil {
		t.Fatal(err)
	}

	repo := activationRepo(f, f.db)
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               1,
		SHA256:                 meta1.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Same revision with different hash -> ErrConflict
	badHash := strings.Repeat("e", 64)
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               1,
		SHA256:                 badHash,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for same revision with different hash, got %v", err)
	}

	// 2. Prepare and activate revision 2
	now2 := now.Add(time.Minute)
	meta2, err := builder.Prepare(ctx, hubID, 1, now2, now2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               2,
		SHA256:                 meta2.SHA256,
		ExpectedActiveRevision: 1,
		AppliedAt:              now2,
		AssignmentCount:        -1,
	}); err != nil {
		t.Fatal(err)
	}

	// 3. Older revision (revision 1) attempted while revision 2 is active -> ErrConflict
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               1,
		SHA256:                 meta1.SHA256,
		ExpectedActiveRevision: 1,
		AppliedAt:              now2,
		AssignmentCount:        -1,
	}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for older revision, got %v", err)
	}

	// 4. Expected active revision mismatch (e.g. expected 1, but active is 2)
	now3 := now2.Add(time.Minute)
	meta3, err := builder.Prepare(ctx, hubID, 2, now3, now3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               3,
		SHA256:                 meta3.SHA256,
		ExpectedActiveRevision: 1, // Mismatch: active is 2
		AppliedAt:              now3,
		AssignmentCount:        -1,
	}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for expected active revision mismatch, got %v", err)
	}

	// Active selection must remain revision 2
	active, err := repo.GetActive(ctx, domain.LocalProbeID)
	if err != nil || active.Revision != 2 {
		t.Fatalf("active selection changed: %+v", active)
	}
}

func testProbeActivationWrongTargetAndDisabledProbe(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	seedInstallation(t, f)

	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}
	monID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monID); err != nil {
		t.Fatal(err)
	}

	builder, _ := sourceConfigBuilder(t, f, f.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	meta, err := builder.Prepare(ctx, hubID, 0, now, now)
	if err != nil {
		t.Fatal(err)
	}

	repo := activationRepo(f, f.db)

	// 1. Wrong hub ID -> ErrConflict
	wrongHubTarget := domain.ProbeConfigTarget{HubID: "99999999-9999-4999-8999-999999999999", ProbeID: domain.LocalProbeID}
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 wrongHubTarget,
		Revision:               1,
		SHA256:                 meta.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for wrong hub ID, got %v", err)
	}

	// 2. Disabled local probe -> ErrValidation
	if _, err := f.db.NewUpdate().Table("probes").Set("enabled = ?", false).Where("id = ?", domain.LocalProbeID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               1,
		SHA256:                 meta.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation for disabled probe, got %v", err)
	}
	// Re-enable
	if _, err := f.db.NewUpdate().Table("probes").Set("enabled = ?", true).Where("id = ?", domain.LocalProbeID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
}

func testProbeActivationSourceFreshnessTwoConnections(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	seedInstallation(t, f)

	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}
	monID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monID); err != nil {
		t.Fatal(err)
	}

	// Open second connection
	var db2 *bun.DB
	var err error
	if f.engine == "sqlite" {
		db2, err = sqlite.NewDB(f.dsn)
	} else {
		db2, err = mariadb.NewDB(f.dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })

	builder, _ := sourceConfigBuilder(t, f, f.db)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Case A: Monitor edit on connection 2 between preparation and activation
	meta, err := builder.Prepare(ctx, hubID, 0, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// Connection 2 commits an edit to monitor interval
	if _, err := db2.NewUpdate().Table("monitors").Set("check_interval = ?", 120).Where("id = ?", monID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	repo1 := activationRepo(f, f.db)
	// Activation on connection 1 must detect stale candidate and reject with ErrConflict!
	if _, err := repo1.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               1,
		SHA256:                 meta.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for stale candidate after monitor edit, got %v", err)
	}

	// Case B: Rebuild with new interval, prepare revision 2, then add a tag on connection 2
	meta2, err := builder.Prepare(ctx, hubID, 1, now, now)
	if err != nil {
		t.Fatal(err)
	}
	tag := &repository.TagModel{Name: "env"}
	insertConfigModel(t, f.db, tag)
	insertConfigModel(t, db2, &repository.MonitorTagModel{MonitorID: monID, TagID: tag.ID, Value: "prod"})

	// Activation must fail due to tag edit
	if _, err := repo1.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               meta2.Revision,
		SHA256:                 meta2.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("expected ErrConflict for stale candidate after tag link edit, got %v", err)
	}

	// Case C: Rollback on connection 2 leaves source untouched -> activation succeeds
	meta3, err := builder.Prepare(ctx, hubID, 2, now, now)
	if err != nil {
		t.Fatal(err)
	}

	// Connection 2 starts a transaction, modifies monitor, and rolls back
	_ = db2.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Table("monitors").Set("name = ?", "temporary").Where("id = ?", monID).Exec(ctx); err != nil {
			return err
		}
		return errors.New("force rollback")
	})

	// Activation on connection 1 must succeed because connection 2 rolled back!
	applied, err := repo1.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               meta3.Revision,
		SHA256:                 meta3.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	})
	if err != nil {
		t.Fatalf("activation failed after rollback: %v", err)
	}
	if applied.Revision != 3 {
		t.Fatalf("unexpected revision: %d", applied.Revision)
	}
}

func testProbeActivationConcurrentRaces(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	seedInstallation(t, f)

	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}
	monID := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, monID); err != nil {
		t.Fatal(err)
	}

	builder, _ := sourceConfigBuilder(t, f, f.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	meta, err := builder.Prepare(ctx, hubID, 0, now, now)
	if err != nil {
		t.Fatal(err)
	}

	repo := activationRepo(f, f.db)

	// Race two activations of the same candidate
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = repo.ActivateLocal(ctx, ports.LocalActivationParams{
				Target:                 target,
				Revision:               1,
				SHA256:                 meta.SHA256,
				ExpectedActiveRevision: 0,
				AppliedAt:              now,
				AssignmentCount:        -1,
			})
		}(i)
	}
	wg.Wait()

	// All identical retries must succeed (idempotent)
	for idx, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v", idx, err)
		}
	}

	// Verify only 1 active config and 1 receipt
	var count int
	if err := f.db.NewSelect().Table("probe_active_configs").ColumnExpr("COUNT(*)").Scan(ctx, &count); err != nil || count != 1 {
		t.Fatalf("expected 1 active config, got %d, %v", count, err)
	}
	if err := f.db.NewSelect().Table("probe_config_applied_receipts").ColumnExpr("COUNT(*)").Scan(ctx, &count); err != nil || count != 1 {
		t.Fatalf("expected 1 receipt, got %d, %v", count, err)
	}
}

func testProbeActivationEmptyConfiguration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	seedInstallation(t, f)

	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}

	// No monitors assigned to local -> empty configuration
	builder, _ := sourceConfigBuilder(t, f, f.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	meta, err := builder.Prepare(ctx, hubID, 0, now, now)
	if err != nil {
		t.Fatal(err)
	}

	repo := activationRepo(f, f.db)
	applied, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               1,
		SHA256:                 meta.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	})
	if err != nil {
		t.Fatalf("unexpected error activating empty configuration: %v", err)
	}
	if applied.AssignmentCount != 0 {
		t.Fatalf("expected 0 assignments, got %d", applied.AssignmentCount)
	}

	active, err := repo.GetActive(ctx, domain.LocalProbeID)
	if err != nil || active.AssignmentCount != 0 {
		t.Fatalf("expected 0 assignments in active config, got %+v", active)
	}
}

func testProbeActivationDowngradeGuards(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	seedInstallation(t, f)

	target := domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID}
	builder, _ := sourceConfigBuilder(t, f, f.db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	meta, err := builder.Prepare(ctx, hubID, 0, now, now)
	if err != nil {
		t.Fatal(err)
	}

	repo := activationRepo(f, f.db)
	if _, err := repo.ActivateLocal(ctx, ports.LocalActivationParams{
		Target:                 target,
		Revision:               1,
		SHA256:                 meta.SHA256,
		ExpectedActiveRevision: 0,
		AppliedAt:              now,
		AssignmentCount:        -1,
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Downgrade while populated must fail due to downgrade guard!
	if err := runProbeActivationMigration(t, f.db, f.engine, "down"); err == nil {
		t.Fatal("expected downgrade guard to refuse while populated, but down migration succeeded")
	}

	// 2. Clear rows
	if _, err := f.db.NewDelete().Table("probe_active_configs").Where("1 = 1").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.NewDelete().Table("probe_config_applied_receipts").Where("1 = 1").Exec(ctx); err != nil {
		t.Fatal(err)
	}

	// 3. Downgrade while empty must succeed!
	if err := runProbeActivationMigration(t, f.db, f.engine, "down"); err != nil {
		t.Fatalf("unexpected error downgrading empty tables: %v", err)
	}

	// 4. Upgrade again
	if err := runProbeActivationMigration(t, f.db, f.engine, "up"); err != nil {
		t.Fatalf("unexpected error reapplying up migration: %v", err)
	}
}

func runProbeActivationMigration(t *testing.T, db *bun.DB, engine, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(engine, "migrations", "049_probe_activation."+direction+".sql"))
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			return err
		}
	}
	return nil
}
