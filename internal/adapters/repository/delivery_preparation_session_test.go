package repository_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type preparationReceiptRefuser struct {
	dispatcher ports.ProbeCommandDispatcher
	refuse     atomic.Bool
	refused    chan domain.ProbeCommandOutcome
}

func (r *preparationReceiptRefuser) NextCommand(ctx context.Context, s domain.ProbeReplaySession, budget time.Duration, c domain.ProbeCommandCapabilities) (*domain.ProbeCommandDispatch, error) {
	return r.dispatcher.NextCommand(ctx, s, budget, c)
}
func (r *preparationReceiptRefuser) RecordCommandResult(ctx context.Context, s domain.ProbeReplaySession, out domain.ProbeCommandOutcome) (bool, error) {
	if r.refuse.CompareAndSwap(true, false) {
		r.refused <- out
		return false, domain.ErrInternal
	}
	return r.dispatcher.RecordCommandResult(ctx, s, out)
}

// TestDeliveryPreparationSessionContract verifies real config/report/preparation
// and refused-result recovery over pinned TLS/WebSocket on both hub engines.
// TestDeliveryPreparationRoundSessionContract exercises periodic DB-backed round
// rearm through real TLS/WebSocket, encrypted source state and immutable receipts.
func TestDeliveryPreparationRoundSessionContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) {
					testAssignmentRevisionSession(t, newProbeRegistryFixture(t, engine), mode, true, true)
				})
			}
		})
	}
}

func TestDeliveryPreparationSessionContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryRegional, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) { testAssignmentRevisionSession(t, newProbeRegistryFixture(t, engine), mode, true) })
			}
		})
	}
}
