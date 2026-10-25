package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type runnerForeignFinish struct {
	ports.ProbeInstallOperationRepository
}

func (r runnerForeignFinish) FinishInstall(ctx context.Context, id, owner string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
	op, err := r.ProbeInstallOperationRepository.FinishInstall(ctx, id, owner, outcome)
	if err == nil {
		op.Identity.RequestedBy++
	}
	return op, err
}

func TestInstallRunnerRejectsForeignFinishAcknowledgement(t *testing.T) {
	fx := runnerSetup(t, domain.ProbeInstallSystemd)
	journal, err := NewProbeInstallJournalService(runnerForeignFinish{fx.fake}, fx.fake)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewProbeInstallRunner(journal, fx.exec)
	if err != nil {
		t.Fatal(err)
	}
	fx.exec.apply = func(context.Context, domain.ProbeInstallOperation, string) (domain.ProbeInstallResourceOutcome, error) {
		return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}, nil
	}
	op, err := r.Run(t.Context(), fx.identity, fx.owner, fx.attempts)
	if !errors.Is(err, ports.ErrConflict) || (op != nil && op.Identity != fx.identity) {
		t.Fatalf("foreign finish acknowledgement surfaced: op=%v err=%v", op, err)
	}
}

func TestInstallRunnerJoinedAppliedCancellationAtEveryPosition(t *testing.T) {
	for _, position := range []int{0, 3} {
		name := "first"
		if position == 3 {
			name = "last"
		}
		t.Run(name, func(t *testing.T) {
			fx := runnerSetup(t, domain.ProbeInstallSystemd)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fx.exec.apply = func(_ context.Context, _ domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
				if kind == fx.identity.Resources()[position].Kind {
					cancel()
				}
				return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}, nil
			}
			op, err := fx.runner.Run(ctx, fx.identity, fx.owner, fx.attempts)
			if !errors.Is(err, context.Canceled) || op == nil || op.State != domain.ProbeInstallInterrupted || op.ErrorCode != "owner_interrupted" {
				t.Fatalf("joined applied lost owner cancellation at %s resource: state=%v err=%v", name, op, err)
			}
			stored := fx.fake.snapshot()
			if stored.Resources[position].State != domain.ProbeInstallResourceApplied {
				t.Fatal("cancellation erased definite joined effect")
			}
			if fx.exec.callCount() != position+1 {
				t.Fatal("owner cancellation dispatched a dependent")
			}
		})
	}
}
