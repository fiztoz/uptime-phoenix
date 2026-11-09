package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// T08: a target fails and recovers while the probe is cut off from the hub.
// The edge records the whole outage, opens and resolves the incident and pages
// directly. When the link returns, the hub ingests the backlog in one replay.
// History must keep the real times, and the hub must not send (or queue) any
// stale DOWN or recovery page of its own, neither on the first replay nor on a
// lost-ACK duplicate.
func TestProbeReplayOfflineOutageAndRecovery(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			// The configuration is active from a minute before base; the whole
			// outage lies in the past by the time the link returns.
			base := time.Now().UTC().Truncate(time.Minute).Add(-25 * time.Minute)
			r := newReplayFixtureAt(t, engine, base)
			if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets"); err != nil {
				t.Fatal(err)
			}
			downAt, upAt := base.Add(5*time.Minute), base.Add(15*time.Minute)
			const alertID = "66666666-6666-4666-8666-666666666666"

			var events []domain.ProbeReplayEvent
			seq := int64(0)
			next := func() int64 { seq++; return seq }
			observed := map[int64]time.Time{}
			for minute := 0; minute < 20; minute++ {
				at := base.Add(time.Duration(minute) * time.Minute)
				status := domain.StatusUp
				if !at.Before(downAt) && at.Before(upAt) {
					status = domain.StatusDown
				}
				s := next()
				e := replayObservationAt(r, s, 0, status)
				e.ObservedAt, e.Observation.ObservedAt = at, at
				if status == domain.StatusDown {
					e.Observation.DownCount = int(at.Sub(downAt)/time.Minute) + 1
				}
				observed[s] = at
				events = append(events, e)
				switch at {
				case downAt:
					events = append(events, offlineIncident(r, next(), alertID, 1, downAt, nil), offlineDelivery(r, next(), alertID, 1, "77777777-7777-4777-8777-777777777777", at.Add(2*time.Second)))
				case upAt:
					resolved := upAt
					events = append(events, offlineIncident(r, next(), alertID, 2, downAt, &resolved), offlineDelivery(r, next(), alertID, 2, "88888888-8888-4888-8888-888888888888", at.Add(2*time.Second)))
				}
			}
			b := r.batch(events...)
			result := r.ingest(t, b)
			if result.CommittedSeq != seq || result.AcceptedCount != seq || len(result.Rejected) != 0 {
				t.Fatalf("offline backlog not accepted whole: %+v", result)
			}

			// Real times: every observation keeps the time the edge measured it.
			var stored []struct {
				Seq        int64     `bun:"seq"`
				ObservedAt time.Time `bun:"observed_at"`
			}
			if err := r.f.db.NewRaw("SELECT seq, observed_at FROM probe_observations ORDER BY seq").Scan(t.Context(), &stored); err != nil {
				t.Fatal(err)
			}
			got := map[int64]time.Time{}
			for _, row := range stored {
				got[row.Seq] = row.ObservedAt.UTC()
			}
			if len(got) != len(observed) {
				t.Fatalf("observations stored %d, want %d", len(got), len(observed))
			}
			for s, want := range observed {
				if !got[s].Equal(want) {
					t.Fatalf("seq %d observed_at %s, want %s", s, got[s], want)
				}
			}
			var status string
			var started, resolved time.Time
			if err := r.f.db.NewRaw("SELECT status, started_at, resolved_at FROM probe_incidents WHERE source_alert_id = ?", alertID).Scan(t.Context(), &status, &started, &resolved); err != nil {
				t.Fatal(err)
			}
			if status != domain.AlertStatusResolved || !started.UTC().Equal(downAt) || !resolved.UTC().Equal(upAt) {
				t.Fatalf("incident mirror %s %s..%s, want resolved %s..%s", status, started, resolved, downAt, upAt)
			}

			// History is projected at the measured times, not at receipt time.
			drainHistory(t, r)
			for minute := 0; minute < 19; minute++ {
				at := base.Add(time.Duration(minute) * time.Minute)
				row := readHistoryAggregate(t, r, "1m", at)
				down := !at.Before(downAt) && at.Before(upAt)
				full := time.Minute.Microseconds()
				if (down && (row.DownUS != full || row.UpUS != 0)) || (!down && (row.UpUS != full || row.DownUS != 0)) {
					t.Fatalf("minute %d (%s): up=%dus down=%dus unknown=%dus", minute, at, row.UpUS, row.DownUS, row.UnknownUS)
				}
			}
			intervals, err := r.f.projections.ListHealthHistory(t.Context(), r.monitor, base, base.Add(19*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			var downFrom, downTo time.Time
			for _, interval := range intervals {
				if interval.Status != domain.StatusDown {
					continue
				}
				if downFrom.IsZero() {
					downFrom = interval.From
				}
				downTo = interval.To
			}
			if !downFrom.Equal(downAt) || !downTo.Equal(upAt) {
				t.Fatalf("overall DOWN %s..%s, want %s..%s: %+v", downFrom, downTo, downAt, upAt, intervals)
			}

			assertNoHubPages(t, r)
			// A lost ACK replays the same backlog: duplicates only, still no pages.
			again := r.ingest(t, b)
			if again.AcceptedCount != 0 || again.DuplicateCount != seq || len(again.Rejected) != 0 {
				t.Fatalf("duplicate replay: %+v", again)
			}
			assertNoHubPages(t, r)
			if n := replayCount(t, r.f, "probe_delivery_events"); n != 2 {
				t.Fatalf("edge page mirrors = %d, want 2", n)
			}
		})
	}
}

func offlineIncident(r replayFixture, seq int64, alertID string, version int64, startedAt time.Time, resolvedAt *time.Time) domain.ProbeReplayEvent {
	i := domain.RegionalIncident{SourceAlertID: alertID, Scope: domain.IncidentScopeRegional, MonitorID: r.monitor, ProbeID: r.session.ProbeID, AssignmentGeneration: 1, Status: domain.AlertStatusFiring, TransitionVersion: version, StartedAt: startedAt, ConfigRevision: 1, SubjectKind: domain.IncidentSubjectAvailability}
	observed := startedAt
	if resolvedAt != nil {
		i.Status, i.ResolvedAt, observed = domain.AlertStatusResolved, resolvedAt, *resolvedAt
	}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindAlertTransition, ObservedAt: observed, Incident: &i}
}

func offlineDelivery(r replayFixture, seq int64, alertID string, version int64, deliveryID string, at time.Time) domain.ProbeReplayEvent {
	d := domain.RegionalDelivery{DeliveryID: deliveryID, SourceAlertID: alertID, SourceTransitionVersion: version, ProbeID: r.session.ProbeID, NotificationID: r.channel, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, Attempt: 1, Status: domain.DeliveryStatusSent, ObservedAt: at}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindDeliveryResult, ObservedAt: at, Delivery: &d}
}

// assertNoHubPages checks every table through which the hub itself would page
// or queue a page: legacy alerts and escalations, hub-side regional delivery
// intents, and both aggregate paths.
func assertNoHubPages(t *testing.T, r replayFixture) {
	t.Helper()
	for _, table := range []string{"alerts", "alert_escalations", "probe_delivery_intents", "operative_aggregate_incidents", "operative_aggregate_intents", "aggregate_standby_incidents", "aggregate_standby_intents", "notification_sent_history"} {
		if n := replayCount(t, r.f, table); n != 0 {
			t.Fatalf("%s = %d after offline replay, want 0", table, n)
		}
	}
}
