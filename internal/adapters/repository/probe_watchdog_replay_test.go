package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func newWatchdogReplayFixture(t *testing.T, engine string) replayFixture {
	t.Helper()
	r := newReplayFixture(t, engine)
	w := domain.DefaultProbeWatchdogSettings()
	w.Enabled, w.NotificationIDs = true, []int64{r.channel}
	if _, err := repository.NewProbeWatchdogSettingsStore(r.f.db).Replace(t.Context(), r.session.ProbeID, 0, w); err != nil {
		t.Fatal(err)
	}
	if m, err := r.syncer.RefreshRemote(t.Context(), syncTarget(), r.at.Add(-time.Minute)); err != nil || m.Revision != 2 {
		t.Fatal("prepare enabled watchdog", err)
	}
	return r
}

func watchdogReplayIncident(r replayFixture, seq, version int64) domain.ProbeReplayEvent {
	e := r.incident(seq, version)
	e.Kind = domain.ReplayKindWatchdogTransition
	i := e.Incident
	i.Scope, i.SubjectKind, i.MonitorID, i.AssignmentGeneration, i.ConfigRevision = domain.IncidentScopeProbeConnection, domain.IncidentSubjectWatchdog, 0, 0, 2
	return e
}

func watchdogReplayDelivery(r replayFixture, seq, version, attempt int64, status string) domain.ProbeReplayEvent {
	e := r.delivery(seq, version, attempt, status)
	e.Delivery.EventKind, e.Delivery.NotificationVersion = domain.DeliveryEventProbeConnection, 2
	return e
}

func TestWatchdogReplayStorage(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, run := range map[string]func(*testing.T, replayFixture){"ResendHistoryAndClockRollback": testWatchdogReplayHistory, "HubSourceOwnership": testWatchdogReplayOwner, "AtomicRollback": testWatchdogReplayRollback, "AdministrativeDisable": testWatchdogReplayDisable} {
				t.Run(name, func(t *testing.T) { run(t, newWatchdogReplayFixture(t, engine)) })
			}
		})
	}
}

