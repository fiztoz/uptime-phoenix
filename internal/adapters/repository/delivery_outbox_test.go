package repository_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestDeliveryOutboxContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"AtomicRecording":      testOutboxAtomicRecording,
				"LeaseRestartAndRetry": testOutboxLeaseRestart,
				"CompletionRollback":   testOutboxCompletionRollback,
				"ConcurrentClaims":     testOutboxConcurrentClaims,
				"RegionalScope":        testOutboxRegionalScope,
				"SnapshotAndMigration": testOutboxSnapshotMigration,
				"Validation":           testOutboxValidation,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func outbox(f probeRegistryFixture) ports.DeliveryOutboxRepository {
	return f.localHeartbeat.(ports.DeliveryOutboxRepository)
}

func queuedLocalCommit(id int64) domain.LocalHeartbeatCommit {
	c := localCommit(id, 0)
	c.Heartbeat.Status, c.RawStatus, c.Heartbeat.DownCount = domain.StatusDown, domain.StatusDown, 1
	c.Heartbeat.Msg = "connection refused"
	c.Heartbeat.ConfigRevision = 3
	c.Incident = availabilityIncident(incidentLocalID, id, domain.LocalProbeID, c.Heartbeat.Time)
	c.Incident.ConfigRevision = c.Heartbeat.ConfigRevision
	c.DeliveryIntents = []domain.DeliveryIntent{intentFor(c.Incident, deliveryLocalID, c.Heartbeat.Time)}
	return c
}

func intentFor(inc *domain.RegionalIncident, id string, at time.Time) domain.DeliveryIntent {
	return domain.DeliveryIntent{DeliveryID: id, SourceAlertID: inc.SourceAlertID,
		SourceTransitionVersion: inc.TransitionVersion, ProbeID: inc.ProbeID,
		NotificationID: 17, NotificationVersion: inc.ConfigRevision, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at}
}

func receipt(work domain.QueuedDelivery) domain.DeliveryClaim {
	return domain.DeliveryClaim{DeliveryID: work.DeliveryID, ProbeID: work.ProbeID, Attempt: work.Attempt, LeaseToken: work.LeaseToken}
}

func assertTableCount(t *testing.T, f probeRegistryFixture, table string, want int) {
	t.Helper()
	var n int
	if err := f.db.NewSelect().Table(table).ColumnExpr("COUNT(*)").Scan(context.Background(), &n); err != nil || n != want {
		t.Fatalf("%s count = %d, want %d: %v", table, n, want, err)
	}
}

func injectOutboxFailure(t *testing.T, f probeRegistryFixture, table, operation string) func() {
	t.Helper()
	statement := fmt.Sprintf("CREATE TRIGGER fail_outbox_write BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'injected outbox failure'); END", operation, table)
	if f.engine == "mariadb" {
		statement = fmt.Sprintf("CREATE TRIGGER fail_outbox_write BEFORE %s ON %s FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected outbox failure'", operation, table)
	}
	if _, err := f.db.ExecContext(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := f.db.ExecContext(context.Background(), "DROP TRIGGER fail_outbox_write"); err != nil {
			t.Fatal(err)
		}
	}
}

