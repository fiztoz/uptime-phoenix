package repository_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
	"github.com/uptrace/bun"
)

func localCommit(monitorID, expected int64) domain.LocalHeartbeatCommit {
	at := time.Date(2026, 9, 17, 15, 4, 5, 0, time.FixedZone("UTC+7", 7*3600))
	return domain.LocalHeartbeatCommit{Heartbeat: domain.Heartbeat{
		MonitorID: monitorID, ProbeID: domain.LocalProbeID, StreamID: domain.LocalStreamID,
		AssignmentGeneration: 1, ConfigRevision: 1, Status: domain.StatusUp, Time: at, ReceivedAt: at,
	}, RawStatus: domain.StatusUp, ExpectedStateSeq: expected}
}

func localSequence(t *testing.T, f probeRegistryFixture) int64 {
	t.Helper()
	var seq int64
	if err := f.db.NewSelect().TableExpr("probe_local_sequence").Column("last_seq").Where("id = 1").Scan(context.Background(), &seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func localMonitor(t *testing.T, f probeRegistryFixture) int64 {
	t.Helper()
	id := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func runLocalSequenceMigration(t *testing.T, f probeRegistryFixture, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.engine, "migrations", "042_local_stream_sequence."+direction+".sql"))
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := f.db.ExecContext(context.Background(), statement); err != nil {
			return err
		}
	}
	return nil
}

func TestLocalHeartbeatContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("Rollback", func(t *testing.T) { testLocalHeartbeatRollback(t, newProbeRegistryFixture(t, engine)) })
			t.Run("ConcurrentConnections", func(t *testing.T) { testLocalHeartbeatConcurrency(t, newProbeRegistryFixture(t, engine)) })
			t.Run("StaleStateAndAssignments", func(t *testing.T) { testLocalHeartbeatGuards(t, newProbeRegistryFixture(t, engine)) })
			t.Run("RestartDeletionAndExhaustion", func(t *testing.T) { testLocalHeartbeatDurability(t, newProbeRegistryFixture(t, engine)) })
			t.Run("Migration", func(t *testing.T) { testLocalSequenceMigration(t, newProbeRegistryFixture(t, engine)) })
			t.Run("LifecycleAndOutbox", func(t *testing.T) { testLocalHeartbeatLifecycleAndOutbox(t, newProbeRegistryFixture(t, engine)) })
			t.Run("LifecycleFaultInjection", func(t *testing.T) { testLocalHeartbeatLifecycleFaultInjection(t, newProbeRegistryFixture(t, engine)) })
			t.Run("AckAndResolveCancellation", func(t *testing.T) { testLocalHeartbeatAckAndResolveCancellation(t, newProbeRegistryFixture(t, engine)) })
		})
	}
}

func testLocalHeartbeatRollback(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	// Creation already marks a dirty bucket. Clear it to make partial writes visible.
	if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"heartbeats", "probe_observations", "monitor_probe_state", "probe_dirty_buckets"} {
		trigger := fmt.Sprintf("CREATE TRIGGER fail_local_write BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'injected local write failure'); END", table)
		if f.engine == "mariadb" {
			trigger = fmt.Sprintf("CREATE TRIGGER fail_local_write BEFORE INSERT ON %s FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected local write failure'", table)
		}
		if _, err := f.db.ExecContext(ctx, trigger); err != nil {
			t.Fatal(err)
		}
		hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
		if err == nil || hb != nil {
			t.Fatalf("%s fault returned success/partial heartbeat: %+v %v", table, hb, err)
		}
		if _, err := f.db.ExecContext(ctx, "DROP TRIGGER fail_local_write"); err != nil {
			t.Fatal(err)
		}
		if got := localSequence(t, f); got != 0 {
			t.Fatalf("%s fault consumed sequence %d", table, got)
		}
		for _, changed := range []string{"heartbeats", "probe_observations", "monitor_probe_state", "probe_dirty_buckets"} {
			var count int
			if err := f.db.NewSelect().Table(changed).ColumnExpr("COUNT(*)").Scan(ctx, &count); err != nil || count != 0 {
				t.Fatalf("%s fault retained %s: %d %v", table, changed, count, err)
			}
		}
	}
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
	if err != nil || hb.SourceSeq != 1 {
		t.Fatalf("fault recovery skipped first sequence: %+v %v", hb, err)
	}
	stored, err := newEngineHeartbeatRepo(f).GetLatest(ctx, id)
	if err != nil || stored.ID != hb.ID || stored.SourceSeq != hb.SourceSeq || stored.ConfigRevision != hb.ConfigRevision {
		t.Fatalf("heartbeat identity differs from commit: %+v %v", stored, err)
	}
	state, err := f.commits.GetState(ctx, id, domain.LocalProbeID)
	if err != nil || state.Seq != hb.SourceSeq || state.ObservedAt.Location() != time.UTC || !state.ObservedAt.Equal(hb.Time) || hb.Time.Location() != time.UTC {
		t.Fatalf("regional/UTC commit mismatch: %+v %v", state, err)
	}
	rows, err := f.commits.ListObservations(ctx, id, domain.LocalProbeID, hb.Time.Add(-time.Second), hb.Time.Add(time.Second))
	if err != nil || len(rows) != 1 || rows[0].Seq != hb.SourceSeq {
		t.Fatalf("observation mismatch: %+v %v", rows, err)
	}
	var dirty int
	if err := f.db.NewSelect().TableExpr("probe_dirty_buckets").ColumnExpr("COUNT(*)").Scan(ctx, &dirty); err != nil || dirty != 4 {
		t.Fatalf("dirty work = %d, %v", dirty, err)
	}
}

