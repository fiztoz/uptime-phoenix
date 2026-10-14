package repository_test

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestOperativeActivationCycle keeps immutable application history while a
// deliberate disable/drain/re-enable applies the same acknowledged document.
func TestOperativeActivationCycle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) {
					o := newOperativeFixture(t, engine, mode, 1, true)
					for _, phase := range []string{"quiescing", "quiesced", "applying", "active"} {
						if got := o.run(t, true); got.Phase != phase {
							t.Fatal("first activation", phase, got)
						}
					}
					first := o.state(t)
					if got := o.drain(t); got.Phase != "released" {
						t.Fatal("first drain", got)
					}
					for _, phase := range []string{"preparing", "quiescing", "quiesced", "applying", "active"} {
						if got := o.run(t, true); got.Phase != phase {
							t.Fatal("re-enable", phase, got)
						}
					}
					second := o.state(t)
					if first.BarrierID == nil || second.BarrierID == nil || *first.BarrierID == *second.BarrierID {
						t.Fatal("re-enable reused an old barrier")
					}
					var count int
					if err := o.p.f.db.NewSelect().Table("operative_local_applications").ColumnExpr("COUNT(*)").Where("monitor_id = ?", o.p.monitor.ID).Scan(t.Context(), &count); err != nil || count != 2 {
						t.Fatal("applications were rewritten instead of retained", err, count)
					}
				})
			}
		})
	}
}