func testOutboxAtomicRecording(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"probe_incidents", "probe_delivery_intents"} {
		clear := injectOutboxFailure(t, f, table, "INSERT")
		c := queuedLocalCommit(id)
		hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c)
		clear()
		if err == nil || hb != nil || c.Incident.HubIncidentID != 0 || localSequence(t, f) != 0 {
			t.Fatalf("%s fault leaked result/sequence: %+v %+v %v", table, hb, c.Incident, err)
		}
		for _, table := range []string{"heartbeats", "probe_observations", "monitor_probe_state", "probe_dirty_buckets", "probe_incidents", "probe_delivery_intents"} {
			assertTableCount(t, f, table, 0)
		}
	}
	// A malformed second channel must roll back the first intent too.
	c := queuedLocalCommit(id)
	c.DeliveryIntents = append(c.DeliveryIntents, intentFor(c.Incident, incidentRemoteID, c.Heartbeat.Time))
	c.DeliveryIntents[1].NotificationVersion = 0
	if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); !errors.Is(err, domain.ErrValidation) || hb != nil || c.Incident.HubIncidentID != 0 {
		t.Fatalf("partial channel list accepted: %+v %v", hb, err)
	}
	assertTableCount(t, f, "probe_delivery_intents", 0)
	assertTableCount(t, f, "probe_incidents", 0)
	if localSequence(t, f) != 0 {
		t.Fatal("invalid intent consumed sequence")
	}
	c = queuedLocalCommit(id)
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c)
	if err != nil || hb.SourceSeq != 1 || c.Incident.HubIncidentID == 0 {
		t.Fatalf("successful atomic record: %+v %v", hb, err)
	}
	work, err := outbox(f).GetDeliveryIntent(ctx, domain.LocalProbeID, deliveryLocalID)
	if err != nil || work.SourceSeq != hb.SourceSeq || work.StreamID != hb.StreamID || work.Status != domain.DeliveryStatusPending || work.Attempt != 0 ||
		work.CheckOutput != c.Heartbeat.Msg || work.MonitorID != id || work.AssignmentGeneration != 1 || work.NotificationVersion != 3 ||
		!work.ObservedAt.Equal(c.Heartbeat.Time) || work.ObservedAt.Location() != time.UTC || work.AvailableAt.Location() != time.UTC {
		t.Fatalf("queued context mismatch: %+v %v", work, err)
	}
	assertTableCount(t, f, "probe_delivery_events", 0)
	// Retrying an uncertain recording cannot append duplicate work or alter it.
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); !errors.Is(err, ports.ErrStaleLocalState) {
		t.Fatalf("duplicate check: %v", err)
	}
	assertTableCount(t, f, "probe_delivery_intents", 1)
	assertTableCount(t, f, "heartbeats", 1)
}