func peerLocalRecorder(t *testing.T, f probeRegistryFixture) ports.LocalHeartbeatRecorder {
	t.Helper()
	if f.engine == "sqlite" {
		db, err := sqlite.NewDB(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return sqlite.NewRegionalCommitRepo(db)
	}
	db, err := mariadb.NewDB(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return mariadb.NewRegionalCommitRepo(db)
}

func testLocalHeartbeatConcurrency(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	peer := peerLocalRecorder(t, f)
	const workers = 8
	ids := make([]int64, workers)
	for i := range ids {
		ids[i] = localMonitor(t, f)
	}
	errorsOut := make(chan error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, id := range ids {
		recorder := f.localHeartbeat
		if i%2 == 1 {
			recorder = peer
		}
		wg.Go(func() {
			<-start
			var expected int64
			for range 4 {
				hb, err := recorder.CommitLocalHeartbeat(ctx, localCommit(id, expected))
				if err != nil {
					errorsOut <- err
					return
				}
				expected = hb.SourceSeq
			}
			errorsOut <- nil
		})
	}
	close(start)
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	var seqs []int64
	if err := f.db.NewSelect().TableExpr("heartbeats").Column("source_seq").Order("source_seq ASC").Scan(ctx, &seqs); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 4*workers || localSequence(t, f) != 4*workers {
		t.Fatalf("lost concurrent writes: %v", seqs)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("sequence gap/reuse: %v", seqs)
		}
	}
	// Same-monitor contention must re-evaluate retries after the winning commit.
	id := localMonitor(t, f)
	start = make(chan struct{})
	errorsOut = make(chan error, 2)
	for _, recorder := range []ports.LocalHeartbeatRecorder{f.localHeartbeat, peer} {
		wg.Go(func() {
			svc := services.NewHeartbeatService(newEngineHeartbeatRepo(f), silentBus{})
			svc.SetRegionalRecorder(f.assignments, recorder)
			<-start
			errorsOut <- svc.Record(ctx, &domain.Monitor{ID: id, MaxRetries: 1}, ports.CheckResult{Status: domain.StatusDown})
		})
	}
	close(start)
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := f.commits.GetState(ctx, id, domain.LocalProbeID)
	if err != nil || state.DownCount != 2 || state.Status != domain.StatusDown {
		t.Fatalf("concurrent retry counter: %+v %v", state, err)
	}
}

func testLocalHeartbeatGuards(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	first, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
	if err != nil {
		t.Fatal(err)
	}
	if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0)); !errors.Is(err, ports.ErrStaleLocalState) || hb != nil {
		t.Fatalf("stale state accepted: %+v %v", hb, err)
	}
	if localSequence(t, f) != 1 {
		t.Fatal("stale evaluation consumed sequence")
	}
	next := localCommit(id, first.SourceSeq)
	next.Heartbeat.Status, next.RawStatus, next.Heartbeat.DownCount = domain.StatusDown, domain.StatusDown, 1
	second, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.commits.GetState(ctx, id, domain.LocalProbeID)
	if err != nil || state.LastSuccessAt == nil || !state.LastSuccessAt.Equal(first.Time) {
		t.Fatalf("failure lost last success: %+v %v", state, err)
	}
	for _, mutate := range []func(*domain.LocalHeartbeatCommit){
		func(c *domain.LocalHeartbeatCommit) { c.Heartbeat.SourceSeq = 9 },
		func(c *domain.LocalHeartbeatCommit) { c.Heartbeat.ProbeID = probeRegistryID1 },
		func(c *domain.LocalHeartbeatCommit) { c.Heartbeat.AssignmentGeneration = 0 },
		func(c *domain.LocalHeartbeatCommit) { c.ExpectedStateSeq = -1 },
		func(c *domain.LocalHeartbeatCommit) { c.Heartbeat.Status = domain.StatusUnknown },
	} {
		invalid := localCommit(id, second.SourceSeq)
		mutate(&invalid)
		if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, invalid); !errors.Is(err, domain.ErrValidation) || hb != nil {
			t.Fatalf("invalid local commit accepted: %+v %v", hb, err)
		}
	}
	f.remote(t, probeRegistryID1, "remote")
	if _, err := f.assignments.Replace(ctx, id, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	svc := services.NewHeartbeatService(newEngineHeartbeatRepo(f), silentBus{})
	svc.SetRegionalRecorder(f.assignments, f.localHeartbeat)
	if err := svc.Record(ctx, &domain.Monitor{ID: id}, ports.CheckResult{Status: domain.StatusUp}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("removed assignment recorded locally: %v", err)
	}
	if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, second.SourceSeq)); !errors.Is(err, ports.ErrNotFound) || hb != nil {
		t.Fatalf("removed assignment committed: %+v %v", hb, err)
	}
	if _, err := f.assignments.Replace(ctx, id, 2, []string{"local"}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, second.SourceSeq)); !errors.Is(err, ports.ErrConflict) || hb != nil {
		t.Fatalf("old assignment generation committed: %+v %v", hb, err)
	}
	if localSequence(t, f) != 2 {
		t.Fatal("invalid/removed generation consumed sequence")
	}

	// Active configuration revision guard:
	type probeConfigSnapshotRow struct {
		bun.BaseModel    `bun:"table:probe_config_snapshots"`
		ProbeID          string    `bun:"probe_id,pk"`
		Revision         int64     `bun:"revision,pk"`
		HubID            string    `bun:"hub_id,notnull"`
		SchemaVersion    int       `bun:"schema_version,notnull"`
		SHA256           string    `bun:"sha256,notnull"`
		CreatedAt        time.Time `bun:"source_created_at,notnull"`
		EffectiveAt      time.Time `bun:"effective_at,notnull"`
		ProtectedPayload []byte    `bun:"protected_payload,notnull"`
		StoredAt         time.Time `bun:"stored_at,notnull"`
	}
	payload := append([]byte{0x01}, bytes.Repeat([]byte{0x00}, 30)...)
	if _, err := f.db.NewInsert().Model(&probeConfigSnapshotRow{
		ProbeID:          domain.LocalProbeID,
		Revision:         5,
		HubID:            "00000000-0000-0000-0000-000000000001",
		SchemaVersion:    1,
		SHA256:           strings.Repeat("a", 64),
		CreatedAt:        time.Now().UTC(),
		EffectiveAt:      time.Now().UTC(),
		ProtectedPayload: payload,
		StoredAt:         time.Now().UTC(),
	}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	type probeActiveConfigRow struct {
		bun.BaseModel   `bun:"table:probe_active_configs"`
		ProbeID         string    `bun:"probe_id,pk"`
		Revision        int64     `bun:"revision,notnull"`
		SHA256          string    `bun:"sha256,notnull"`
		HubID           string    `bun:"hub_id,notnull"`
		AppliedAt       time.Time `bun:"applied_at,notnull"`
		AssignmentCount int       `bun:"assignment_count,notnull"`
	}
	if _, err := f.db.NewInsert().Model(&probeActiveConfigRow{
		ProbeID:         domain.LocalProbeID,
		Revision:        5,
		SHA256:          strings.Repeat("a", 64),
		HubID:           "00000000-0000-0000-0000-000000000001",
		AppliedAt:       time.Now().UTC(),
		AssignmentCount: 1,
	}).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	staleRevCommit := localCommit(id, second.SourceSeq)
	staleRevCommit.Heartbeat.AssignmentGeneration = 2
	staleRevCommit.Heartbeat.ConfigRevision = 4
	if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, staleRevCommit); !errors.Is(err, ports.ErrConflict) || hb != nil {
		t.Fatalf("stale config revision committed: %+v %v", hb, err)
	}

	matchingRevCommit := localCommit(id, second.SourceSeq)
	matchingRevCommit.Heartbeat.AssignmentGeneration = 2
	matchingRevCommit.Heartbeat.ConfigRevision = 5
	if hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, matchingRevCommit); err != nil || hb == nil {
		t.Fatalf("matching config revision failed: %+v %v", hb, err)
	}
	for _, batch := range []domain.ProbeIngestBatch{{ProbeID: "local", StreamID: remoteStreamID}, {ProbeID: probeRegistryID1, StreamID: domain.LocalStreamID}} {
		batch.FromSeq, batch.ThroughSeq = 1, 1
		batch.Events = []domain.RegionalObservation{regionalSample(id, batch.ProbeID, batch.StreamID, 1, 1, domain.StatusUp, 0, first.Time)}
		if _, err := f.ingest.Ingest(ctx, batch); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("ingest claimed local stream: %v", err)
		}
	}
}

