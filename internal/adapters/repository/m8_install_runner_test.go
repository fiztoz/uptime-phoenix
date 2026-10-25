package repository_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type m8RunnerExecutor func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error)

func (f m8RunnerExecutor) ApplyResource(ctx context.Context, op domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
	return f(ctx, op, kind)
}

type m8RunnerResult struct {
	op  *domain.ProbeInstallOperation
	err error
}

func m8RunnerWait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(8 * time.Second):
		t.Fatal("runner lifecycle did not join")
		var zero T
		return zero
	}
}
func m8RunnerAttempts(mode string) []string {
	out := []string{m8InstallAttempt1, m8InstallAttempt2, m8InstallAttempt3, m8InstallAttempt4}
	if mode == domain.ProbeInstallDocker {
		out = out[:3]
	}
	return out
}
func m8Runner(t *testing.T, svc *services.ProbeInstallJournalService, exec ports.ProbeInstallResourceExecutor) *services.ProbeInstallRunner {
	t.Helper()
	r, err := services.NewProbeInstallRunner(svc, exec)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func m8RunnerAdmit(t *testing.T, svc *services.ProbeInstallJournalService, mode string) domain.ProbeInstallIdentity {
	t.Helper()
	identity := m8InstallIdentity(mode, m8InstallOp1)
	if _, fresh, err := svc.Create(t.Context(), identity, m8InstallOwnerA); err != nil || !fresh {
		t.Fatalf("runner create: fresh=%v err=%v", fresh, err)
	}
	return identity
}

type m8RunnerUnavailable struct {
	ports.ProbeInstallOperationRepository
	unavailable atomic.Bool
}

func (r *m8RunnerUnavailable) ObserveInstall(ctx context.Context, id string) (*domain.ProbeInstallOperation, error) {
	if r.unavailable.Load() {
		return nil, errors.New("private observation diagnostic")
	}
	return r.ProbeInstallOperationRepository.ObserveInstall(ctx, id)
}

func TestM8InstallRunner(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ResourcesReady", func(t *testing.T) {
				for _, mode := range []string{domain.ProbeInstallSystemd, domain.ProbeInstallDocker} {
					t.Run(mode, func(t *testing.T) {
						f := newProbeRegistryFixture(t, engine)
						svc := m8JournalService(t, f)
						identity := m8RunnerAdmit(t, svc, mode)
						peer := f
						peer.db = m8InstallSecondDB(t, f, "")
						reader := m8JournalService(t, peer)
						var calls int
						exec := m8RunnerExecutor(func(ctx context.Context, grant domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
							visible, err := reader.Observe(ctx, identity.OperationID)
							if err != nil {
								return domain.ProbeInstallResourceOutcome{}, err
							}
							index := calls
							calls++
							if visible.State != domain.ProbeInstallRunning || visible.OwnerID != m8InstallOwnerA || visible.Resources[index].State != domain.ProbeInstallResourceInFlight || visible.Resources[index].AttemptID != m8RunnerAttempts(mode)[index] || grant.Identity != identity || grant.Resources[index].Spec.Kind != kind {
								t.Error("dispatch lacked exact committed intent")
								return domain.ProbeInstallResourceOutcome{}, domain.ErrInternal
							}
							for i := 0; i < index; i++ {
								if visible.Resources[i].State != domain.ProbeInstallResourceApplied {
									t.Error("dispatch preceded committed predecessor")
									return domain.ProbeInstallResourceOutcome{}, domain.ErrInternal
								}
							}
							grant.Resources[index].Spec.Reference = "/executor-substitution"
							return m8InstallApplied(), nil
						})
						op, err := m8Runner(t, svc, exec).Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(mode))
						if err != nil || op == nil || op.State != domain.ProbeInstallResourcesReady || op.Terminal() || calls != len(identity.Resources()) {
							t.Fatalf("resource preparation: calls=%d op=%v err=%v", calls, op, err)
						}
						stored, err := reader.Observe(t.Context(), identity.OperationID)
						if err != nil || !stored.Valid() {
							t.Fatalf("persisted readiness: %v", err)
						}
						for _, resource := range stored.Resources {
							if resource.State != domain.ProbeInstallResourceApplied || resource.Spec.Reference == "/executor-substitution" {
								t.Fatal("executor changed journal identity or preparation history")
							}
						}
						if mode == domain.ProbeInstallDocker && stored.Resources[0].Spec.Owned {
							t.Fatal("shared cache became owned")
						}
					})
				}
			})
			t.Run("ConcurrentRunSingleBegin", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				started, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(release) })
				var calls atomic.Int32
				exec := m8RunnerExecutor(func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
					if calls.Add(1) == 1 {
						close(started)
						<-release
					}
					return m8InstallApplied(), nil
				})
				r := m8Runner(t, svc, exec)
				done := make(chan m8RunnerResult, 1)
				go func() {
					op, err := r.Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode))
					done <- m8RunnerResult{op, err}
				}()
				m8RunnerWait(t, started)
				if _, err := r.Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode)); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("second run acquired dispatch: %v", err)
				}
				once.Do(func() { close(release) })
				result := m8RunnerWait(t, done)
				if result.err != nil || result.op.State != domain.ProbeInstallResourcesReady || calls.Load() != 4 {
					t.Fatalf("single begin: calls=%d err=%v", calls.Load(), result.err)
				}
			})
			t.Run("LostReservationAckNoInvocation", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				store := m8InstallStore(f)
				faulty, err := services.NewProbeInstallJournalService(store, m8JournalLostAck{ProbeInstallResourceJournal: store, cancel: cancel})
				if err != nil {
					t.Fatal(err)
				}
				var calls int
				exec := m8RunnerExecutor(func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
					calls++
					return m8InstallApplied(), nil
				})
				if _, err := m8Runner(t, faulty, exec).Run(ctx, identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode)); !errors.Is(err, context.Canceled) {
					t.Fatalf("lost ack: %v", err)
				}
				stored, err := svc.Observe(t.Context(), identity.OperationID)
				if err != nil || calls != 0 || stored.Resources[0].State != domain.ProbeInstallResourceInFlight {
					t.Fatalf("ambiguous reservation was invoked or fabricated: calls=%d err=%v", calls, err)
				}
				if _, err := m8Runner(t, svc, exec).Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode)); !errors.Is(err, ports.ErrConflict) || calls != 0 {
					t.Fatalf("cold retry redispatched: calls=%d err=%v", calls, err)
				}
			})
			t.Run("RemoteCancelJoinsBeforeTerminal", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(release) })
				exec := m8RunnerExecutor(func(ctx context.Context, _ domain.ProbeInstallOperation, _ string) (domain.ProbeInstallResourceOutcome, error) {
					close(started)
					<-ctx.Done()
					close(canceled)
					<-release
					return m8InstallNotStartedOutcome(), nil
				})
				done := make(chan m8RunnerResult, 1)
				r := m8Runner(t, svc, exec)
				go func() {
					op, err := r.Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode))
					done <- m8RunnerResult{op, err}
				}()
				m8RunnerWait(t, started)
				peer := f
				peer.db = m8InstallSecondDB(t, f, "")
				remote := m8JournalService(t, peer)
				if op, err := remote.Cancel(t.Context(), identity.OperationID); err != nil || op.State != domain.ProbeInstallCancelRequested {
					t.Fatalf("replica cancel: %v", err)
				}
				m8RunnerWait(t, canceled)
				select {
				case <-done:
					t.Fatal("terminal cancellation returned before executor joined")
				default:
				}
				pending, err := remote.Observe(t.Context(), identity.OperationID)
				if err != nil || pending.State != domain.ProbeInstallCancelRequested || pending.Resources[0].State != domain.ProbeInstallResourceInFlight {
					t.Fatalf("unjoined effect invented terminal outcome: %v", err)
				}
				once.Do(func() { close(release) })
				result := m8RunnerWait(t, done)
				if result.err != nil || result.op.State != domain.ProbeInstallCanceled || result.op.Resources[0].State != domain.ProbeInstallResourceNotStarted {
					t.Fatalf("joined cancel: %v", result.err)
				}
			})
			t.Run("OwnerLossUnknownStopsDependents", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				exec := m8RunnerExecutor(func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
					calls++
					cancel()
					return m8InstallApplied(), errors.New("private executor diagnostic")
				})
				op, err := m8Runner(t, svc, exec).Run(ctx, identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode))
				if !errors.Is(err, domain.ErrInternal) || op == nil || op.State != domain.ProbeInstallInterrupted || op.ErrorCode != "outcome_unknown" || op.Resources[0].State != domain.ProbeInstallResourceUnknown || calls != 1 {
					t.Fatalf("owner loss hid uncertainty or dispatched dependent: calls=%d err=%v", calls, err)
				}
				stored, _ := svc.Observe(t.Context(), identity.OperationID)
				if stored.Resources[1].State != domain.ProbeInstallResourcePlanned {
					t.Fatal("uncertainty fabricated dependent work")
				}
			})
			t.Run("FinalAppliedOwnerLoss", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := 0
				exec := m8RunnerExecutor(func(_ context.Context, _ domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
					calls++
					if kind == "service" {
						cancel()
					}
					return m8InstallApplied(), nil
				})
				op, err := m8Runner(t, svc, exec).Run(ctx, identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode))
				if !errors.Is(err, context.Canceled) || op == nil || op.State != domain.ProbeInstallInterrupted || op.ErrorCode != "owner_interrupted" || calls != 4 {
					t.Fatalf("last applied bypassed known owner loss: calls=%d state=%v err=%v", calls, op, err)
				}
				stored, err := svc.Observe(t.Context(), identity.OperationID)
				if err != nil || stored.State != domain.ProbeInstallInterrupted {
					t.Fatalf("last-resource interruption not persisted: %v", err)
				}
				for _, resource := range stored.Resources {
					if resource.State != domain.ProbeInstallResourceApplied {
						t.Fatal("owner loss erased definite final effect")
					}
				}
			})
			t.Run("ColdExpiryRejectsLateApplied", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				calls := 0
				exec := m8RunnerExecutor(func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
					calls++
					m8InstallExpire(t, f, identity.OperationID)
					cold, err := svc.Observe(t.Context(), identity.OperationID)
					if err != nil || cold.State != domain.ProbeInstallInterrupted || cold.Resources[0].State != domain.ProbeInstallResourceUnknown {
						t.Fatalf("cold expiry: %v", err)
					}
					return m8InstallApplied(), nil
				})
				if _, err := m8Runner(t, svc, exec).Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode)); !errors.Is(err, ports.ErrConflict) || calls != 1 {
					t.Fatalf("late applied accepted after expiry: calls=%d err=%v", calls, err)
				}
				stored, err := svc.Observe(t.Context(), identity.OperationID)
				if err != nil || stored.ErrorCode != "deadline_expired" || stored.Resources[0].State != domain.ProbeInstallResourceUnknown {
					t.Fatalf("late result repaired uncertainty: %v", err)
				}
			})
			t.Run("MalformedOutcomeCannotAdvance", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallDocker)
				calls := 0
				exec := m8RunnerExecutor(func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
					calls++
					return domain.ProbeInstallResourceOutcome{State: "installed", ErrorCode: "private output"}, nil
				})
				op, err := m8Runner(t, svc, exec).Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode))
				if !errors.Is(err, domain.ErrInternal) || op == nil || op.ErrorCode != "outcome_unknown" || calls != 1 {
					t.Fatalf("malformed executor result advanced: calls=%d err=%v", calls, err)
				}
			})
			t.Run("IdentityOwnerNoDispatch", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				svc := m8JournalService(t, f)
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				before := m8InstallSnapshot(t, f, identity.OperationID)
				calls := 0
				exec := m8RunnerExecutor(func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
					calls++
					return m8InstallApplied(), nil
				})
				r := m8Runner(t, svc, exec)
				changed := identity
				changed.RequestedBy++
				if _, err := r.Run(t.Context(), changed, m8InstallOwnerA, m8RunnerAttempts(identity.Mode)); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("different immutable identity admitted: %v", err)
				}
				if _, err := r.Run(t.Context(), identity, m8InstallOwnerB, m8RunnerAttempts(identity.Mode)); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("foreign owner admitted: %v", err)
				}
				if calls != 0 || before != m8InstallSnapshot(t, f, identity.OperationID) {
					t.Fatal("foreign run began or dispatched")
				}
			})
			t.Run("ObservationLossStopsDispatch", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				store := m8InstallStore(f)
				unavailable := &m8RunnerUnavailable{ProbeInstallOperationRepository: store}
				svc, err := services.NewProbeInstallJournalService(unavailable, store)
				if err != nil {
					t.Fatal(err)
				}
				identity := m8RunnerAdmit(t, svc, domain.ProbeInstallSystemd)
				calls := 0
				exec := m8RunnerExecutor(func(ctx context.Context, _ domain.ProbeInstallOperation, _ string) (domain.ProbeInstallResourceOutcome, error) {
					calls++
					unavailable.unavailable.Store(true)
					<-ctx.Done()
					return m8InstallApplied(), nil
				})
				op, err := m8Runner(t, svc, exec).Run(t.Context(), identity, m8InstallOwnerA, m8RunnerAttempts(identity.Mode))
				if !errors.Is(err, domain.ErrInternal) || op == nil || op.State != domain.ProbeInstallInterrupted || op.ErrorCode != "owner_interrupted" || calls != 1 {
					t.Fatalf("observation loss advanced: calls=%d err=%v", calls, err)
				}
				stored, err := store.ObserveInstall(t.Context(), identity.OperationID)
				if err != nil || stored.Resources[0].State != domain.ProbeInstallResourceApplied || stored.Resources[1].State != domain.ProbeInstallResourcePlanned {
					t.Fatalf("observation loss erased joined evidence or dispatched dependent: %v", err)
				}
			})
		})
	}
}