func testOutboxLeaseRestart(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	c := queuedLocalCommit(localMonitor(t, f))
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); err != nil {
		t.Fatal(err)
	}
	store := outbox(f)
	at := c.Heartbeat.Time.UTC()
	if rows, err := store.ClaimDeliveries(ctx, "local", at.Add(-time.Microsecond), time.Minute, 10); err != nil || len(rows) != 0 {
		t.Fatalf("claimed before due: %+v %v", rows, err)
	}
	first := claimOne(t, store, at)
	if first.Attempt != 1 || first.LeaseUntil == nil || !first.LeaseUntil.Equal(at.Add(time.Minute)) {
		t.Fatalf("first lease: %+v", first)
	}
	if rows, err := store.ClaimDeliveries(ctx, "local", at.Add(time.Second), time.Minute, 1); err != nil || len(rows) != 0 {
		t.Fatalf("live lease stolen: %+v %v", rows, err)
	}
	for _, invalidAt := range []time.Time{at.Add(-time.Microsecond), at.Add(time.Minute)} {
		if err := store.FinishDelivery(ctx, receipt(first), domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: invalidAt}); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("out-of-lease completion: %v", err)
		}
	}
	// A separate connection represents process restart after claim, before I/O.
	peer := peerLocalRecorder(t, f).(ports.DeliveryOutboxRepository)
	second := claimOne(t, peer, at.Add(time.Minute))
	if second.Attempt != 2 || second.LeaseToken == first.LeaseToken {
		t.Fatalf("reclaim did not fence old worker: %+v", second)
	}
	if err := store.FinishDelivery(ctx, receipt(first), domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: at.Add(time.Second)}); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale worker completed newer attempt: %v", err)
	}
	retry := domain.DeliveryResult{Status: domain.DeliveryStatusRetrying, ErrorCode: "provider_unavailable", At: at.Add(61*time.Second + 123456789*time.Nanosecond), RetryAt: at.Add(2 * time.Minute)}
	for range 2 {
		if err := peer.FinishDelivery(ctx, receipt(second), retry); err != nil {
			t.Fatalf("retry result/idempotent receipt: %v", err)
		}
	}
	conflict := retry
	conflict.ErrorCode = "another_error"
	if err := peer.FinishDelivery(ctx, receipt(second), conflict); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("conflicting completion accepted: %v", err)
	}
	if rows, err := store.ClaimDeliveries(ctx, "local", retry.RetryAt.Add(-time.Microsecond), time.Minute, 1); err != nil || len(rows) != 0 {
		t.Fatalf("retry ran too early: %+v %v", rows, err)
	}
	third := claimOne(t, store, retry.RetryAt)
	if third.Attempt != 3 || third.DeliveryID != first.DeliveryID {
		t.Fatalf("retry lost stable identity: %+v", third)
	}
	result := domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: at.Add(121 * time.Second)}
	for range 2 {
		if err := store.FinishDelivery(ctx, receipt(third), result); err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := store.ClaimDeliveries(ctx, "local", at.Add(time.Hour), time.Minute, 10); err != nil || len(rows) != 0 {
		t.Fatalf("terminal delivery reclaimed: %+v %v", rows, err)
	}
	got, err := f.deliveries.GetDelivery(ctx, deliveryLocalID)
	if err != nil || got.Status != domain.DeliveryStatusSent || got.Attempt != 3 || got.NotificationID != 17 || got.NotificationVersion != 3 || got.SourceTransitionVersion != 1 {
		t.Fatalf("outcome identity: %+v %v", got, err)
	}
	got.Status, got.ErrorCode, got.Attempt = domain.DeliveryStatusFailed, "forged_result", 4
	if err := f.deliveries.PutDelivery(ctx, got); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("mirror API overwrote source-owned result: %v", err)
	}
}

func claimOne(t *testing.T, store ports.DeliveryOutboxRepository, at time.Time) domain.QueuedDelivery {
	t.Helper()
	rows, err := store.ClaimDeliveries(context.Background(), domain.LocalProbeID, at, time.Minute, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim one: %+v %v", rows, err)
	}
	return rows[0]
}

func testOutboxCompletionRollback(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	c := queuedLocalCommit(localMonitor(t, f))
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); err != nil {
		t.Fatal(err)
	}
	work := claimOne(t, outbox(f), c.Heartbeat.Time)
	result := domain.DeliveryResult{Status: domain.DeliveryStatusFailed, ErrorCode: "invalid_configuration", At: c.Heartbeat.Time.Add(time.Second)}
	for table, operation := range map[string]string{"probe_delivery_events": "INSERT", "probe_delivery_intents": "UPDATE"} {
		clear := injectOutboxFailure(t, f, table, operation)
		err := outbox(f).FinishDelivery(ctx, receipt(work), result)
		clear()
		if err == nil {
			t.Fatalf("%s fault returned success", table)
		}
		got, err := outbox(f).GetDeliveryIntent(ctx, "local", work.DeliveryID)
		if err != nil || !reflect.DeepEqual(got, &work) {
			t.Fatalf("%s fault partially changed queue: %+v %v", table, got, err)
		}
		assertTableCount(t, f, "probe_delivery_events", 0)
	}
	if err := outbox(f).FinishDelivery(ctx, receipt(work), result); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, f, "probe_delivery_events", 1)
}