func testLocalHeartbeatDurability(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	if _, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0)); err != nil {
		t.Fatal(err)
	}
	if err := newEngineMonitorRepo(f).Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	peer := peerLocalRecorder(t, f)
	id = localMonitor(t, f)
	hb, err := peer.CommitLocalHeartbeat(ctx, localCommit(id, 0))
	if err != nil || hb.SourceSeq != 2 {
		t.Fatalf("reopened recorder reused deleted sequence: %+v %v", hb, err)
	}
	if _, err := f.db.ExecContext(ctx, "UPDATE probe_local_sequence SET last_seq = ? WHERE id = 1", int64(math.MaxInt64-1)); err != nil {
		t.Fatal(err)
	}
	last, err := peer.CommitLocalHeartbeat(ctx, localCommit(id, hb.SourceSeq))
	if err != nil || last.SourceSeq != math.MaxInt64 {
		t.Fatalf("maximum sequence lost: %+v %v", last, err)
	}
	if hb, err := peer.CommitLocalHeartbeat(ctx, localCommit(id, last.SourceSeq)); !errors.Is(err, ports.ErrConflict) || hb != nil {
		t.Fatalf("exhaustion wrapped: %+v %v", hb, err)
	}
	if localSequence(t, f) != math.MaxInt64 {
		t.Fatal("overflow changed allocator")
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_local_sequence"); err != nil {
		t.Fatal(err)
	}
	if hb, err := peer.CommitLocalHeartbeat(ctx, localCommit(id, last.SourceSeq)); !errors.Is(err, ports.ErrConflict) || hb != nil {
		t.Fatalf("missing counter silently recreated: %+v %v", hb, err)
	}
}

