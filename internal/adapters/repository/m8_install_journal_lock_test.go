package repository_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type m8JournalLockEntry struct {
	entered chan struct{}
	once    sync.Once
}

func (h *m8JournalLockEntry) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if strings.Contains(event.Query, "probe_install_operations SET operation_id = operation_id") {
		h.once.Do(func() { close(h.entered) })
	}
	return ctx
}
func (*m8JournalLockEntry) AfterQuery(context.Context, *bun.QueryEvent) {}

// A committed deadline expires on the DB clock while unchanged authority is
// held by another transaction. InnoDB 1020/retry cannot mask a pre-lock clock
// sample, and neither host/VM clock agreement nor scheduling sleeps prove expiry.
func TestM8InstallJournalClockAfterAuthorityLock(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, action := range []string{"Begin", "Reserve", "Record", "Finish", "Observe", "Cancel"} {
				t.Run(action, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
					defer cancel()
					store := m8InstallStore(f)
					m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
					if action != "Begin" {
						m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
					}
					if action == "Record" {
						m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
					}
					query := "SELECT strftime('%Y-%m-%d %H:%M:%f', 'now')"
					if engine == "mariadb" {
						query = "SELECT DATE_FORMAT(UTC_TIMESTAMP(6), '%Y-%m-%d %H:%i:%s.%f')"
					}
					readClock := func(tx bun.Tx) time.Time {
						var stamp string
						if err := tx.NewRaw(query).Scan(ctx, &stamp); err != nil {
							t.Fatal(err)
						}
						now, err := time.ParseInLocation("2006-01-02 15:04:05.999999", stamp, time.UTC)
						if err != nil {
							t.Fatal(err)
						}
						return now
					}
					seed, err := f.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = seed.Rollback() }()
					deadline := readClock(seed).Add(1200 * time.Millisecond)
					if _, err := seed.ExecContext(ctx, "UPDATE probe_install_operations SET created_at = ?, deadline_at = ? WHERE operation_id = ?", deadline.Add(-domain.ProbeInstallLifetime), deadline, m8InstallOp1); err != nil {
						t.Fatal(err)
					}
					if err := seed.Commit(); err != nil {
						t.Fatal(err)
					}
					blocker, err := f.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = blocker.Rollback() }()
					if engine == "mariadb" {
						var id string
						if err := blocker.NewRaw("SELECT operation_id FROM probe_install_operations WHERE operation_id = ? FOR UPDATE", m8InstallOp1).Scan(ctx, &id); err != nil {
							t.Fatal(err)
						}
					} else if _, err := blocker.ExecContext(ctx, "UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", m8InstallOp1); err != nil {
						t.Fatal(err)
					}
					peer := f
					peer.db = m8InstallSecondDB(t, f, "")
					hook := &m8JournalLockEntry{entered: make(chan struct{})}
					attachInPlaceQueryHook(peer.db, hook)
					svc := m8JournalService(t, peer)
					type reply struct {
						operation *domain.ProbeInstallOperation
						fresh     bool
						err       error
					}
					results := make(chan reply, 1)
					joined := make(chan struct{})
					go func() {
						defer close(joined)
						var r reply
						switch action {
						case "Begin":
							r.operation, r.err = svc.Begin(ctx, m8InstallOp1, m8InstallOwnerA)
						case "Reserve":
							r.operation, r.fresh, r.err = svc.ReserveResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
						case "Record":
							r.operation, r.err = svc.RecordResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallApplied())
						case "Finish":
							r.operation, r.err = svc.Finish(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "execution_failed"})
						case "Observe":
							r.operation, r.err = svc.Observe(ctx, m8InstallOp1)
						case "Cancel":
							r.operation, r.err = svc.Cancel(ctx, m8InstallOp1)
						}
						results <- r
					}()
					defer func() { cancel(); _ = blocker.Rollback(); <-joined }()
					select {
					case <-hook.entered:
					case r := <-results:
						t.Fatalf("operation returned before held authority lock: %v", r.err)
					case <-ctx.Done():
						t.Fatal("operation never reached authority lock")
					}
					if !readClock(blocker).Before(deadline) {
						t.Fatal("fixture did not establish a live pre-lock deadline")
					}
					for readClock(blocker).Before(deadline) {
						select {
						case r := <-results:
							t.Fatalf("operation bypassed held authority: %v", r.err)
						case <-time.After(20 * time.Millisecond):
						}
					}
					if err := blocker.Commit(); err != nil {
						t.Fatal(err)
					}
					r := <-results
					if action == "Observe" || action == "Cancel" {
						if r.err != nil || r.operation == nil || r.operation.State != domain.ProbeInstallInterrupted || r.operation.ErrorCode != "deadline_expired" {
							t.Fatalf("expired authority was not reconciled after lock: %v", r.err)
						}
					} else if !errors.Is(r.err, ports.ErrConflict) || r.fresh || r.operation != nil {
						t.Fatalf("expired authority accepted after lock wait: action=%s fresh=%v err=%v", action, r.fresh, r.err)
					}
					// Read raw rows BEFORE Observe can reconcile again: a rolled-
					// back expiry must not be hidden by a subsequent healing read.
					raw := m8InstallRawOp(t, f, m8InstallOp1)
					if raw.State != domain.ProbeInstallInterrupted || raw.ErrorCode == nil || *raw.ErrorCode != "deadline_expired" {
						t.Fatal("expiry reconciliation did not commit before observation")
					}
					if action == "Record" {
						rows := m8InstallRawResources(t, f, m8InstallOp1)
						if rows[0].State != domain.ProbeInstallResourceUnknown || rows[0].ErrorCode == nil || *rows[0].ErrorCode != "deadline_expired" {
							t.Fatal("uncertain resource reconciliation did not commit before observation")
						}
					}
					op, err := store.ObserveInstall(ctx, m8InstallOp1)
					if err != nil || op.State != domain.ProbeInstallInterrupted || op.ErrorCode != "deadline_expired" {
						t.Fatalf("expiry reconciliation did not commit: %v", err)
					}
					if action == "Record" && op.Resources[0].State != domain.ProbeInstallResourceUnknown {
						t.Fatal("late applied report replaced uncertain resource")
					}
				})
			}
		})
	}
}
