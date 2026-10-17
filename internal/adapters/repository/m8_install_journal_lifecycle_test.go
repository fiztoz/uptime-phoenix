package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func m8JournalService(t *testing.T, f probeRegistryFixture) *services.ProbeInstallJournalService {
	t.Helper()
	store := m8InstallStore(f)
	svc, err := services.NewProbeInstallJournalService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func m8JournalAdmit(t *testing.T, svc *services.ProbeInstallJournalService, mode string) *domain.ProbeInstallOperation {
	t.Helper()
	op, created, err := svc.Create(t.Context(), m8InstallIdentity(mode, m8InstallOp1), m8InstallOwnerA)
	if err != nil || !created || op == nil || op.State != domain.ProbeInstallQueued {
		t.Fatalf("admit journal intent: created=%v err=%v", created, err)
	}
	if _, err := svc.Begin(t.Context(), m8InstallOp1, m8InstallOwnerA); err != nil {
		t.Fatal(err)
	}
	return op
}

// The real DB commit succeeds, then its acknowledgement's context is lost.
// This adapter injects transport timing only; both journal methods still use
// the production store and no resource mutation is actually executed.
type m8JournalLostAck struct {
	ports.ProbeInstallResourceJournal
	cancel context.CancelFunc
}

func (j m8JournalLostAck) ReserveInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
	op, created, err := j.ProbeInstallResourceJournal.ReserveInstallResource(ctx, operationID, ownerID, kind, attemptID)
	if err == nil && created {
		j.cancel()
	}
	return op, created, err
}

