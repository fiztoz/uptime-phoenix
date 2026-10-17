package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Schema checks are tested directly: service/repository validation cannot hide
// SQL UNKNOWN accepting an incomplete terminal outcome. No remote work occurs.
func TestM8InstallJournalSchema(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, state := range []string{domain.ProbeInstallFailed, domain.ProbeInstallCanceled, domain.ProbeInstallInterrupted} {
				t.Run("OperationNullCode_"+state, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					store := m8InstallStore(f)
					m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
					if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_operations SET state = 'installed' WHERE operation_id = ?", m8InstallOp1); err == nil {
						t.Fatal("nonnull invalid-state control did not reject: CHECK enforcement is absent")
					}
					if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_operations SET state = ?, error_code = NULL WHERE operation_id = ?", state, m8InstallOp1); err == nil {
						t.Fatal("SQL accepted terminal operation with NULL outcome code")
					}
				})
			}
			for _, state := range []string{domain.ProbeInstallResourceNotStarted, domain.ProbeInstallResourceUnknown} {
				t.Run("ResourceNullCode_"+state, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					store := m8InstallStore(f)
					m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
					m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
					m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
					if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_resources SET state = 'success' WHERE operation_id = ?", m8InstallOp1); err == nil {
						t.Fatal("nonnull invalid-resource-state control did not reject")
					}
					if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_resources SET state = ?, error_code = NULL, finished_at = started_at WHERE operation_id = ? AND kind = 'data_directory'", state, m8InstallOp1); err == nil {
						t.Fatal("SQL accepted completed resource with NULL outcome code")
					}
				})
			}
			t.Run("SubmicrosecondResourceCorruptionRefused", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				if engine == "mariadb" {
					var precision int
					if err := f.db.NewRaw("SELECT DATETIME_PRECISION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'probe_install_resources' AND COLUMN_NAME = 'started_at'").Scan(t.Context(), &precision); err != nil || precision != 6 {
						t.Fatalf("resource timestamp storage precision: %d, %v", precision, err)
					}
					return
				}
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				op := m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				started := op.Resources[0].StartedAt.Add(time.Nanosecond).Format("2006-01-02 15:04:05.999999999-07:00")
				if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_operations SET updated_at = ? WHERE operation_id = ?", op.UpdatedAt.Add(time.Microsecond), m8InstallOp1); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_resources SET started_at = ? WHERE operation_id = ? AND ordinal = 1", started, m8InstallOp1); err != nil {
					t.Fatal(err)
				}
				if rows := m8InstallRawResources(t, f, m8InstallOp1); rows[0].StartedAt.Nanosecond()%1000 != 1 {
					t.Fatal("fixture failed to persist submicrosecond resource corruption")
				}
				before := m8InstallSnapshot(t, f, m8InstallOp1)
				if _, err := store.ObserveInstall(t.Context(), m8InstallOp1); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("resource precision corruption was normalized instead of refused: %v", err)
				}
				if before != m8InstallSnapshot(t, f, m8InstallOp1) {
					t.Fatal("corrupt resource was repaired")
				}
			})
			t.Run("SubmicrosecondCorruptionRefused", func(t *testing.T) {
				if engine == "mariadb" {
					// DATETIME(6) cannot represent this corruption; prove the
					// actual storage precision rather than invent a MariaDB skip.
					f := newProbeRegistryFixture(t, engine)
					var precision int
					if err := f.db.NewRaw("SELECT DATETIME_PRECISION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'probe_install_operations' AND COLUMN_NAME = 'created_at'").Scan(t.Context(), &precision); err != nil || precision != 6 {
						t.Fatalf("journal timestamp storage precision: %d, %v", precision, err)
					}
					return
				}
				f := newProbeRegistryFixture(t, engine)
				store := m8InstallStore(f)
				operation := m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				// Keep the exact lifetime and monotonic ordering: only the
				// forbidden stored precision changes, isolating the guard.
				// Bun formats time.Time parameters to microseconds. Bind strings
				// to construct the higher-precision SQLite corruption for real.
				precise := func(stamp time.Time) string {
					return stamp.Add(time.Nanosecond).Format("2006-01-02 15:04:05.999999999-07:00")
				}
				if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_operations SET created_at = ?, updated_at = ?, deadline_at = ? WHERE operation_id = ?", precise(operation.CreatedAt), precise(operation.UpdatedAt), precise(operation.DeadlineAt), m8InstallOp1); err != nil {
					t.Fatal(err)
				}
				if raw := m8InstallRawOp(t, f, m8InstallOp1); raw.CreatedAt.Nanosecond()%1000 != 1 || raw.DeadlineAt.Nanosecond()%1000 != 1 {
					t.Fatal("fixture failed to persist submicrosecond identity corruption")
				}
				before := m8InstallSnapshot(t, f, m8InstallOp1)
				svc, err := services.NewProbeInstallJournalService(store, store)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := svc.Observe(t.Context(), m8InstallOp1); !errors.Is(err, domain.ErrInternal) {
					t.Fatalf("stored precision corruption was normalized instead of refused: %v", err)
				}
				if after := m8InstallSnapshot(t, f, m8InstallOp1); before != after {
					t.Fatal("observing corruption changed persisted evidence")
				}
			})
		})
	}
}
