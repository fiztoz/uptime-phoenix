package repository_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestOperativeHubUnsettledAttemptDefersBarrier covers the hub half of issue
// #191 for a monitor whose members are all remote. The coordinator re-checks
// its own provider journal after every member quiesced, so opening a barrier
// while a hub send may still run would hold every member's regional paging
// meanwhile. The barrier stays pending instead, issuing no quiesce, until that
// send settles. An ambiguous attempt (a past provider error) never resolves and
// must not defer it forever (#211).
func TestOperativeHubUnsettledAttemptDefersBarrier(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, state := range []domain.ProviderAttemptState{domain.ProviderAttemptInFlight, domain.ProviderAttemptAmbiguous} {
				t.Run(string(state), func(t *testing.T) { testHubAttemptDefersBarrier(t, engine, state) })
			}
		})
	}
}

func testHubAttemptDefersBarrier(t *testing.T, engine string, state domain.ProviderAttemptState) {
	ctx := t.Context()
	o := newOperativeRemoteFixture(t, engine)
	if err := o.store.SetOperativeSourceCapability(ctx, o.f.session, true); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	var finished *time.Time // An in_flight grant has not finished.
	if state != domain.ProviderAttemptInFlight {
		finished = &at
	}
	if _, err := o.f.f.db.ExecContext(ctx, "INSERT INTO operative_provider_attempts (attempt_id,scope,monitor_id,probe_id,delivery_id,claim_attempt,notification_id,budget_until,state,created_at,finished_at) VALUES (?,'source',?,'local',?,1,0,?,?,?,?)",
		uuid.NewString(), o.f.monitor, uuid.NewString(), at.Add(10*time.Second), state, at, finished); err != nil {
		t.Fatal(err)
	}
	if state == domain.ProviderAttemptAmbiguous {
		if got := o.run(t, true); got.Phase != "quiescing" {
			t.Fatalf("a past ambiguous send deferred the barrier: %+v", got)
		}
		return
	}
	for i := 0; i < 2; i++ {
		if got := o.run(t, true); got.Phase != "preparing" || got.BarrierID != "" {
			t.Fatalf("a hub send still in its budget opened a barrier: %+v", got)
		}
	}
	if n := countKind(t, o.f.f.db, "probe_commands", domain.DeliveryQuiesceKind); n != 0 {
		t.Fatalf("quiesce issued to %d members while a hub send may still run", n)
	}
	if count, err := o.f.f.db.NewSelect().Table("operative_cutover_members").ColumnExpr("COUNT(*)").Count(ctx); err != nil || count != 0 {
		t.Fatal("deferred barrier recorded membership", err, count)
	}
	// The send never completes and its budget passes: it crashed.
	if _, err := o.f.f.db.ExecContext(ctx, "UPDATE operative_provider_attempts SET budget_until = ?", at.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("an expired in_flight send deferred the barrier forever: %+v", got)
	}
}
