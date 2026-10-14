package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestOperativeProviderFences verifies ordinary execution compatibility and
// provider journals independently of cutover preparation and caller clocks.
func TestOperativeProviderFences(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, scenario := range []string{"MissingRegionalFact", "MissingPreparation", "AvailableAt", "FinishClockRollback"} {
				t.Run(scenario, func(t *testing.T) {
					o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
					ctx := t.Context()
					switch scenario {
					case "MissingRegionalFact":
						if _, err := o.p.f.db.ExecContext(ctx, "DELETE FROM probe_local_regional_applications"); err != nil {
							t.Fatal(err)
						}
					case "MissingPreparation":
						if _, err := o.p.f.db.ExecContext(ctx, "DELETE FROM probe_local_delivery_preparations"); err != nil {
							t.Fatal(err)
						}
					}
					o.record(t, domain.StatusDown)
					at := time.Now().UTC()
					if scenario == "AvailableAt" {
						if _, err := o.p.f.db.ExecContext(ctx, "UPDATE probe_delivery_intents SET available_at=?", at.Add(time.Hour)); err != nil {
							t.Fatal(err)
						}
						at = at.Add(2 * time.Hour)
					}
					claimed, err := o.p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, at, 5*time.Minute, 10)
					if err != nil || len(claimed) != 1 {
						t.Fatal("claim", err, len(claimed))
					}
					active, err := o.p.active.GetActive(ctx, domain.LocalProbeID)
					if err != nil {
						t.Fatal(err)
					}
					grant, err := o.store.AuthorizeLocalDelivery(ctx, receipt(claimed[0]), *active, time.Second)
					if scenario == "AvailableAt" {
						if !errors.Is(err, ports.ErrConflict) || grant != nil {
							t.Fatal("caller clock admitted future work", err)
						}
						return
					}
					if err != nil || grant == nil {
						t.Fatal("ordinary regional authorization became preparation/fact gated", err)
					}
					if scenario == "FinishClockRollback" {
						// Preserve the historical grant timestamp while simulating a
						// backward clock: exact release is identity-, not time-authorized.
						if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_provider_attempts SET created_at=?, budget_until=? WHERE attempt_id=?", at.Add(time.Hour), at.Add(2*time.Hour), grant.AttemptID); err != nil {
							t.Fatal(err)
						}
					}
					if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptSucceeded); err != nil {
						t.Fatal("exact returned attempt did not complete", err)
					}
					if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptAmbiguous); !errors.Is(err, ports.ErrConflict) {
						t.Fatal("terminal journal outcome was rewritten", err)
					}
				})
			}
		})
	}
}
