package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestOperativeAttemptBackfill: an rc.4 hub reconciled a crashed aggregate
// delivery to failed/provider_outcome_unknown but left its expired grant
// in_flight forever, and the claim selector never revisits a terminal delivery
// (#219). Migration 094 must settle exactly those rows, as the current
// reconciler would have, and leave a grant whose budget is still live alone.
func TestOperativeAttemptBackfill(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 2, true)
			awaitOperativeObservationClock(t, driveOperativeActive(t, o))
			o.record(t, domain.StatusDown)
			if got := o.run(t, true); !got.Changed {
				t.Fatal("fresh post-activation DOWN did not admit work")
			}
			db := o.p.f.db
			sender := &restartRecordingSender{db: db}
			resolve := func(string) (ports.NotificationSender, bool) { return sender, true }
			journal := lostAggregateJournalReceipt{SourceProviderAttemptJournal: o.store}
			svc, err := services.NewOperativeDeliveryService(o.store, o.store, journal, operativeOwner, true, resolve)
			if err != nil {
				t.Fatal(err)
			}
			if worked, err := svc.ProcessNext(t.Context()); !worked || err == nil {
				t.Fatal("did not inject post-provider crash boundary", worked, err)
			}
			var deliveryID string
			if err := db.NewSelect().Table("operative_aggregate_deliveries").Column("delivery_id").Where("status = 'leased'").Scan(t.Context(), &deliveryID); err != nil {
				t.Fatal(err)
			}
			past := time.Now().UTC().Add(-3 * time.Minute)
			if _, err := db.ExecContext(t.Context(), "UPDATE operative_aggregate_deliveries SET leased_at=?, lease_until=? WHERE delivery_id=?", past.Add(-time.Minute), past, deliveryID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), "UPDATE operative_provider_attempts SET budget_until=? WHERE delivery_id=?", past, deliveryID); err != nil {
				t.Fatal(err)
			}
			cold := newOperativeStore(t, o.p.f)
			coldSvc, err := services.NewOperativeDeliveryService(cold, cold, cold, operativeOwner, true, resolve)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := coldSvc.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			var delivery struct {
				Status    string
				ErrorCode string
				OutcomeAt time.Time
			}
			if err := db.NewSelect().Table("operative_aggregate_deliveries").Column("status", "error_code", "outcome_at").Where("delivery_id = ?", deliveryID).Scan(t.Context(), &delivery); err != nil {
				t.Fatal(err)
			}
			if delivery.Status != domain.DeliveryStatusFailed || delivery.ErrorCode != "provider_outcome_unknown" {
				t.Fatalf("crashed delivery = %+v, want failed/provider_outcome_unknown", delivery)
			}

			// Put the journal back the way an rc.4 hub left it.
			if _, err := db.ExecContext(t.Context(), "UPDATE operative_provider_attempts SET state='in_flight', finished_at=NULL WHERE delivery_id=?", deliveryID); err != nil {
				t.Fatal(err)
			}
			attempt := func() (state string, finished *time.Time) {
				t.Helper()
				var row struct {
					State      string
					FinishedAt *time.Time
				}
				if err := db.NewSelect().Table("operative_provider_attempts").Column("state", "finished_at").Where("delivery_id = ? AND scope = 'aggregate'", deliveryID).Scan(t.Context(), &row); err != nil {
					t.Fatal(err)
				}
				return row.State, row.FinishedAt
			}

			// A grant whose budget is still live may yet complete: untouched.
			future := time.Now().UTC().Add(time.Hour)
			if _, err := db.ExecContext(t.Context(), "UPDATE operative_provider_attempts SET budget_until=? WHERE delivery_id=?", future, deliveryID); err != nil {
				t.Fatal(err)
			}
			if err := runAlertMigration(t, o.p.f, "094_settle_orphaned_aggregate_attempts", "up"); err != nil {
				t.Fatal("094 up:", err)
			}
			if state, finished := attempt(); state != string(domain.ProviderAttemptInFlight) || finished != nil {
				t.Fatalf("backfill settled a live grant: %s %v", state, finished)
			}

			if _, err := db.ExecContext(t.Context(), "UPDATE operative_provider_attempts SET budget_until=? WHERE delivery_id=?", past, deliveryID); err != nil {
				t.Fatal(err)
			}
			succeededBefore, err := db.NewSelect().Table("operative_provider_attempts").Where("state = ?", domain.ProviderAttemptSucceeded).Count(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for range 2 { // Re-running is a no-op.
				if err := runAlertMigration(t, o.p.f, "094_settle_orphaned_aggregate_attempts", "up"); err != nil {
					t.Fatal("094 up:", err)
				}
			}
			state, finished := attempt()
			if state != string(domain.ProviderAttemptAmbiguous) || finished == nil {
				t.Fatalf("orphaned grant = %s %v, want ambiguous with finished_at", state, finished)
			}
			if !finished.UTC().Equal(delivery.OutcomeAt.UTC()) {
				t.Fatalf("finished_at = %s, want the delivery's outcome_at %s", finished.UTC(), delivery.OutcomeAt.UTC())
			}
			inFlight, err := db.NewSelect().Table("operative_provider_attempts").Where("monitor_id = ? AND state = ?", o.p.monitor.ID, domain.ProviderAttemptInFlight).Count(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			succeededAfter, err := db.NewSelect().Table("operative_provider_attempts").Where("state = ?", domain.ProviderAttemptSucceeded).Count(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if inFlight != 0 || succeededAfter != succeededBefore {
				t.Fatalf("backfill left %d in_flight, succeeded %d -> %d", inFlight, succeededBefore, succeededAfter)
			}
			if err := runAlertMigration(t, o.p.f, "094_settle_orphaned_aggregate_attempts", "down"); err != nil {
				t.Fatal("094 down:", err)
			}
		})
	}
}
