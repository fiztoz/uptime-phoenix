package probe

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Cached regional tuples are selectors only. Each retry independently verifies
// the committed report/document/current assignment and stored session in the DB.
type preparationSelection struct {
	mu    sync.Mutex
	facts []domain.ProbeDeliveryModeApplication
}

func (s *preparationSelection) retain(facts []domain.ProbeDeliveryModeApplication) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = slices.Clone(facts)
}
func (s *preparationSelection) capture() []domain.ProbeDeliveryModeApplication {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.facts)
}

func (t *HubTransport) reconcileDeliveryPreparations(ctx context.Context, authority domain.ProbeReplaySession, selection *preparationSelection, stop func()) {
	interval := t.preparationReconcileInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			facts := selection.capture()
			if len(facts) == 0 {
				continue
			} // No negotiated committed report, no issuance.
			opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := t.deliveryPreparations.IssueRemoteDeliveryPreparations(opCtx, authority, facts)
			cancel()
			if err != nil {
				stop()
				return
			}
		}
	}
}