func testOutboxConcurrentClaims(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	c := queuedLocalCommit(localMonitor(t, f))
	c.DeliveryIntents = nil
	for i := range 16 {
		intent := intentFor(c.Incident, fmt.Sprintf("abcddcba-1234-4567-8901-%012d", i), c.Heartbeat.Time)
		intent.NotificationID = int64(i + 1)
		c.DeliveryIntents = append(c.DeliveryIntents, intent)
	}
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); err != nil {
		t.Fatal(err)
	}
	stores := []ports.DeliveryOutboxRepository{outbox(f), peerLocalRecorder(t, f).(ports.DeliveryOutboxRepository)}
	start := make(chan struct{})
	results := make(chan []domain.QueuedDelivery, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			<-start
			rows, err := stores[i%2].ClaimDeliveries(ctx, "local", c.Heartbeat.Time, time.Minute, 2)
			results <- rows
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[string]bool)
	for batch := range results {
		for i, work := range batch {
			if seen[work.DeliveryID] || work.Attempt != 1 || (i > 0 && work.DeliveryID <= batch[i-1].DeliveryID) {
				t.Fatalf("duplicate/unordered claim: %+v", batch)
			}
			seen[work.DeliveryID] = true
		}
	}
	if len(seen) != 16 {
		t.Fatalf("claimed %d of 16 due intents", len(seen))
	}
}

