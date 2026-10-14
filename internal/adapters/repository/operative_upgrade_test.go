package repository_test

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestOperativeUpgradeContract verifies real migration and startup effects.
func TestOperativeUpgradeContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("LegacyAmbiguity", func(t *testing.T) {
				o := newOperativeFixture(t, engine, domain.AlertDeliveryRegional, 1, false)
				o.record(t, domain.StatusDown)
				claims, err := o.p.outbox.ClaimDeliveries(t.Context(), domain.LocalProbeID, time.Now().UTC(), 30*time.Second, 10)
				if err != nil || len(claims) != 1 {
					t.Fatal("legacy claim fixture", err, len(claims))
				}
				if err := runEngineMigration(t, o.p.f.db, engine, "082_operative_cutover", "down"); err != nil {
					t.Fatal(err)
				}
				if err := runEngineMigration(t, o.p.f.db, engine, "082_operative_cutover", "up"); err != nil {
					t.Fatal(err)
				}
				var state string
				if err := o.p.f.db.NewSelect().Table("operative_provider_attempts").Column("state").Where("delivery_id = ? AND scope = ?", claims[0].DeliveryID, "source").Scan(t.Context(), &state); err != nil || state != string(domain.ProviderAttemptAmbiguous) {
					t.Fatal("upgrade lost unknown I/O", err, state)
				}
				if err := repository.RequireSafeOperativeStartup(t.Context(), o.p.f.db, false, false); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("downgrade entered uninstrumented dispatch", err)
				}
				if err := repository.RequireSafeOperativeStartup(t.Context(), o.p.f.db, true, true); err != nil {
					t.Fatal("protected recovery runtime unavailable", err)
				}
				if err := runEngineMigration(t, o.p.f.db, engine, "082_operative_cutover", "down"); err == nil {
					t.Fatal("downgrade erased legacy ambiguity")
				}
			})
			t.Run("HeldRuntime", func(t *testing.T) {
				o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
				if err := repository.RequireSafeOperativeStartup(t.Context(), o.p.f.db, false, false); err != nil {
					t.Fatal("empty state broke default startup", err)
				}
				if got := o.run(t, true); got.Phase != "quiescing" {
					t.Fatal("fixture has no source hold", got)
				}
				for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}} {
					if err := repository.RequireSafeOperativeStartup(t.Context(), o.p.f.db, flags[0], flags[1]); !errors.Is(err, ports.ErrConflict) {
						t.Fatal("stranded operative authority permitted", flags, err)
					}
				}
				if err := repository.RequireSafeOperativeStartup(t.Context(), o.p.f.db, true, true); err != nil {
					t.Fatal(err)
				}
				if got := o.drain(t); got.Phase != "released" {
					t.Fatal("disable did not safely resume", got)
				}
				if err := repository.RequireSafeOperativeStartup(t.Context(), o.p.f.db, false, false); err != nil {
					t.Fatal("finished authority blocked default", err)
				}
			})
		})
	}
}
