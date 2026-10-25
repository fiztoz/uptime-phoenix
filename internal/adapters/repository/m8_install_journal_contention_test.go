package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestM8InstallJournalSQLiteContention(t *testing.T) {
	f := newProbeRegistryFixture(t, "sqlite")
	m8InstallCreate(t, m8InstallStore(f), m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
	peer := f
	var err error
	peer.db, err = sqlite.NewDB(f.dsn + "&_pragma=busy_timeout(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.db.Close() })
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(t.Context(), "UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", m8InstallOp1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	operation, fresh, err := m8JournalService(t, peer).Create(ctx, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
	if !errors.Is(err, domain.ErrInternal) || fresh || operation != nil {
		t.Fatalf("contention invented permanent identity conflict or grant: fresh=%v err=%v", fresh, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Releasing contention permits the same intent, proving there never was
	// a stored identity mismatch and that no partial graph escaped rollback.
	if op, fresh, err := m8JournalService(t, peer).Create(t.Context(), m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA); err != nil || !fresh || op == nil {
		t.Fatalf("valid intent was permanently lost: fresh=%v err=%v", fresh, err)
	}
}