func testOutboxRegionalScope(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	f.remote(t, probeRegistryID1, "asia")
	if _, err := f.assignments.Replace(ctx, id, 1, []string{"local", probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	c := queuedLocalCommit(id)
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); err != nil {
		t.Fatal(err)
	}
	obs := regionalSample(id, probeRegistryID1, remoteStreamID, 1, 1, domain.StatusDown, 1, c.Heartbeat.Time)
	inc := availabilityIncident(incidentRemoteID, id, probeRegistryID1, obs.ObservedAt)
	remoteDeliveryID := "fe09a765-1234-4321-8901-987654321000"
	remote := domain.RegionalCommit{Observation: obs, State: stateFrom(obs), Incident: inc, DeliveryIntents: []domain.DeliveryIntent{intentFor(inc, remoteDeliveryID, obs.ObservedAt)}}
	// Fail after observation/state writes in the explicitly sequenced path too.
	remote.DeliveryIntents[0].ProbeID = "local"
	if err := f.commits.Commit(ctx, remote); !errors.Is(err, ports.ErrConflict) || inc.HubIncidentID != 0 {
		t.Fatalf("remote identity mismatch: %v", err)
	}
	if _, err := f.commits.GetState(ctx, id, probeRegistryID1); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("failed remote commit retained state: %v", err)
	}
	remote.DeliveryIntents[0].ProbeID = probeRegistryID1
	if err := f.commits.Commit(ctx, remote); err != nil {
		t.Fatal(err)
	}
	local := claimOne(t, outbox(f), obs.ObservedAt)
	if local.ProbeID != "local" || local.DeliveryID != deliveryLocalID {
		t.Fatalf("local claimed remote: %+v", local)
	}
	if _, err := outbox(f).GetDeliveryIntent(ctx, "local", remoteDeliveryID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("cross-probe read: %v", err)
	}
	wrongProbe := receipt(local)
	wrongProbe.ProbeID = probeRegistryID1
	if err := outbox(f).FinishDelivery(ctx, wrongProbe, domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: obs.ObservedAt}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("cross-probe completion: %v", err)
	}
	if _, err := f.assignments.Replace(ctx, id, 2, []string{"local"}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(ctx, id, 3, []string{"local", probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	rows, err := outbox(f).ClaimDeliveries(ctx, probeRegistryID1, obs.ObservedAt.Add(time.Hour), time.Minute, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("new generation inherited old work: %+v %v", rows, err)
	}
	// Telemetry/mirror APIs never create source work.
	if _, err := f.ingest.Ingest(ctx, domain.ProbeIngestBatch{ProbeID: probeRegistryID1, StreamID: probeRegistryID3, FromSeq: 1, ThroughSeq: 1,
		Events: []domain.RegionalObservation{regionalSample(id, probeRegistryID1, probeRegistryID3, 2, 1, domain.StatusUp, 0, obs.ObservedAt)}}); err != nil {
		t.Fatal(err)
	}
	mirrored := domain.RegionalDelivery{DeliveryID: ackCommandID, SourceAlertID: incidentRemoteID, SourceTransitionVersion: 1, ProbeID: probeRegistryID1,
		NotificationID: 17, NotificationVersion: 3, EventKind: domain.DeliveryEventStatusChange, Attempt: 1, Status: domain.DeliveryStatusSent, ObservedAt: obs.ObservedAt}
	if err := f.deliveries.PutDelivery(ctx, &mirrored); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, f, "probe_delivery_intents", 2)
}

func testOutboxSnapshotMigration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	for range 2 {
		if err := runEngineMigration(t, f.db, f.engine, "045_probe_delivery_outbox", "down"); err != nil {
			t.Fatal(err)
		}
		if err := runEngineMigration(t, f.db, f.engine, "045_probe_delivery_outbox", "up"); err != nil {
			t.Fatal(err)
		}
		if err := runEngineMigration(t, f.db, f.engine, "050_delivery_cancellation", "up"); err != nil {
			t.Fatal(err)
		}
		if err := runEngineMigration(t, f.db, f.engine, "051_escalation_delivery_context", "up"); err != nil {
			t.Fatal(err)
		}
	}
	c := queuedLocalCommit(id)
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, f.db, f.engine, "045_probe_delivery_outbox", "down"); err == nil {
		t.Fatal("downgrade discarded pending work")
	}
	resolved := *c.Incident
	resolved.Status, resolved.TransitionVersion = domain.AlertStatusResolved, 2
	resolvedAt := c.Heartbeat.Time.Add(time.Minute)
	resolved.ResolvedAt = &resolvedAt
	up := localCommit(id, hb.SourceSeq)
	up.Heartbeat.ConfigRevision = resolved.ConfigRevision
	up.Heartbeat.Time, up.Heartbeat.ReceivedAt = resolvedAt, resolvedAt
	up.Incident = &resolved
	up.DeliveryIntents = []domain.DeliveryIntent{intentFor(&resolved, incidentRemoteID, resolvedAt)}
	up.DeliveryIntents[0].EventKind = domain.DeliveryEventIncidentSummary
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, up); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_observations"); err != nil {
		t.Fatal(err)
	}
	original, err := outbox(f).GetDeliveryIntent(ctx, "local", deliveryLocalID)
	if err != nil || original.IncidentStatus != domain.AlertStatusFiring || original.ResolvedAt != nil || original.SourceSeq != 1 || original.CheckOutput != c.Heartbeat.Msg {
		t.Fatalf("incident/history mutation rewrote queued context: %+v %v", original, err)
	}
	summary, err := outbox(f).GetDeliveryIntent(ctx, "local", incidentRemoteID)
	if err != nil || summary.IncidentStatus != domain.AlertStatusResolved || summary.ResolvedAt == nil || !summary.ResolvedAt.Equal(resolvedAt) || summary.SourceTransitionVersion != 2 {
		t.Fatalf("summary lost outage timing: %+v %v", summary, err)
	}
	work := claimOne(t, outbox(f), resolvedAt)
	if err := outbox(f).FinishDelivery(ctx, receipt(work), domain.DeliveryResult{Status: domain.DeliveryStatusSuperseded, At: resolvedAt}); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, f.db, f.engine, "045_probe_delivery_outbox", "down"); err == nil {
		t.Fatal("downgrade discarded source receipts")
	}
	for _, statement := range []string{
		"UPDATE probe_delivery_intents SET attempt = -1",
		"UPDATE probe_delivery_intents SET attempt = 9223372036854775808",
		"UPDATE probe_delivery_intents SET status = 'leased', lease_until = NULL",
		"UPDATE probe_delivery_intents SET notification_version = 0",
	} {
		if _, err := f.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %s", statement)
		}
	}
	if err := newEngineMonitorRepo(f).Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, f, "probe_delivery_intents", 0)
	assertTableCount(t, f, "probe_delivery_events", 0)
	if err := runEngineMigration(t, f.db, f.engine, "045_probe_delivery_outbox", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, f.db, f.engine, "045_probe_delivery_outbox", "up"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, f.db, f.engine, "050_delivery_cancellation", "up"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, f.db, f.engine, "051_escalation_delivery_context", "up"); err != nil {
		t.Fatal(err)
	}
}