func testLocalSequenceMigration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	if err := runLocalSequenceMigration(t, f, "down"); err != nil {
		t.Fatal(err)
	}
	hb := localCommit(id, 0).Heartbeat
	hb.SourceSeq = 7
	if err := newEngineHeartbeatRepo(f).Save(ctx, &hb); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO probe_observations
 (monitor_id, probe_id, assignment_generation, stream_id, seq, config_revision, status, raw_status, down_count, message, observed_at, received_at)
 VALUES (?, 'local', 1, ?, 3, 1, 1, 1, 0, 'legacy observation', ?, ?)`, id, domain.LocalStreamID, hb.Time.UTC(), hb.Time.UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `INSERT INTO monitor_probe_state
 (monitor_id, probe_id, assignment_generation, stream_id, seq, config_revision, status, down_count, observed_at, received_at)
 VALUES (?, 'local', 1, ?, 5, 1, 1, 0, ?, ?)`, id, domain.LocalStreamID, hb.Time.UTC(), hb.Time.UTC()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := runLocalSequenceMigration(t, f, "up"); err != nil {
			t.Fatal(err)
		}
	}
	if localSequence(t, f) != 7 {
		t.Fatal("migration ignored highest retained evidence")
	}
	next, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 5))
	if err != nil || next.SourceSeq != 8 {
		t.Fatalf("migration collided with old rows: %+v %v", next, err)
	}
	if next.ID <= hb.ID {
		t.Fatal("migration changed heartbeat IDs")
	}
	if err := newEngineMonitorRepo(f).Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := runLocalSequenceMigration(t, f, "up"); err != nil {
		t.Fatal(err)
	}
	if localSequence(t, f) != 8 {
		t.Fatal("migration lowered high-water after deletion")
	}
	if err := runLocalSequenceMigration(t, f, "down"); err == nil {
		t.Fatal("downgrade discarded sequence high-water")
	}
	if localSequence(t, f) != 8 {
		t.Fatal("downgrade guard lost allocator")
	}
	for _, statement := range []string{"UPDATE probe_local_sequence SET last_seq = -1", "INSERT INTO probe_local_sequence (id,last_seq) VALUES (2,0)"} {
		if _, err := f.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %s", statement)
		}
	}
}

func testLocalHeartbeatLifecycleAndOutbox(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	uID := f.user(t)
	mID := localMonitor(t, f)
	notifID := f.notification(t, uID)
	policyID := f.escalationPolicy(t, uID)

	at := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	downCommit := domain.LocalHeartbeatCommit{
		Heartbeat: domain.Heartbeat{
			MonitorID:            mID,
			ProbeID:              domain.LocalProbeID,
			StreamID:             domain.LocalStreamID,
			AssignmentGeneration: 1,
			ConfigRevision:       1,
			Status:               domain.StatusDown,
			DownCount:            1,
			Time:                 at,
			ReceivedAt:           at,
			Msg:                  "Connection refused",
		},
		RawStatus:        domain.StatusDown,
		ExpectedStateSeq: 0,
		Incident: &domain.RegionalIncident{
			Status: domain.AlertStatusFiring,
		},
		Alert: &domain.Alert{
			Status: domain.AlertStatusFiring,
		},
		Escalation: &domain.AlertEscalation{
			PolicyID:  policyID,
			NextStep:  1,
			NextRunAt: at.Add(5 * time.Minute),
		},
		ThrottleUpdate: true,
		DeliveryIntents: []domain.DeliveryIntent{
			{NotificationID: notifID, NotificationVersion: 1},
		},
	}

	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, downCommit)
	if err != nil {
		t.Fatalf("down commit failed: %v", err)
	}
	if hb.SourceSeq != 1 {
		t.Fatalf("expected seq 1, got %d", hb.SourceSeq)
	}

	// Verify alert was created
	var alert repository.AlertModel
	if err := f.db.NewSelect().Model(&alert).Where("monitor_id = ?", mID).Scan(ctx); err != nil {
		t.Fatalf("failed to query alert: %v", err)
	}
	if alert.Status != domain.AlertStatusFiring || alert.TransitionVersion != 1 || alert.SourceAlertID == "" || alert.AckToken == "" || alert.OpenMonitorID == nil {
		t.Fatalf("alert shape mismatch: %+v", alert)
	}

	// Verify probe_incident was created with identical source_alert_id and transition_version
	inc, err := f.incidents.GetIncident(ctx, alert.SourceAlertID)
	if err != nil {
		t.Fatalf("failed to query incident: %v", err)
	}
	if inc.Status != domain.AlertStatusFiring || inc.TransitionVersion != 1 || inc.SourceAlertID != alert.SourceAlertID {
		t.Fatalf("incident shape mismatch: %+v", inc)
	}

	// Verify escalation was created
	var esc repository.AlertEscalationModel
	if err := f.db.NewSelect().Model(&esc).Where("alert_id = ?", alert.ID).Scan(ctx); err != nil {
		t.Fatalf("failed to query escalation: %v", err)
	}
	if esc.Status != domain.EscalationStatePending || esc.NextStep != 1 || esc.PolicyID != policyID {
		t.Fatalf("escalation shape mismatch: %+v", esc)
	}

	// Verify throttle was recorded
	var throttleCount int
	if err := f.db.NewSelect().Table("notification_throttles").ColumnExpr("COUNT(*)").Where("monitor_id = ?", mID).Scan(ctx, &throttleCount); err != nil || throttleCount != 1 {
		t.Fatalf("throttle was not recorded: count=%d, %v", throttleCount, err)
	}

	// Verify delivery intent was created
	var intentCount int
	if err := f.db.NewSelect().Table("probe_delivery_intents").ColumnExpr("COUNT(*)").Where("source_alert_id = ?", alert.SourceAlertID).Scan(ctx, &intentCount); err != nil || intentCount != 1 {
		t.Fatalf("failed to query intents: count=%d, %v", intentCount, err)
	}

	// 2. Recovery transition:
	recTime := at.Add(2 * time.Minute)
	upCommit := domain.LocalHeartbeatCommit{
		Heartbeat: domain.Heartbeat{
			MonitorID:            mID,
			ProbeID:              domain.LocalProbeID,
			StreamID:             domain.LocalStreamID,
			AssignmentGeneration: 1,
			ConfigRevision:       1,
			Status:               domain.StatusUp,
			DownCount:            0,
			Time:                 recTime,
			ReceivedAt:           recTime,
			Msg:                  "OK",
		},
		RawStatus:        domain.StatusUp,
		ExpectedStateSeq: 1,
		Incident: &domain.RegionalIncident{
			SourceAlertID: alert.SourceAlertID,
			Status:        domain.AlertStatusResolved,
			ResolvedAt:    &recTime,
		},
		Alert: &domain.Alert{
			SourceAlertID: alert.SourceAlertID,
			Status:        domain.AlertStatusResolved,
			ResolvedAt:    &recTime,
		},
		ThrottleClear: true,
		DeliveryIntents: []domain.DeliveryIntent{
			{NotificationID: notifID, NotificationVersion: 1, EventKind: domain.DeliveryEventIncidentSummary},
		},
	}

	hb2, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, upCommit)
	if err != nil {
		t.Fatalf("up commit failed: %v", err)
	}
	if hb2.SourceSeq != 2 {
		t.Fatalf("expected seq 2, got %d", hb2.SourceSeq)
	}

	// Verify alert is resolved
	var resolvedAlert repository.AlertModel
	if err := f.db.NewSelect().Model(&resolvedAlert).Where("id = ?", alert.ID).Scan(ctx); err != nil {
		t.Fatalf("failed to re-query alert: %v", err)
	}
	if resolvedAlert.Status != domain.AlertStatusResolved || resolvedAlert.TransitionVersion != 2 || resolvedAlert.OpenMonitorID != nil || resolvedAlert.ResolvedAt == nil {
		t.Fatalf("resolved alert mismatch: %+v", resolvedAlert)
	}

	// Verify probe_incident is resolved
	inc, err = f.incidents.GetIncident(ctx, alert.SourceAlertID)
	if err != nil {
		t.Fatalf("failed to re-query incident: %v", err)
	}
	if inc.Status != domain.AlertStatusResolved || inc.TransitionVersion != 2 || inc.ResolvedAt == nil {
		t.Fatalf("resolved incident mismatch: %+v", inc)
	}

	// Verify escalation is resolved
	if err := f.db.NewSelect().Model(&esc).Where("alert_id = ?", alert.ID).Scan(ctx); err != nil {
		t.Fatalf("failed to re-query escalation: %v", err)
	}
	if esc.Status != domain.EscalationStateCanceled {
		t.Fatalf("escalation status on resolve: %s", esc.Status)
	}

	// Verify throttle was cleared
	assertTableCount(t, f, "notification_throttles", 0)

	// Verify delivery intents: 2 rows (1 superseded, 1 pending)
	var supersededCount int
	if err := f.db.NewSelect().Table("probe_delivery_intents").ColumnExpr("COUNT(*)").
		Where("source_alert_id = ? AND status = ?", alert.SourceAlertID, domain.DeliveryStatusSuperseded).
		Scan(ctx, &supersededCount); err != nil || supersededCount != 1 {
		t.Fatalf("superseded intent count = %d, err = %v", supersededCount, err)
	}
	var pendingCount int
	if err := f.db.NewSelect().Table("probe_delivery_intents").ColumnExpr("COUNT(*)").
		Where("source_alert_id = ? AND status = ? AND event_kind = ?", alert.SourceAlertID, domain.DeliveryStatusPending, domain.DeliveryEventIncidentSummary).
		Scan(ctx, &pendingCount); err != nil || pendingCount != 1 {
		t.Fatalf("pending recovery intent count = %d, err = %v", pendingCount, err)
	}
}

func testLocalHeartbeatLifecycleFaultInjection(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	uID := f.user(t)
	mID := localMonitor(t, f)
	notifID := f.notification(t, uID)
	policyID := f.escalationPolicy(t, uID)

	// Clear dirty buckets to observe partial writes
	if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 9, 19, 11, 0, 0, 0, time.UTC)
	for _, table := range []string{"alerts", "probe_incidents", "probe_delivery_intents", "notification_throttles", "alert_escalations"} {
		trigger := fmt.Sprintf("CREATE TRIGGER fail_local_write BEFORE INSERT ON %s BEGIN SELECT RAISE(ABORT, 'injected lifecycle write failure'); END", table)
		if f.engine == "mariadb" {
			trigger = fmt.Sprintf("CREATE TRIGGER fail_local_write BEFORE INSERT ON %s FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected lifecycle write failure'", table)
		}
		if _, err := f.db.ExecContext(ctx, trigger); err != nil {
			t.Fatal(err)
		}

		downCommit := domain.LocalHeartbeatCommit{
			Heartbeat: domain.Heartbeat{
				MonitorID:            mID,
				ProbeID:              domain.LocalProbeID,
				StreamID:             domain.LocalStreamID,
				AssignmentGeneration: 1,
				ConfigRevision:       1,
				Status:               domain.StatusDown,
				DownCount:            1,
				Time:                 at,
				ReceivedAt:           at,
				Msg:                  "Connection refused",
			},
			RawStatus:        domain.StatusDown,
			ExpectedStateSeq: 0,
			Incident: &domain.RegionalIncident{
				Status: domain.AlertStatusFiring,
			},
			Alert: &domain.Alert{
				Status: domain.AlertStatusFiring,
			},
			Escalation: &domain.AlertEscalation{
				PolicyID:  policyID,
				NextStep:  1,
				NextRunAt: at.Add(5 * time.Minute),
			},
			ThrottleUpdate: true,
			DeliveryIntents: []domain.DeliveryIntent{
				{NotificationID: notifID, NotificationVersion: 1},
			},
		}

		hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, downCommit)
		if err == nil || hb != nil {
			t.Fatalf("%s fault returned success/partial: %+v, %v", table, hb, err)
		}
		if _, err := f.db.ExecContext(ctx, "DROP TRIGGER fail_local_write"); err != nil {
			t.Fatal(err)
		}
		if got := localSequence(t, f); got != 0 {
			t.Fatalf("%s fault consumed sequence %d", table, got)
		}
		for _, changed := range []string{
			"heartbeats", "probe_observations", "monitor_probe_state", "probe_dirty_buckets",
			"alerts", "probe_incidents", "probe_delivery_intents", "notification_throttles", "alert_escalations",
		} {
			assertTableCount(t, f, changed, 0)
		}
	}

	// Fault recovery: commit succeeds cleanly
	downCommit := domain.LocalHeartbeatCommit{
		Heartbeat: domain.Heartbeat{
			MonitorID:            mID,
			ProbeID:              domain.LocalProbeID,
			StreamID:             domain.LocalStreamID,
			AssignmentGeneration: 1,
			ConfigRevision:       1,
			Status:               domain.StatusDown,
			DownCount:            1,
			Time:                 at,
			ReceivedAt:           at,
			Msg:                  "Connection refused",
		},
		RawStatus:        domain.StatusDown,
		ExpectedStateSeq: 0,
		Incident: &domain.RegionalIncident{
			Status: domain.AlertStatusFiring,
		},
		Alert: &domain.Alert{
			Status: domain.AlertStatusFiring,
		},
		Escalation: &domain.AlertEscalation{
			PolicyID:  policyID,
			NextStep:  1,
			NextRunAt: at.Add(5 * time.Minute),
		},
		ThrottleUpdate: true,
		DeliveryIntents: []domain.DeliveryIntent{
			{NotificationID: notifID, NotificationVersion: 1},
		},
	}
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, downCommit)
	if err != nil || hb.SourceSeq != 1 {
		t.Fatalf("fault recovery failed: %+v %v", hb, err)
	}
}

func testLocalHeartbeatAckAndResolveCancellation(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	uID := f.user(t)
	mID := localMonitor(t, f)
	notifID := f.notification(t, uID)
	policyID := f.escalationPolicy(t, uID)

	at := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	downCommit := domain.LocalHeartbeatCommit{
		Heartbeat: domain.Heartbeat{
			MonitorID:            mID,
			ProbeID:              domain.LocalProbeID,
			StreamID:             domain.LocalStreamID,
			AssignmentGeneration: 1,
			ConfigRevision:       1,
			Status:               domain.StatusDown,
			DownCount:            1,
			Time:                 at,
			ReceivedAt:           at,
			Msg:                  "Connection refused",
		},
		RawStatus:        domain.StatusDown,
		ExpectedStateSeq: 0,
		Incident: &domain.RegionalIncident{
			Status: domain.AlertStatusFiring,
		},
		Alert: &domain.Alert{
			Status: domain.AlertStatusFiring,
		},
		Escalation: &domain.AlertEscalation{
			PolicyID:  policyID,
			NextStep:  1,
			NextRunAt: at.Add(5 * time.Minute),
		},
		ThrottleUpdate: true,
		DeliveryIntents: []domain.DeliveryIntent{
			{NotificationID: notifID, NotificationVersion: 1},
		},
	}
	_, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, downCommit)
	if err != nil {
		t.Fatalf("down commit failed: %v", err)
	}

	r := alertScopeRepos(f)
	alert, err := r.alerts.GetOpenByMonitorID(ctx, mID)
	if err != nil {
		t.Fatalf("failed to get open alert: %v", err)
	}

	// 1. Acknowledge alert via AlertService with EscalationService
	escSvc := services.NewEscalationService(r.policies, r.assignments, r.state, r.alerts, newEngineMonitorRepo(f), nil, &regionalAlertNotifier{})
	alertSvc := services.NewAlertService(r.alerts)
	alertSvc.SetEscalationCanceller(escSvc)

	alert, err = alertSvc.Acknowledge(ctx, alert.ID, &uID)
	if err != nil {
		t.Fatalf("ack failed: %v", err)
	}
	if alert.TransitionVersion != 2 {
		t.Fatalf("expected version 2 after ack, got %d", alert.TransitionVersion)
	}

	inc, err := f.incidents.GetIncident(ctx, alert.SourceAlertID)
	if err != nil {
		t.Fatalf("failed to query incident: %v", err)
	}
	if inc.Status != domain.AlertStatusAcked || inc.TransitionVersion != 2 || inc.AckedAt == nil {
		t.Fatalf("incident was not acked in lockstep: %+v", inc)
	}

	var esc repository.AlertEscalationModel
	if err := f.db.NewSelect().Model(&esc).Where("alert_id = ?", alert.ID).Scan(ctx); err != nil {
		t.Fatalf("failed to query escalation: %v", err)
	}
	if esc.Status != domain.EscalationStateCanceled {
		t.Fatalf("escalation was not cancelled on ack: %s", esc.Status)
	}

	var intentStatus string
	if err := f.db.NewSelect().Table("probe_delivery_intents").Column("status").Where("source_alert_id = ?", alert.SourceAlertID).Scan(ctx, &intentStatus); err != nil {
		t.Fatalf("failed to query intent status: %v", err)
	}
	if intentStatus != domain.DeliveryStatusSuperseded {
		t.Fatalf("intent was not superseded on ack: %s", intentStatus)
	}

	// 2. Resolve alert via AlertStore.Update
	resTime := at.Add(3 * time.Minute)
	alert.Status = domain.AlertStatusResolved
	alert.ResolvedAt = &resTime
	alert.OpenMonitorID = nil
	if err := r.alerts.Update(ctx, alert); err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if alert.TransitionVersion != 3 {
		t.Fatalf("expected version 3 after resolve, got %d", alert.TransitionVersion)
	}

	inc, err = f.incidents.GetIncident(ctx, alert.SourceAlertID)
	if err != nil {
		t.Fatalf("failed to query incident: %v", err)
	}
	if inc.Status != domain.AlertStatusResolved || inc.TransitionVersion != 3 || inc.ResolvedAt == nil {
		t.Fatalf("incident was not resolved in lockstep: %+v", inc)
	}

	assertTableCount(t, f, "notification_throttles", 0)
}
