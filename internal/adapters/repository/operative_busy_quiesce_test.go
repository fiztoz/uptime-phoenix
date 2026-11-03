package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestOperativeHubUnresolvedAttemptDefersBarrier covers the hub half of issue
// #191 for a monitor whose members are all remote. The coordinator re-checks
// its own provider journal after every member quiesced, and an ambiguous
// attempt there never resolves — e.g. the one migration 082 records for an
// earlier failed hub-local send, from before the monitor moved to remote
// probes. Opening a barrier then would hold every member's regional paging
// forever while aggregate paging could never start, and drain waits on the
// same journal. The barrier must stay pending instead, issuing no quiesce.
func TestOperativeHubUnresolvedAttemptDefersBarrier(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			o := newOperativeRemoteFixture(t, engine)
			if err := o.store.SetOperativeSourceCapability(ctx, o.f.session, true); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			if _, err := o.f.f.db.ExecContext(ctx, "INSERT INTO operative_provider_attempts (attempt_id,scope,monitor_id,probe_id,delivery_id,claim_attempt,notification_id,budget_until,state,created_at,finished_at) VALUES (?,'source',?,'local',?,1,0,?,'ambiguous',?,?)",
				uuid.NewString(), o.f.monitor, uuid.NewString(), at, at, at); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if got := o.run(t, true); got.Phase != "preparing" || got.BarrierID != "" {
					t.Fatalf("unresolvable hub attempt opened a barrier: %+v", got)
				}
			}
			if n := countKind(t, o.f.f.db, "probe_commands", domain.DeliveryQuiesceKind); n != 0 {
				t.Fatalf("quiesce issued to %d members for a barrier that can never quiesce", n)
			}
			if count, err := o.f.f.db.NewSelect().Table("operative_cutover_members").ColumnExpr("COUNT(*)").Count(ctx); err != nil || count != 0 {
				t.Fatal("deferred barrier recorded membership", err, count)
			}
		})
	}
}
