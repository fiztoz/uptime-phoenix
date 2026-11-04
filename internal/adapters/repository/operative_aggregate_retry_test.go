package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestOperativeAggregateBoundedRetry preserves deliberately committed retries
// without turning an ambiguous provider return into an endless leased loop.
func TestOperativeAggregateBoundedRetry(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
			awaitOperativeObservationClock(t, driveOperativeActive(t, o))
			o.record(t, domain.StatusDown)
			o.run(t, true)
			sender := &restartRecordingSender{db: o.p.f.db, reviewSender: reviewSender{err: context.DeadlineExceeded}}
			for attempt := 1; attempt <= 5; attempt++ {
				cold := newOperativeStore(t, o.p.f)
				svc, err := services.NewOperativeDeliveryService(cold, cold, cold, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return sender, true })
				if err != nil {
					t.Fatal(err)
				}
				if worked, err := svc.ProcessNext(t.Context()); !worked || err != nil {
					t.Fatalf("attempt %d: worked=%t err=%v", attempt, worked, err)
				}
				wantStatus := "retrying"
				if attempt == 5 {
					wantStatus = "failed"
				}
				var row struct {
					Status  string
					Attempt int64
				}
				if err := o.p.f.db.NewSelect().Table("operative_aggregate_deliveries").Column("status", "attempt").Scan(t.Context(), &row); err != nil {
					t.Fatal(err)
				}
				if row.Status != wantStatus || row.Attempt != int64(attempt) {
					t.Fatalf("attempt %d: %+v", attempt, row)
				}
				if attempt < 5 {
					if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_aggregate_deliveries SET available_at=?", time.Now().UTC().Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
			}
			cold := newOperativeStore(t, o.p.f)
			svc, err := services.NewOperativeDeliveryService(cold, cold, cold, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return sender, true })
			if err != nil {
				t.Fatal(err)
			}
			if worked, err := svc.ProcessNext(t.Context()); worked || err != nil {
				t.Fatal("terminal retry was reclaimed", worked, err)
			}
			if len(sender.alerts) != 5 {
				t.Fatalf("provider requests=%d, want 5", len(sender.alerts))
			}
			if attempts := o.attempts(t); len(attempts) != 5 {
				t.Fatalf("journal grants=%d, want 5", len(attempts))
			} else {
				for _, a := range attempts {
					if a.State != "ambiguous" {
						t.Fatal("provider errors lost ambiguity", a)
					}
				}
			}
		})
	}
}