func testWatchdogReplayHistory(t *testing.T, r replayFixture) {
	opening := watchdogReplayIncident(r, 1, 1)
	if v := r.ingest(t, r.batch(opening)); v.AcceptedCount != 1 {
		t.Fatal("opening rejected", v)
	}
	// A changed resend interval publishes revision 3, keeping the incident at
	// transition 1/config 2. No hub applied receipt is needed for lost-receipt history.
	settings := repository.NewProbeWatchdogSettingsStore(r.f.db)
	w, err := settings.Get(t.Context(), r.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	w.ResendInterval = 2
	if _, err := settings.Replace(t.Context(), r.session.ProbeID, w.Revision, w); err != nil {
		t.Fatal(err)
	}
	if m, err := r.syncer.RefreshRemote(t.Context(), syncTarget(), r.at); err != nil || m.Revision != 3 {
		t.Fatal("resend config", err)
	}
	retry := watchdogReplayDelivery(r, 2, 1, 1, domain.DeliveryStatusRetrying)
	retry.Delivery.NotificationVersion = 3
	sent := watchdogReplayDelivery(r, 3, 1, 2, domain.DeliveryStatusSent)
	sent.Delivery.NotificationVersion = 3
	sent.ObservedAt, sent.Delivery.ObservedAt = r.at.Add(-time.Minute), r.at.Add(-time.Minute)
	resolved := watchdogReplayIncident(r, 4, 2)
	resolved.ObservedAt, resolved.Incident.ResolvedAt, resolved.Incident.ConfigRevision = sent.ObservedAt, &sent.ObservedAt, 3
	up := watchdogReplayDelivery(r, 5, 2, 1, domain.DeliveryStatusSent)
	up.Delivery.NotificationVersion = 3
	up.ObservedAt, up.Delivery.ObservedAt = sent.ObservedAt.Add(-time.Second), sent.ObservedAt.Add(-time.Second)
	batch := r.batch(retry, sent, resolved, up)
	if v := r.ingest(t, batch); v.AcceptedCount != 4 || len(v.Rejected) != 0 {
		t.Fatal("valid watchdog history", v)
	}
	if v := r.ingest(t, batch); v.DuplicateCount != 4 || v.AcceptedCount != 0 {
		t.Fatal("duplicate rewrote history", v)
	}
	for table, want := range map[string]int{"probe_telemetry_receipts": 5, "probe_incidents": 1, "probe_delivery_events": 2, "probe_delivery_intents": 0, "probe_observations": 0, "alerts": 0, "monitor_probe_state": 0, "probe_hub_watchdog_incidents": 0, "probe_watchdog_events": 0} {
		if got := replayCount(t, r.f, table); got != want {
			t.Fatalf("%s=%d, want%d", table, got, want)
		}
	}
	nulls, err := r.f.db.NewSelect().Table("probe_telemetry_receipts").Where("kind=? AND monitor_id IS NULL AND assignment_generation IS NULL", domain.ReplayKindWatchdogTransition).Count(t.Context())
	if err != nil || nulls != 2 {
		t.Fatal("watchdog receipt fabricated monitor", nulls, err)
	}
	inc, err := repository.NewRegionalCommitStore(r.f.db).GetIncident(t.Context(), opening.Incident.SourceAlertID)
	if err != nil || inc.Status != domain.AlertStatusResolved || inc.TransitionVersion != 2 || inc.StartedAt.UnixMicro() != opening.Incident.StartedAt.UnixMicro() || inc.ResolvedAt.UnixMicro() != resolved.ObservedAt.UnixMicro() {
		t.Fatal("source lifecycle not mirrored", err)
	}
}

func testWatchdogReplayOwner(t *testing.T, r replayFixture) {
	e := watchdogReplayIncident(r, 1, 1)
	store := repository.NewRegionalCommitStore(r.f.db)
	owned := *e.Incident
	owned.Status, owned.TransitionVersion, owned.ResolvedAt = domain.AlertStatusResolved, 2, &r.at
	if err := store.PutIncident(t.Context(), &owned); err != nil {
		t.Fatal(err)
	}
	// The durable ownership registry covers every retained source UUID, even a
	// resolved incident no longer referenced by the current watchdog checkpoint.
	if _, err := r.f.db.ExecContext(t.Context(), "INSERT INTO probe_hub_watchdog_incidents(source_alert_id,probe_id,created_at) VALUES(?,?,?)", e.Incident.SourceAlertID, r.session.ProbeID, r.at); err != nil {
		t.Fatal(err)
	}
	d := watchdogReplayDelivery(r, 2, 1, 1, domain.DeliveryStatusSent)
	valid := watchdogReplayIncident(r, 3, 1)
	valid.Incident.SourceAlertID = "99999999-9999-4999-8999-999999999999"
	batch := r.batch(e, d, valid)
	v := r.ingest(t, batch)
	if v.CommittedSeq != 3 || v.AcceptedCount != 1 || len(v.Rejected) != 2 || v.Rejected[0].Code != "source_owner_conflict" || v.Rejected[1].Code != "source_owner_conflict" {
		t.Fatal("hub UUID forged or stream stalled", v)
	}
	if v := r.ingest(t, batch); v.DuplicateCount != 1 || len(v.Rejected) != 2 {
		t.Fatal("rejection receipt not stable", v)
	}
	if replayCount(t, r.f, "probe_delivery_events") != 0 || replayCount(t, r.f, "probe_delivery_intents") != 0 {
		t.Fatal("forged provider work")
	}
}

func testWatchdogReplayRollback(t *testing.T, r replayFixture) {
	trigger := "CREATE TRIGGER fail_watchdog_replay BEFORE UPDATE ON probe_streams BEGIN SELECT RAISE(ABORT,'cursor failure'); END"
	if r.f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_watchdog_replay BEFORE UPDATE ON probe_streams FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='cursor failure'"
	}
	if _, err := r.f.db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_watchdog_replay") })
	b := r.batch(watchdogReplayIncident(r, 1, 1), watchdogReplayDelivery(r, 2, 1, 1, domain.DeliveryStatusSent))
	if _, err := r.store.IngestReplayBatch(t.Context(), r.session, b, &services.AccessService{}); !errors.Is(err, domain.ErrInternal) {
		t.Fatal("cursor failure not returned", err)
	}
	for _, table := range []string{"probe_incidents", "probe_delivery_events", "probe_telemetry_receipts"} {
		if replayCount(t, r.f, table) != 0 {
			t.Fatal("partial watchdog commit", table)
		}
	}
	if cursor, err := r.store.GetCursor(t.Context(), r.session.ProbeID, r.session.StreamID); err != nil || cursor != 0 {
		t.Fatal("cursor advanced before commit", err)
	}
	if _, err := r.f.db.ExecContext(t.Context(), "DROP TRIGGER fail_watchdog_replay"); err != nil {
		t.Fatal(err)
	}
	if v := r.ingest(t, b); v.AcceptedCount != 2 {
		t.Fatal("retry did not recover", v)
	}
}

func testWatchdogReplayDisable(t *testing.T, r replayFixture) {
	if v := r.ingest(t, r.batch(watchdogReplayIncident(r, 1, 1))); v.AcceptedCount != 1 {
		t.Fatal(v)
	}
	store := repository.NewProbeWatchdogSettingsStore(r.f.db)
	w, err := store.Get(t.Context(), r.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	w.Enabled = false
	if _, err := store.Replace(t.Context(), r.session.ProbeID, w.Revision, w); err != nil {
		t.Fatal(err)
	}
	if m, err := r.syncer.RefreshRemote(t.Context(), syncTarget(), r.at); err != nil || m.Revision != 3 {
		t.Fatal(err)
	}
	end := watchdogReplayIncident(r, 2, 2)
	end.Incident.ConfigRevision = 3
	bad := watchdogReplayIncident(r, 3, 1)
	bad.Incident.SourceAlertID, bad.Incident.ConfigRevision = "99999999-9999-4999-8999-999999999999", 3
	v := r.ingest(t, r.batch(end, bad))
	if v.AcceptedCount != 1 || len(v.Rejected) != 1 || v.Rejected[0].Code != "watchdog_disabled" {
		t.Fatal("disabled graph accepted a new outage or rejected closure", v)
	}
}
