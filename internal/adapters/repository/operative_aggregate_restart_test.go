package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// awaitOperativeObservationClock keeps fault fixtures on the real causality
// boundary: host-recorded observations must follow DB-clock barrier activation.
// Waiting for DB to catch up after recording cannot repair a pre-activation
// observation when the host clock is slightly behind the database container.
func awaitOperativeObservationClock(t *testing.T, activated time.Time) {
	t.Helper()
	if wait := time.Until(activated.Add(time.Millisecond)); wait > 0 {
		if wait > 2*time.Second {
			t.Fatal("host/database clock skew exceeds fixture allowance")
		}
		time.Sleep(wait)
	}
}

// TestOperativeAggregateRestart exercises the durable boundary between a
// provider grant and the delivery receipt through a cold production consumer.
func TestOperativeAggregateRestart(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, outcome := range []string{"crashed", "succeeded", "ambiguous", "not_started", "ungranted", "retry_drained", "retry_active", "retry_ungranted", "crashed_recovered", "live_budget"} {
				t.Run(outcome, func(t *testing.T) {
					o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
					awaitOperativeObservationClock(t, driveOperativeActive(t, o))
					o.record(t, domain.StatusDown)
					o.run(t, true)
					ctx := t.Context()
					q, err := o.store.ClaimAggregateDelivery(ctx, operativeOwner, 30*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					claim := receipt(*q)
					var attemptID string
					if outcome != "ungranted" {
						grant, err := o.store.AuthorizeAggregateDelivery(ctx, claim, time.Second)
						if err != nil {
							t.Fatal(err)
						}
						attemptID = grant.AttemptID
						state := domain.ProviderAttemptInFlight
						switch outcome {
						case "succeeded":
							state = domain.ProviderAttemptSucceeded
						case "ambiguous", "retry_drained", "retry_active", "retry_ungranted":
							state = domain.ProviderAttemptAmbiguous
						case "not_started":
							state = domain.ProviderAttemptNotStarted
						}
						if state != domain.ProviderAttemptInFlight {
							if err := o.store.FinishSourceProviderAttempt(ctx, attemptID, state); err != nil {
								t.Fatal(err)
							}
						}
					}
					past := time.Now().UTC().Add(-3 * time.Minute)
					if outcome == "retry_drained" || outcome == "retry_active" || outcome == "retry_ungranted" {
						awaitOperativeObservationClock(t, *q.LeasedAt)
						at := time.Now().UTC()
						if err := o.store.FinishAggregateDelivery(ctx, claim, domain.DeliveryResult{Status: domain.DeliveryStatusRetrying, ErrorCode: "network_timeout", At: at, RetryAt: at.Add(time.Second)}); err != nil {
							t.Fatal(err)
						}
						if outcome == "retry_drained" {
							if got := o.drain(t); got.Phase != "released" {
								t.Fatal(got)
							}
						}
						if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_aggregate_deliveries SET available_at=? WHERE delivery_id=?", past, q.DeliveryID); err != nil {
							t.Fatal(err)
						}
					} else {
						if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_aggregate_deliveries SET leased_at=?, lease_until=? WHERE delivery_id=?", past.Add(-time.Minute), past, q.DeliveryID); err != nil {
							t.Fatal(err)
						}
					}
					if outcome != "live_budget" && attemptID != "" {
						if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_provider_attempts SET budget_until=? WHERE attempt_id=?", past, attemptID); err != nil {
							t.Fatal(err)
						}
					}
					if outcome == "retry_ungranted" {
						// Retry decision committed, then the next worker died after
						// claiming but BEFORE it received a new provider grant.
						if _, err := o.store.ClaimAggregateDelivery(ctx, operativeOwner, 30*time.Second); err != nil {
							t.Fatal(err)
						}
						if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_aggregate_deliveries SET leased_at=?, lease_until=? WHERE delivery_id=?", past.Add(-time.Minute), past, q.DeliveryID); err != nil {
							t.Fatal(err)
						}
					}
					// Reconstruct both service and store: no in-memory state survives restart.
					cold := newOperativeStore(t, o.p.f)
					sender := &restartRecordingSender{db: o.p.f.db}
					svc, err := services.NewOperativeDeliveryService(cold, cold, cold, operativeOwner, true, func(string) (ports.NotificationSender, bool) { return sender, true })
					if err != nil {
						t.Fatal(err)
					}
					if outcome == "crashed_recovered" {
						o.record(t, domain.StatusUp)
						o.run(t, true)
					}
					for i := 0; i < 3; i++ {
						if _, err := svc.ProcessNext(ctx); err != nil {
							t.Fatalf("restart pass %d: %v", i, err)
						}
					}
					wantSends := 0
					if outcome == "ungranted" || outcome == "not_started" || outcome == "crashed_recovered" || outcome == "retry_active" || outcome == "retry_ungranted" {
						wantSends = 1
					}
					if len(sender.alerts) != wantSends {
						t.Fatalf("restart sent %d pages, want %d (must not replay admitted unknown I/O)", len(sender.alerts), wantSends)
					}
					if outcome == "crashed_recovered" && sender.alerts[0].Status != domain.StatusUp {
						t.Fatal("recovery did not send UP")
					}
					var row struct {
						Status     string
						Attempt    int64
						LeaseUntil *time.Time
						ErrorCode  string
					}
					if err := o.p.f.db.NewSelect().Table("operative_aggregate_deliveries").Column("status", "attempt", "lease_until", "error_code").Where("delivery_id=?", q.DeliveryID).Scan(ctx, &row); err != nil {
						t.Fatal(err)
					}
					wantStatus := domain.DeliveryStatusFailed
					switch outcome {
					case "succeeded", "ungranted", "not_started", "retry_active", "retry_ungranted":
						wantStatus = domain.DeliveryStatusSent
					case "retry_drained":
						wantStatus = domain.DeliveryStatusSuperseded
					case "live_budget":
						wantStatus = domain.DeliveryStatusLeased
					}
					if row.Status != wantStatus {
						t.Fatalf("delivery = %+v, want %s", row, wantStatus)
					}
					wantAttempt := int64(1)
					if outcome == "ungranted" || outcome == "not_started" || outcome == "retry_active" {
						wantAttempt = 2
					}
					if outcome == "retry_ungranted" {
						wantAttempt = 3
					}
					if row.Attempt != wantAttempt {
						t.Fatalf("restart attempt = %d, want %d", row.Attempt, wantAttempt)
					}
					if wantStatus == domain.DeliveryStatusFailed && row.ErrorCode != "provider_outcome_unknown" {
						t.Fatalf("unknown provider outcome not distinguished: %+v", row)
					}
					if outcome != "live_budget" && row.LeaseUntil != nil {
						t.Fatal("terminal delivery retained lease")
					}
					if outcome == "crashed" || outcome == "crashed_recovered" || outcome == "live_budget" {
						var attempt struct {
							State      string
							FinishedAt *time.Time
						}
						if err := o.p.f.db.NewSelect().Table("operative_provider_attempts").Column("state", "finished_at").Where("attempt_id=?", attemptID).Scan(ctx, &attempt); err != nil {
							t.Fatal(err)
						}
						if attempt.State != "in_flight" || attempt.FinishedAt != nil {
							t.Fatalf("restart fabricated provider completion: %+v", attempt)
						}
					}
					if outcome != "live_budget" {
						if _, err := cold.ClaimAggregateDelivery(ctx, operativeOwner, 30*time.Second); !errors.Is(err, ports.ErrNotFound) {
							t.Fatalf("terminal queue reclaims work: %v", err)
						}
					}
				})
			}
		})
	}
}
