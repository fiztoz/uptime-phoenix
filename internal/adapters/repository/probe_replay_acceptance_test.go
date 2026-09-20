package repository_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type replayFixture struct {
	f                probeRegistryFixture
	store            *repository.ProbeReplayStore
	syncer           *repository.RemoteProbeConfigSyncStore
	session          domain.ProbeReplaySession
	monitor, channel int64
	at               time.Time
}

func newReplayFixture(t *testing.T, engine string) replayFixture {
	t.Helper()
	f := newProbeRegistryFixture(t, engine)
	syncer, protector, monitor, channel := remoteSyncFixture(t, f)
	at := time.Now().UTC().Truncate(time.Microsecond).Add(-5 * time.Second)
	if _, err := f.db.ExecContext(t.Context(), "UPDATE monitor_probe_assignment_history SET started_at = ?", at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.RefreshRemote(t.Context(), syncTarget(), at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	connections := repository.NewProbeConnectorStore(f.db)
	metadata := domain.ProbeCredentialMetadata{HubID: probeRegistryID2, ProbeID: probeRegistryID1, StreamID: "44444444-4444-4444-8444-444444444444", EnrollmentID: "55555555-5555-4555-8555-555555555555", CredentialVersion: 1, Endpoint: "wss://edge.example/ws/probe/v1", Fingerprint: strings.Repeat("a", 64)}
	if _, err := connections.PrepareConnection(t.Context(), domain.ProbeConnection{ProbeCredentialMetadata: metadata, ProtectedCredential: make([]byte, 40), State: "prepared", PreparedAt: at}); err != nil {
		t.Fatal(err)
	}
	lease, err := connections.AcquireConnector(t.Context(), probeRegistryID1, probeRegistryID3)
	if err != nil {
		t.Fatal(err)
	}
	return replayFixture{f: f, store: repository.NewProbeReplayStore(f.db, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector), syncer: syncer, monitor: monitor, channel: channel, at: at, session: domain.ProbeReplaySession{HubID: metadata.HubID, ProbeID: metadata.ProbeID, StreamID: metadata.StreamID, OwnerID: lease.OwnerID, ConnectionGeneration: lease.Generation}}
}
func (r replayFixture) observation(seq int64) domain.ProbeReplayEvent {
	o := domain.RegionalObservation{MonitorID: r.monitor, ProbeID: r.session.ProbeID, StreamID: r.session.StreamID, Seq: seq, AssignmentGeneration: 1, ConfigRevision: 1, Status: domain.StatusUp, RawStatus: domain.StatusUp, ObservedAt: r.at}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindObservation, ObservedAt: r.at, Observation: &o}
}
func (r replayFixture) incident(seq, version int64) domain.ProbeReplayEvent {
	i := domain.RegionalIncident{SourceAlertID: "66666666-6666-4666-8666-666666666666", Scope: domain.IncidentScopeRegional, MonitorID: r.monitor, ProbeID: r.session.ProbeID, AssignmentGeneration: 1, Status: domain.AlertStatusFiring, TransitionVersion: version, StartedAt: r.at.Add(-time.Second), ConfigRevision: 1, SubjectKind: domain.IncidentSubjectAvailability}
	if version == 2 {
		at := r.at
		i.Status = domain.AlertStatusResolved
		i.ResolvedAt = &at
	}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindAlertTransition, ObservedAt: r.at, Incident: &i}
}
func (r replayFixture) delivery(seq, version, attempt int64, status string) domain.ProbeReplayEvent {
	id := "77777777-7777-4777-8777-777777777777"
	if version == 2 {
		id = "88888888-8888-4888-8888-888888888888"
	}
	d := domain.RegionalDelivery{DeliveryID: id, SourceAlertID: "66666666-6666-4666-8666-666666666666", SourceTransitionVersion: version, ProbeID: r.session.ProbeID, NotificationID: r.channel, NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, Attempt: attempt, Status: status, ObservedAt: r.at}
	if status == domain.DeliveryStatusRetrying {
		d.ErrorCode = "provider_unavailable"
	}
	return domain.ProbeReplayEvent{Seq: seq, Kind: domain.ReplayKindDeliveryResult, ObservedAt: r.at, Delivery: &d}
}
func (r replayFixture) batch(events ...domain.ProbeReplayEvent) domain.ProbeReplayBatch {
	for index := range events {
		sum := sha256.Sum256([]byte(fmt.Sprintf("fixture-event-%d-%s", events[index].Seq, events[index].Kind)))
		events[index].Digest = hex.EncodeToString(sum[:])
	}
	return domain.ProbeReplayBatch{ProbeID: r.session.ProbeID, StreamID: r.session.StreamID, FirstSeq: events[0].Seq, LastSeq: events[len(events)-1].Seq, Events: events}
}
func (r replayFixture) ingest(t *testing.T, b domain.ProbeReplayBatch) *domain.ProbeReplayResult {
	t.Helper()
	result, err := r.store.IngestReplayBatch(t.Context(), r.session, b, &services.AccessService{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func replayCount(t *testing.T, f probeRegistryFixture, table string) int {
	t.Helper()
	n, err := f.db.NewSelect().Table(table).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestProbeReplayAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, replayFixture){"MixedDurableOutcomes": testReplayMixed, "LateRollback": testReplayRollback, "LeaseAndStreamFences": testReplayFences, "HistoricalAuthority": testReplayHistory, "ExactConfigAndParent": testReplayAuthority, "TimestampPrecision": testReplayPrecision, "ConcurrentDuplicate": testReplayConcurrent, "LeaseExpiresDuringBatch": testReplayExpiry} {
				t.Run(name, func(t *testing.T) { test(t, newReplayFixture(t, engine)) })
			}
		})
	}
}
func testReplayMixed(t *testing.T, r replayFixture) {
	badChannel := r.delivery(7, 2, 1, domain.DeliveryStatusSent)
	badChannel.Delivery.NotificationID++
	wrongConfig := r.observation(9)
	wrongConfig.Observation.ConfigRevision = 99
	b := r.batch(r.observation(1), r.incident(2, 1), r.delivery(3, 1, 1, domain.DeliveryStatusRetrying), r.delivery(4, 1, 2, domain.DeliveryStatusSent), r.incident(5, 2), r.delivery(6, 2, 1, domain.DeliveryStatusSent), badChannel, domain.ProbeReplayEvent{Seq: 8, Kind: "condition.transition", ObservedAt: r.at}, wrongConfig, r.observation(10))
	result := r.ingest(t, b)
	if result.CommittedSeq != 10 || result.AcceptedCount != 7 || len(result.Rejected) != 3 {
		t.Fatalf("mixed: %+v", result)
	}
	if result.Rejected[0].Code != "channel_unauthorized" || result.Rejected[1].Code != "unsupported_event" || result.Rejected[2].Code != "config_revision_mismatch" {
		t.Fatalf("rejections: %+v", result.Rejected)
	}
	for table, want := range map[string]int{"probe_telemetry_receipts": 10, "probe_observations": 2, "probe_incidents": 1, "probe_delivery_events": 2, "alerts": 0, "alert_escalations": 0, "probe_delivery_intents": 0} {
		if got := replayCount(t, r.f, table); got != want {
			t.Fatalf("%s=%d want%d", table, got, want)
		}
	}
	var updated time.Time
	if err := r.f.db.NewRaw("SELECT updated_at FROM probe_incidents").Scan(t.Context(), &updated); err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}
	duplicate := r.ingest(t, b)
	if duplicate.AcceptedCount != 0 || duplicate.DuplicateCount != 7 || len(duplicate.Rejected) != 3 || replayCount(t, r.f, "probe_dirty_buckets") != 0 {
		t.Fatalf("retry rewrote evidence %+v", duplicate)
	}
	var again time.Time
	if err := r.f.db.NewRaw("SELECT updated_at FROM probe_incidents").Scan(t.Context(), &again); err != nil || !again.Equal(updated) {
		t.Fatal("duplicate rewrote mirror timestamp", err)
	}
	b.Events[0].Digest = strings.Repeat("b", 64)
	if _, err := r.store.IngestReplayBatch(t.Context(), r.session, b, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("sequence identity changed", err)
	}
	if err := runEngineMigration(t, r.f.db, r.f.engine, "054_probe_telemetry_receipts", "down"); err == nil {
		t.Fatal("downgrade discarded receipts")
	}
	if replayCount(t, r.f, "probe_telemetry_receipts") != 10 {
		t.Fatal("guard lost evidence")
	}
}
func testReplayRollback(t *testing.T, r replayFixture) {
	ctx := t.Context()
	trigger := "CREATE TRIGGER fail_replay_cursor BEFORE UPDATE ON probe_streams BEGIN SELECT RAISE(ABORT, 'private cursor fault'); END"
	if r.f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_replay_cursor BEFORE UPDATE ON probe_streams FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'private cursor fault'"
	}
	if _, err := r.f.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_replay_cursor") })
	if _, err := r.f.db.ExecContext(ctx, "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}
	b := r.batch(r.observation(1), r.incident(2, 1), r.delivery(3, 1, 1, domain.DeliveryStatusSent))
	if _, err := r.store.IngestReplayBatch(ctx, r.session, b, &services.AccessService{}); !errors.Is(err, domain.ErrInternal) {
		t.Fatalf("fault leaked or accepted: %v", err)
	}
	for _, table := range []string{"probe_observations", "probe_incidents", "probe_delivery_events", "probe_telemetry_receipts", "probe_dirty_buckets", "monitor_probe_state"} {
		if replayCount(t, r.f, table) != 0 {
			t.Fatalf("%s leaked from rollback", table)
		}
	}
	if cursor, err := r.store.GetCursor(ctx, r.session.ProbeID, r.session.StreamID); err != nil || cursor != 0 {
		t.Fatal("cursor advanced", cursor, err)
	}
	if _, err := r.f.db.ExecContext(ctx, "DROP TRIGGER fail_replay_cursor"); err != nil {
		t.Fatal(err)
	}
	if result := r.ingest(t, b); result.AcceptedCount != 3 {
		t.Fatalf("retry failed: %+v", result)
	}
}
func testReplayFences(t *testing.T, r replayFixture) {
	b := r.batch(r.observation(1))
	for _, change := range []func(*domain.ProbeReplaySession){func(s *domain.ProbeReplaySession) { s.ConnectionGeneration++ }, func(s *domain.ProbeReplaySession) { s.OwnerID = s.HubID }, func(s *domain.ProbeReplaySession) { s.HubID = s.OwnerID }, func(s *domain.ProbeReplaySession) { s.StreamID = s.OwnerID }} {
		s := r.session
		change(&s)
		batch := b
		batch.StreamID = s.StreamID
		if _, err := r.store.IngestReplayBatch(t.Context(), s, batch, &services.AccessService{}); err == nil {
			t.Fatal("stale/foreign authority accepted")
		}
	}
	if _, err := r.f.db.ExecContext(t.Context(), "UPDATE probe_streams SET retired_at = ?", r.at); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.IngestReplayBatch(t.Context(), r.session, b, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retired stream accepted", err)
	}
	if _, err := r.f.db.ExecContext(t.Context(), "UPDATE probe_streams SET retired_at = NULL"); err != nil {
		t.Fatal(err)
	}
	r.ingest(t, b)
	if _, err := r.f.db.ExecContext(t.Context(), "UPDATE probe_sessions SET lease_until = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.IngestReplayBatch(t.Context(), r.session, b, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired duplicate accepted", err)
	}
}
func testReplayHistory(t *testing.T, r replayFixture) {
	r.ingest(t, r.batch(r.observation(1)))
	set, err := r.f.assignments.Replace(t.Context(), r.monitor, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown)
	if err != nil {
		t.Fatal(err)
	}
	historical := r.observation(2)
	historical.Observation.Status = domain.StatusDown
	historical.Observation.RawStatus = domain.StatusDown
	historical.Observation.DownCount = 1
	if result := r.ingest(t, r.batch(historical)); result.AcceptedCount != 1 {
		t.Fatalf("valid history lost: %+v", result)
	}
	state, err := r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
	if err != nil || state.Seq != 1 {
		t.Fatal("historical generation changed current", state, err)
	}
	tooLate := r.observation(3)
	tooLate.ObservedAt = time.Now().UTC()
	tooLate.Observation.ObservedAt = tooLate.ObservedAt
	if result := r.ingest(t, r.batch(tooLate)); len(result.Rejected) != 1 || result.Rejected[0].Code != "assignment_unauthorized" {
		t.Fatalf("removed assignment accepted: %+v", result)
	}
	if _, err := r.f.assignments.Replace(t.Context(), r.monitor, set.Revision, []string{r.session.ProbeID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := r.syncer.RefreshRemote(t.Context(), syncTarget(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	future := r.observation(4)
	future.ObservedAt = time.Now().UTC().Add(20 * time.Second)
	future.Observation.ObservedAt = future.ObservedAt
	future.Observation.ConfigRevision = 2
	future.Observation.AssignmentGeneration = 2
	if result := r.ingest(t, r.batch(future)); result.AcceptedCount != 1 {
		t.Fatalf("bounded future evidence rejected %+v", result)
	}
	state, err = r.f.commits.GetState(t.Context(), r.monitor, r.session.ProbeID)
	if err != nil || state.Seq != 1 {
		t.Fatal("future evidence became live", state, err)
	}
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM monitors WHERE id = ?", r.monitor); err != nil {
		t.Fatal(err)
	}
	gone := r.observation(5)
	if result := r.ingest(t, r.batch(gone)); len(result.Rejected) != 1 || result.Rejected[0].Code != "monitor_not_found" {
		t.Fatalf("deleted monitor recreated %+v", result)
	}
}
func testReplayAuthority(t *testing.T, r replayFixture) {
	noParent := r.delivery(1, 1, 1, domain.DeliveryStatusSent)
	wrongVersion := r.delivery(3, 1, 1, domain.DeliveryStatusSent)
	wrongVersion.Delivery.NotificationVersion = 2
	result := r.ingest(t, r.batch(noParent, r.incident(2, 1), wrongVersion))
	if result.AcceptedCount != 1 || len(result.Rejected) != 2 || result.Rejected[0].Code != "delivery_parent_not_found" || result.Rejected[1].Code != "channel_unauthorized" {
		t.Fatalf("parent/channel authorization: %+v", result)
	}
	wrongIdentity := r.incident(4, 2)
	wrongIdentity.Incident.StartedAt = wrongIdentity.Incident.StartedAt.Add(-time.Hour)
	if result := r.ingest(t, r.batch(wrongIdentity)); len(result.Rejected) != 1 || result.Rejected[0].Code != "transition_identity_conflict" {
		t.Fatalf("incident identity rewritten: %+v", result)
	}
	// Missing seq5 is framing failure, never a permanently skipped sequence.
	if _, err := r.store.IngestReplayBatch(t.Context(), r.session, r.batch(r.observation(6)), &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("gap silently skipped", err)
	}
}

func testReplayPrecision(t *testing.T, r replayFixture) {
	first, last := r.incident(1, 1), r.incident(2, 2)
	first.Incident.StartedAt = first.Incident.StartedAt.Add(123 * time.Nanosecond)
	last.Incident.StartedAt = first.Incident.StartedAt
	if result := r.ingest(t, r.batch(first, last)); result.AcceptedCount != 2 {
		t.Fatalf("wire nanoseconds broke persisted incident identity: %+v", result)
	}
}

func testReplayConcurrent(t *testing.T, r replayFixture) {
	type outcome struct {
		result *domain.ProbeReplayResult
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	b := r.batch(r.observation(1), r.incident(2, 1), r.delivery(3, 1, 1, domain.DeliveryStatusSent))
	for range 2 {
		go func() {
			<-start
			v, e := r.store.IngestReplayBatch(t.Context(), r.session, b, &services.AccessService{})
			results <- outcome{v, e}
		}()
	}
	close(start)
	var accepted, duplicates int64
	for range 2 {
		v := <-results
		if v.err != nil {
			t.Fatal(v.err)
		}
		accepted += v.result.AcceptedCount
		duplicates += v.result.DuplicateCount
	}
	if accepted != 3 || duplicates != 3 || replayCount(t, r.f, "probe_telemetry_receipts") != 3 {
		t.Fatal("concurrent workers duplicated a commit", accepted, duplicates)
	}
}

type delayedReplayAuthority struct{ delay time.Duration }

func (a delayedReplayAuthority) AuthorizeEvent(ctx context.Context, f domain.ProbeReplayAuthorityFacts, e domain.ProbeReplayEvent, now time.Time) (string, bool) {
	time.Sleep(a.delay)
	return (&services.AccessService{}).AuthorizeEvent(ctx, f, e, now)
}
func testReplayExpiry(t *testing.T, r replayFixture) {
	query := "UPDATE probe_sessions SET lease_until = unixepoch()+1"
	if r.f.engine == "mariadb" {
		query = "UPDATE probe_sessions SET lease_until = UNIX_TIMESTAMP()+1"
	}
	if _, err := r.f.db.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.IngestReplayBatch(t.Context(), r.session, r.batch(r.observation(1)), delayedReplayAuthority{1100 * time.Millisecond}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("lease expired before commit but evidence was accepted", err)
	}
	if replayCount(t, r.f, "probe_telemetry_receipts") != 0 || replayCount(t, r.f, "probe_observations") != 0 {
		t.Fatal("expiry rollback lost atomicity")
	}
}