func testOutboxValidation(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	for name, mutate := range map[string]func(*domain.LocalHeartbeatCommit){
		"missing incident": func(c *domain.LocalHeartbeatCommit) { c.Incident = nil },
		"wrong incident":   func(c *domain.LocalHeartbeatCommit) { c.DeliveryIntents[0].SourceAlertID = incidentRemoteID },
		"wrong transition": func(c *domain.LocalHeartbeatCommit) { c.DeliveryIntents[0].SourceTransitionVersion++ },
		"wrong config":     func(c *domain.LocalHeartbeatCommit) { c.DeliveryIntents[0].NotificationVersion++ },
		"invalid ID":       func(c *domain.LocalHeartbeatCommit) { c.DeliveryIntents[0].DeliveryID = "bad" },
		"future summary": func(c *domain.LocalHeartbeatCommit) {
			c.DeliveryIntents[0].EventKind = domain.DeliveryEventIncidentSummary
		},
		"pending check": func(c *domain.LocalHeartbeatCommit) { c.Heartbeat.Status = domain.StatusPending },
		"duplicate ID": func(c *domain.LocalHeartbeatCommit) {
			c.DeliveryIntents = append(c.DeliveryIntents, c.DeliveryIntents[0])
		},
	} {
		c := queuedLocalCommit(id)
		mutate(&c)
		if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); err == nil || hb != nil {
			t.Fatalf("accepted %s: %+v %v", name, hb, err)
		}
	}
	assertTableCount(t, f, "heartbeats", 0)
	assertTableCount(t, f, "probe_delivery_intents", 0)
	c := queuedLocalCommit(id)
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, c); err != nil {
		t.Fatal(err)
	}
	work := claimOne(t, outbox(f), c.Heartbeat.Time)
	for _, result := range []domain.DeliveryResult{
		{Status: domain.DeliveryStatusSent},
		{Status: domain.DeliveryStatusPending, At: c.Heartbeat.Time},
		{Status: domain.DeliveryStatusSent, ErrorCode: "unexpected", At: c.Heartbeat.Time},
		{Status: domain.DeliveryStatusFailed, ErrorCode: "secret https://provider/token", At: c.Heartbeat.Time},
		{Status: domain.DeliveryStatusRetrying, ErrorCode: "timeout", At: c.Heartbeat.Time, RetryAt: c.Heartbeat.Time},
		{Status: domain.DeliveryStatusRetrying, ErrorCode: "timeout", At: c.Heartbeat.Time, RetryAt: c.Heartbeat.Time.Add(time.Nanosecond)},
		{Status: domain.DeliveryStatusFailed, ErrorCode: "timeout", At: c.Heartbeat.Time, RetryAt: c.Heartbeat.Time.Add(time.Second)},
	} {
		if err := outbox(f).FinishDelivery(ctx, receipt(work), result); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid result accepted: %+v %v", result, err)
		}
	}
	for _, limit := range []int{0, 101} {
		if _, err := outbox(f).ClaimDeliveries(ctx, "local", c.Heartbeat.Time, time.Minute, limit); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid claim bound: %v", err)
		}
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE probe_delivery_intents SET attempt = ?", int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if rows, err := outbox(f).ClaimDeliveries(ctx, "local", c.Heartbeat.Time.Add(time.Hour), time.Minute, 10); err != nil || len(rows) != 0 {
		t.Fatalf("attempt overflow claimed: %+v %v", rows, err)
	}
}
