package repository_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type lostAggregateJournalReceipt struct {
	ports.SourceProviderAttemptJournal
	commit bool
}

func (j lostAggregateJournalReceipt) FinishSourceProviderAttempt(ctx context.Context, id string, state domain.ProviderAttemptState) error {
	if j.commit {
		if err := j.SourceProviderAttemptJournal.FinishSourceProviderAttempt(ctx, id, state); err != nil {
			return err
		}
	}
	return errors.New("injected provider receipt loss")
}

type restartRecordingSender struct {
	reviewSender
	mu sync.Mutex
	db *bun.DB
}

func (s *restartRecordingSender) Send(ctx context.Context, config map[string]any, a domain.AlertContext) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The production queue receipt uses the caller's clock. An instantaneous
	// fake sender can return before the DB-clock lease even began on a host
	// slightly behind its container; wait past the actual persisted grant.
	var grant struct{ CreatedAt time.Time }
	if err := s.db.NewSelect().Table("operative_provider_attempts").Column("created_at").
		Where("scope = ? AND state = ?", "aggregate", "in_flight").
		OrderExpr("created_at DESC, attempt_id DESC").Limit(1).Scan(ctx, &grant); err != nil {
		return err
	}
	if wait := time.Until(grant.CreatedAt.Add(time.Millisecond)); wait > 0 {
		if wait > 2*time.Second {
			return errors.New("host/database clock skew exceeds fixture allowance")
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.reviewSender.Send(ctx, config, a)
}

// TestOperativeAggregateReceiptLoss sends through the production consumer, loses
// persistence before/after the provider journal commit, then starts competing
// cold consumers. The other channel must send; the accepted request must not.
func TestOperativeAggregateReceiptLoss(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, point := range []string{"before_journal_commit", "after_journal_commit"} {
				t.Run(point, func(t *testing.T) {
					o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 2, true)
					awaitOperativeObservationClock(t, driveOperativeActive(t, o))
					o.record(t, domain.StatusDown)
					if got := o.run(t, true); !got.Changed {
						t.Fatal("fresh post-activation DOWN did not admit work")
					}
					sender := &restartRecordingSender{db: o.p.f.db}
					resolve := func(string) (ports.NotificationSender, bool) { return sender, true }
					journal := lostAggregateJournalReceipt{SourceProviderAttemptJournal: o.store, commit: point == "after_journal_commit"}
					svc, err := services.NewOperativeDeliveryService(o.store, o.store, journal, operativeOwner, true, resolve)
					if err != nil {
						t.Fatal(err)
					}
					if worked, err := svc.ProcessNext(t.Context()); !worked || err == nil {
						t.Fatal("did not inject post-provider crash boundary", worked, err)
					}
					if len(sender.alerts) != 1 {
						t.Fatal("provider did not receive first request")
					}
					var old struct {
						DeliveryID     string
						PrimaryProbeID string
						Attempt        int64
						LeaseToken     string
					}
					if err := o.p.f.db.NewSelect().Table("operative_aggregate_deliveries").Column("delivery_id", "primary_probe_id", "attempt", "lease_token").Where("status = 'leased'").Scan(t.Context(), &old); err != nil {
						t.Fatal(err)
					}
					past := time.Now().UTC().Add(-3 * time.Minute)
					if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_aggregate_deliveries SET leased_at=?, lease_until=? WHERE delivery_id=?", past.Add(-time.Minute), past, old.DeliveryID); err != nil {
						t.Fatal(err)
					}
					if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_provider_attempts SET budget_until=? WHERE delivery_id=?", past, old.DeliveryID); err != nil {
						t.Fatal(err)
					}
					// Each service has a distinct store instance, sharing only the database.
					servicesAfterRestart := make([]*services.OperativeDeliveryService, 4)
					for i := range servicesAfterRestart {
						cold := newOperativeStore(t, o.p.f)
						servicesAfterRestart[i], err = services.NewOperativeDeliveryService(cold, cold, cold, operativeOwner, true, resolve)
						if err != nil {
							t.Fatal(err)
						}
					}
					start := make(chan struct{})
					results := make(chan error, len(servicesAfterRestart))
					for _, cold := range servicesAfterRestart {
						go func() { <-start; _, err := cold.ProcessNext(t.Context()); results <- err }()
					}
					close(start)
					for range servicesAfterRestart {
						if err := <-results; err != nil {
							t.Error("cold consumer:", err)
						}
					}
					if len(sender.alerts) != 2 {
						t.Fatalf("provider received %d requests, want one per channel", len(sender.alerts))
					}
					statuses := o.deliveryStatuses(t)
					wantSent := 1
					if journal.commit {
						wantSent = 2
					}
					if statuses["sent"] != wantSent || statuses["failed"] != 2-wantSent || statuses["leased"] != 0 {
						t.Fatalf("queue did not settle: %v", statuses)
					}
					stale := domain.DeliveryClaim{DeliveryID: old.DeliveryID, ProbeID: old.PrimaryProbeID, Attempt: old.Attempt, LeaseToken: old.LeaseToken}
					if grant, err := o.store.AuthorizeAggregateDelivery(t.Context(), stale, time.Second); grant != nil || !errors.Is(err, ports.ErrConflict) {
						t.Fatal("stale authorization revived reconciled delivery", err)
					}
					if err := o.store.FinishAggregateDelivery(t.Context(), stale, domain.DeliveryResult{Status: domain.DeliveryStatusRetrying, ErrorCode: "network_timeout", At: time.Now().UTC(), RetryAt: time.Now().UTC().Add(time.Second)}); !errors.Is(err, ports.ErrConflict) {
						t.Fatal("stale result rearmed reconciled delivery", err)
					}
				})
			}
		})
	}
}
