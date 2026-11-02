package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestClaimAggregateDeliveryEmptyQueue pins the idle claim contract on both
// engines. Bun reports an empty slice scan as success, not sql.ErrNoRows, so a
// claim that indexed the first row crash-looped every hub with
// PROBES_AGGREGATE_PAGING_ENABLED=true as soon as the worker polled an idle
// queue.
func TestClaimAggregateDeliveryEmptyQueue(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			store := newOperativeStore(t, f)
			claimed, err := store.ClaimAggregateDelivery(t.Context(), operativeOwner, 30*time.Second)
			if !errors.Is(err, ports.ErrNotFound) || claimed != nil {
				t.Fatalf("idle claim = (%v, %v), want (nil, ErrNotFound)", claimed, err)
			}
		})
	}
}

// TestOperativeDeliveryProcessNextIdle drives the production worker entry point
// against a real store with nothing to claim: it must report no work, not
// panic or fail.
func TestOperativeDeliveryProcessNextIdle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			store := newOperativeStore(t, f)
			sent := false
			service, err := services.NewOperativeDeliveryService(store, store, store, operativeOwner, true, func(string) (ports.NotificationSender, bool) {
				sent = true
				return &reviewSender{}, true
			})
			if err != nil {
				t.Fatal(err)
			}
			worked, err := service.ProcessNext(t.Context())
			if worked || err != nil || sent {
				t.Fatalf("idle ProcessNext = (%v, %v), sender resolved %v; want (false, nil) and no sender", worked, err, sent)
			}
		})
	}
}