func TestM8InstallJournalLifecycle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ResourcesReadyIsNotInstallation", func(t *testing.T) {
				for _, mode := range []string{domain.ProbeInstallSystemd, domain.ProbeInstallDocker} {
					t.Run(mode, func(t *testing.T) {
						f := newProbeRegistryFixture(t, engine)
						svc := m8JournalService(t, f)
						original := m8JournalAdmit(t, svc, mode)
						peer := f
						peer.db = m8InstallSecondDB(t, f, "")
						replica := m8JournalService(t, peer)
						if _, err := replica.Begin(t.Context(), m8InstallOp1, m8InstallOwnerA); !errors.Is(err, ports.ErrConflict) {
							t.Fatalf("second begin dispatched: %v", err)
						}
						for i, spec := range original.Identity.Resources() {
							attempt := []string{m8InstallAttempt1, m8InstallAttempt2, m8InstallAttempt3, m8InstallAttempt4}[i]
							op, fresh, err := svc.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, spec.Kind, attempt)
							if err != nil || !fresh || op == nil {
								t.Fatalf("reserve %s: fresh=%v err=%v", spec.Kind, fresh, err)
							}
							visible, fresh, err := replica.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, spec.Kind, attempt)
							if err != nil || fresh || visible == nil || visible.Resources[i].State != domain.ProbeInstallResourceInFlight {
								t.Fatalf("replica replayed intent %s: fresh=%v err=%v", spec.Kind, fresh, err)
							}
							if _, err := replica.RecordResource(t.Context(), m8InstallOp1, m8InstallOwnerA, spec.Kind, attempt, m8InstallApplied()); err != nil {
								t.Fatal(err)
							}
						}
						ready, err := replica.Observe(t.Context(), m8InstallOp1)
						if err != nil || ready == nil || ready.State != domain.ProbeInstallResourcesReady || ready.Terminal() || !ready.DeadlineAt.Equal(original.DeadlineAt) {
							t.Fatalf("resources fabricated installation success or renewed authority: %v", err)
						}
						if mode == domain.ProbeInstallDocker && ready.Resources[0].Spec.Owned {
							t.Fatal("shared image acquired cleanup ownership")
						}
						if _, err := svc.Finish(t.Context(), m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: "installed"}); !errors.Is(err, domain.ErrValidation) {
							t.Fatalf("installation success admitted without proof: %v", err)
						}
						ready.Resources[0].Spec.Reference = "/caller-substitution"
						again, err := svc.Observe(t.Context(), m8InstallOp1)
						if err != nil || again.Resources[0].Spec.Reference == "/caller-substitution" {
							t.Fatal("caller mutation changed durable snapshot")
						}
					})
				}
			})
			t.Run("CancellationRequiresIntent", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				m8JournalAdmit(t, svc, domain.ProbeInstallSystemd)
				before := m8InstallSnapshot(t, f, m8InstallOp1)
				outcome := domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"}
				if _, err := svc.Finish(t.Context(), m8InstallOp1, m8InstallOwnerA, outcome); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("terminal canceled without cancellation intent: %v", err)
				}
				if before != m8InstallSnapshot(t, f, m8InstallOp1) {
					t.Fatal("refused cancellation changed history")
				}
				if op, err := svc.Cancel(t.Context(), m8InstallOp1); err != nil || op.State != domain.ProbeInstallCancelRequested {
					t.Fatalf("cancel intent: %v", err)
				}
				if op, err := svc.Finish(t.Context(), m8InstallOp1, m8InstallOwnerA, outcome); err != nil || op.State != domain.ProbeInstallCanceled {
					t.Fatalf("joined cancellation: %v", err)
				}
			})
			t.Run("LostAcknowledgementNeverReplays", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				m8JournalAdmit(t, svc, domain.ProbeInstallSystemd)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				store := m8InstallStore(f)
				faulty, err := services.NewProbeInstallJournalService(store, m8JournalLostAck{ProbeInstallResourceJournal: store, cancel: cancel})
				if err != nil {
					t.Fatal(err)
				}
				if op, fresh, err := faulty.ReserveResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1); !errors.Is(err, context.Canceled) || fresh || op != nil {
					t.Fatalf("lost acknowledgement returned fresh authority: fresh=%v err=%v", fresh, err)
				}
				peer := f
				peer.db = m8InstallSecondDB(t, f, "")
				replica := m8JournalService(t, peer)
				if op, fresh, err := replica.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1); err != nil || fresh || op.Resources[0].State != domain.ProbeInstallResourceInFlight {
					t.Fatalf("ambiguous commit replayed: fresh=%v err=%v", fresh, err)
				}
				if _, fresh, err := replica.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2); !errors.Is(err, ports.ErrConflict) || fresh {
					t.Fatalf("uncommitted prerequisite authorized: fresh=%v err=%v", fresh, err)
				}
				if op, err := svc.Finish(t.Context(), m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallInterrupted, ErrorCode: "owner_interrupted"}); err != nil || op.Resources[0].State != domain.ProbeInstallResourceUnknown {
					t.Fatalf("owner loss hid uncertainty: %v", err)
				}
			})
			t.Run("ColdReplicaAndLateReport", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				m8JournalAdmit(t, svc, domain.ProbeInstallSystemd)
				if _, _, err := svc.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1); err != nil {
					t.Fatal(err)
				}
				// Exercise foreign reports while the exact action is in-flight:
				// planned/completed rows would reject for an unrelated reason.
				for _, claim := range []struct{ owner, attempt, label string }{
					{m8InstallOwnerB, m8InstallAttempt1, "foreign in-flight owner"},
					{m8InstallOwnerA, m8InstallAttempt2, "foreign in-flight attempt"},
				} {
					before := m8InstallSnapshot(t, f, m8InstallOp1)
					_, err := svc.RecordResource(t.Context(), m8InstallOp1, claim.owner, "data_directory", claim.attempt, m8InstallApplied())
					if before != m8InstallSnapshot(t, f, m8InstallOp1) {
						t.Fatalf("%s changed persisted receipt", claim.label)
					}
					if !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("%s did not conflict: %v", claim.label, err)
					}
				}
				m8InstallExpire(t, f, m8InstallOp1)
				peer := f
				peer.db = m8InstallSecondDB(t, f, "")
				replica := m8JournalService(t, peer)
				op, err := replica.Observe(t.Context(), m8InstallOp1)
				if err != nil || op.State != domain.ProbeInstallInterrupted || op.ErrorCode != "deadline_expired" || op.Resources[0].State != domain.ProbeInstallResourceUnknown {
					t.Fatalf("cold read fabricated safe completion: %v", err)
				}
				before := m8InstallSnapshot(t, f, m8InstallOp1)
				if _, err := svc.RecordResource(t.Context(), m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("late report repaired uncertainty: %v", err)
				}
				if before != m8InstallSnapshot(t, f, m8InstallOp1) {
					t.Fatal("late report changed historical uncertainty")
				}
				if receipt, fresh, err := svc.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1); err != nil || fresh || receipt.Resources[0].State != domain.ProbeInstallResourceUnknown {
					t.Fatalf("expired intent granted again: fresh=%v err=%v", fresh, err)
				}
			})
			t.Run("LastResourceCancelRace", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				op := m8JournalAdmit(t, svc, domain.ProbeInstallSystemd)
				attempts := []string{m8InstallAttempt1, m8InstallAttempt2, m8InstallAttempt3, m8InstallAttempt4}
				for i, spec := range op.Identity.Resources() {
					if _, _, err := svc.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, spec.Kind, attempts[i]); err != nil {
						t.Fatal(err)
					}
					if i != 3 {
						if _, err := svc.RecordResource(t.Context(), m8InstallOp1, m8InstallOwnerA, spec.Kind, attempts[i], m8InstallApplied()); err != nil {
							t.Fatal(err)
						}
					}
				}
				peer := f
				peer.db = m8InstallSecondDB(t, f, "")
				replica := m8JournalService(t, peer)
				start := make(chan struct{})
				results := make(chan error, 2)
				go func() {
					<-start
					_, err := svc.RecordResource(t.Context(), m8InstallOp1, m8InstallOwnerA, "service", m8InstallAttempt4, m8InstallApplied())
					results <- err
				}()
				go func() { <-start; _, err := replica.Cancel(t.Context(), m8InstallOp1); results <- err }()
				close(start)
				for range 2 {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				last, err := svc.Observe(t.Context(), m8InstallOp1)
				if err != nil || last.State != domain.ProbeInstallCancelRequested || last.Resources[3].State != domain.ProbeInstallResourceApplied || last.Terminal() {
					t.Fatalf("last applied lost cancellation intent: %v", err)
				}
			})
			t.Run("ConcurrentReadersAndReservations", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				m8JournalAdmit(t, svc, domain.ProbeInstallSystemd)
				peer := f
				peer.db = m8InstallSecondDB(t, f, "")
				replica := m8JournalService(t, peer)
				var wg sync.WaitGroup
				start := make(chan struct{})
				grants := make(chan bool, 8)
				errs := make(chan error, 12)
				for i := range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						target := svc
						if i%2 == 1 {
							target = replica
						}
						_, fresh, err := target.ReserveResource(t.Context(), m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
						grants <- fresh
						errs <- err
					}()
				}
				for range 4 {
					wg.Add(1)
					go func() { defer wg.Done(); <-start; _, err := replica.Observe(t.Context(), m8InstallOp1); errs <- err }()
				}
				close(start)
				wg.Wait()
				close(grants)
				close(errs)
				count := 0
				for fresh := range grants {
					if fresh {
						count++
					}
				}
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
				if count != 1 {
					t.Fatalf("one committed intent returned %d fresh grants", count)
				}
			})
			t.Run("CorruptionBeforeExpiry", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				m8JournalAdmit(t, svc, domain.ProbeInstallSystemd)
				m8InstallExpire(t, f, m8InstallOp1)
				if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_install_resources SET reference = '/not-owned' WHERE operation_id = ? AND ordinal = 1", m8InstallOp1); err != nil {
					t.Fatal(err)
				}
				before := m8InstallSnapshot(t, f, m8InstallOp1)
				checks := []func() error{
					func() error { _, e := svc.Observe(t.Context(), m8InstallOp1); return e },
					func() error { _, e := svc.Cancel(t.Context(), m8InstallOp1); return e },
					func() error { _, e := svc.Begin(t.Context(), m8InstallOp1, m8InstallOwnerA); return e },
					func() error {
						_, _, e := svc.Create(t.Context(), m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerB)
						return e
					},
				}
				for _, check := range checks {
					if err := check(); err != domain.ErrInternal {
						t.Fatalf("corrupt graph not secret-safe unavailable: %v", err)
					}
				}
				if before != m8InstallSnapshot(t, f, m8InstallOp1) {
					t.Fatal("expiry repaired corrupt graph")
				}
				var reference string
				if err := f.db.NewRaw("SELECT reference FROM probe_install_resources WHERE operation_id = ? AND ordinal = 1", m8InstallOp1).Scan(t.Context(), &reference); err != nil || reference != "/not-owned" {
					t.Fatalf("corrupt reference changed: %v", err)
				}
			})
		})
	}
}
