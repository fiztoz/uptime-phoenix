package repository_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type roundRow struct {
	MonitorID          int64
	RequestRound       int64
	AssignmentRevision int64
	RoundID            string
	HubID              string
	AlertDelivery      domain.AlertDelivery
	CreatedAt          time.Time
	ExpiresAt          time.Time
}
type roundRequestRow struct {
	RequestID      string
	RequestRound   int64
	ProbeID        string
	Generation     int64
	ConfigRevision int64
	StreamID       string
	SHA256         string
	CommandID      *string
	PreparedAt     time.Time
}

func latestRound(t *testing.T, db *bun.DB, monitor int64) roundRow {
	t.Helper()
	var r roundRow
	if err := db.NewSelect().Table("probe_delivery_preparation_rounds").Where("monitor_id = ?", monitor).Order("request_round DESC").Limit(1).Scan(t.Context(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func roundRequests(t *testing.T, db *bun.DB, monitor, round int64) []roundRequestRow {
	t.Helper()
	var rows []roundRequestRow
	if err := db.NewSelect().Table("probe_delivery_preparation_requests").Column("request_id", "request_round", "probe_id", "generation", "config_revision", "stream_id", "sha256", "command_id", "prepared_at").Where("monitor_id = ? AND request_round = ?", monitor, round).Order("probe_id ASC").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}
func expireRound(t *testing.T, db *bun.DB, monitor int64) {
	t.Helper()
	end := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := db.ExecContext(t.Context(), "UPDATE probe_delivery_preparation_rounds SET created_at = ?, expires_at = ? WHERE monitor_id = ? AND request_round = ?", end.Add(-24*time.Hour), end, monitor, 1); err != nil {
		t.Fatal(err)
	}
}
func ageRound(t *testing.T, db *bun.DB, monitor, round int64) {
	t.Helper()
	start := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Microsecond)
	if _, err := db.ExecContext(t.Context(), "UPDATE probe_delivery_preparation_rounds SET created_at = ?, expires_at = ? WHERE monitor_id = ? AND request_round = ?", start, start.Add(24*time.Hour), monitor, round); err != nil {
		t.Fatal(err)
	}
}
func publishRoundFact(t *testing.T, f preparationFixture) domain.ProbeDeliveryModeApplication {
	t.Helper()
	set, err := f.f.assignments.GetByMonitorID(t.Context(), f.monitor)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := f.syncer.RefreshRemote(t.Context(), syncTarget(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	lease := domain.ProbeConnectorLease{ProbeID: f.session.ProbeID, OwnerID: f.session.OwnerID, Generation: f.session.ConnectionGeneration}
	if err := f.syncer.RecordRemoteApplied(t.Context(), lease, domain.ProbeActiveConfig{ProbeConfigTarget: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AssignmentCount: 1, AppliedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var gen int64
	for _, a := range set.Assignments {
		if a.ProbeID == f.session.ProbeID {
			gen = a.Generation
		}
	}
	fact := domain.ProbeDeliveryModeApplication{ProbeConfigTarget: meta.ProbeConfigTarget, MonitorID: f.monitor, AssignmentRevision: set.Revision, Generation: gen, ConfigRevision: meta.Revision, SHA256: meta.SHA256, StreamID: f.session.StreamID, AlertDelivery: domain.AlertDeliveryRegional}
	if err := f.syncer.RecordRemoteRegionalApplications(t.Context(), f.session, []domain.ProbeDeliveryModeApplication{fact}); err != nil {
		t.Fatal(err)
	}
	return fact
}

// TestDeliveryPreparationRoundContract exercises the existing production issuer
// and separate local reconciler on both real engines; no pure mock accepts SQL.
func TestDeliveryPreparationRoundContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				run  func(*testing.T, preparationFixture)
			}{
				{"RemoteReuseCold", testRoundRemoteReuse}, {"TerminalCooldownRearm", testRoundTerminal}, {"ExpiryRearm", testRoundExpiry},
				{"MixedAllSources", testRoundMixed}, {"HistoricalFailure", testRoundHistorical}, {"ABA", testRoundABA},
				{"AtomicRollback", testRoundRollback}, {"CorruptMembership", testRoundCorrupt}, {"ConcurrentCreators", testRoundConcurrent},
				{"CanceledStaleAuthority", testRoundAuthority}, {"Migration", testRoundMigration},
			} {
				t.Run(tc.name, func(t *testing.T) { tc.run(t, newPreparationFixture(t, engine)) })
			}
		})
	}
}

// TestLocalDeliveryPreparationRoundRefresh verifies production protected refresh,
// stable document reuse, separate request expiry and regional provider effects.
func TestLocalDeliveryPreparationRoundRefresh(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) {
					p := newReviewPipeline(t, engine, true)
					set, err := p.f.assignments.GetByMonitorID(t.Context(), p.monitor.ID)
					if err != nil {
						t.Fatal(err)
					}
					replaceBindingMode(t, p.f, p.monitor.ID, set.Revision, domain.LocalProbeID, mode)
					refresh := refreshService(t, p.f, p.f.db)
					refresh.SetPreparationRounds(p.active.(ports.LocalDeliveryPreparationRoundReconciler))
					active, err := refresh.Refresh(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					original := *active
					if latestRound(t, p.f.db, p.monitor.ID).RequestRound != 1 || len(roundRequests(t, p.f.db, p.monitor.ID, 1)) != 1 {
						t.Fatal("fresh refresh omitted request round")
					}
					expireRound(t, p.f.db, p.monitor.ID)
					active, err = refresh.Refresh(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if *active != original || latestRound(t, p.f.db, p.monitor.ID).RequestRound != 2 || len(roundRequests(t, p.f.db, p.monitor.ID, 2)) != 1 {
						t.Fatal("request expiry changed config or lost round")
					}
					// Valid-shaped corruption refuses refresh but is not send authority.
					if _, err := p.f.db.ExecContext(t.Context(), "UPDATE probe_delivery_preparation_requests SET sha256 = ? WHERE monitor_id = ? AND request_round = ?", strings.Repeat("0", 64), p.monitor.ID, 2); err != nil {
						t.Fatal(err)
					}
					if got, err := refresh.Refresh(t.Context()); got != nil || !errors.Is(err, ports.ErrConflict) {
						t.Fatal("corrupt request repaired itself", err)
					}
					for _, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
						if err := p.heartbeats.Record(t.Context(), p.monitor, ports.CheckResult{Status: status, Message: "round is not execution authority", AssignmentGeneration: 1, ConfigRevision: original.Revision}); err != nil {
							t.Fatal(err)
						}
						p.send(t)
					}
					if len(p.sender.alerts) != 2 || p.sender.alerts[0].Status != domain.StatusDown || p.sender.alerts[1].Status != domain.StatusUp {
						t.Fatal("round corrupted regional DOWN/recovery")
					}
				})
			}
		})
	}
}

func testRoundRemoteReuse(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	initial := latestRound(t, f.f.db, f.monitor)
	requests := roundRequests(t, f.f.db, f.monitor, 1)
	commands := f.wantRows(t, 1)
	if initial.RequestRound != 1 || initial.AssignmentRevision != fact.AssignmentRevision || initial.HubID != fact.HubID || initial.AlertDelivery != domain.AlertDeliveryAggregate || initial.CreatedAt.IsZero() || !initial.ExpiresAt.Equal(initial.CreatedAt.Add(24*time.Hour)) || len(requests) != 1 || requests[0].RequestID != commands[0].CommandID || requests[0].CommandID == nil || *requests[0].CommandID != commands[0].CommandID || requests[0].ConfigRevision != fact.ConfigRevision || requests[0].SHA256 != fact.SHA256 || requests[0].StreamID != fact.StreamID {
		t.Fatal("round/request did not bind exact issued document")
	}
	cold := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f.f), probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), f.protector)
	cold.SetDeliveryPreparations(probe.DeliveryPreparationCodec{}, f.protector)
	if err := cold.IssueRemoteDeliveryPreparations(t.Context(), f.session, []domain.ProbeDeliveryModeApplication{fact}); err != nil {
		t.Fatal(err)
	}
	if latestRound(t, f.f.db, f.monitor) != initial || !reflect.DeepEqual(roundRequests(t, f.f.db, f.monitor, 1), requests) || !reflect.DeepEqual(f.wantRows(t, 1), commands) {
		t.Fatal("cold retry rewrote request provenance")
	}
	// Referenced command retention is a real FK, not a reader convention.
	if _, err := f.f.db.ExecContext(t.Context(), "DELETE FROM probe_commands WHERE command_id = ?", commands[0].CommandID); err == nil {
		t.Fatal("deleted round-bound request journal")
	}
}
func terminalRoundCommand(t *testing.T, f preparationFixture) preparationCommandRow {
	t.Helper()
	original := f.wantRows(t, 1)[0]
	store := f.commands(t)
	claimed, err := store.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true})
	if err != nil || claimed == nil {
		t.Fatal("claim", err)
	}
	if err := store.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: claimed.CommandID, Status: "rejected", Code: "delivery_binding_changed", Message: "refused current preparation"}); err != nil {
		t.Fatal(err)
	}
	return original
}
func testRoundTerminal(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryBoth)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	old := terminalRoundCommand(t, f)
	stored := f.wantRows(t, 1)[0]
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	if latestRound(t, f.f.db, f.monitor).RequestRound != 1 {
		t.Fatal("terminal failure bypassed cooldown")
	}
	ageRound(t, f.f.db, f.monitor, 1)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	rows := f.wantRows(t, 2)
	r := latestRound(t, f.f.db, f.monitor)
	if r.RequestRound != 2 || len(roundRequests(t, f.f.db, f.monitor, 2)) != 1 {
		t.Fatal("current terminal result did not rearm")
	}
	found := false
	for _, row := range rows {
		if row.CommandID == old.CommandID {
			found = true
			if !reflect.DeepEqual(row, stored) {
				t.Fatal("rearm rewrote old result")
			}
		}
	}
	if !found {
		t.Fatal("rearm deleted old request")
	}
	// Old remote duplicate receipt still commits only its own journal, never round2.
	if err := f.commands(t).CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: old.CommandID, Status: "rejected", Code: "delivery_binding_changed", Message: "refused current preparation"}); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	f.wantRows(t, 2)
	if latestRound(t, f.f.db, f.monitor).RequestRound != 2 {
		t.Fatal("old terminal result invalidated replacement")
	}
}
func testRoundExpiry(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	old := f.wantRows(t, 1)[0]
	expireRound(t, f.f.db, f.monitor)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	rows := f.wantRows(t, 2)
	if latestRound(t, f.f.db, f.monitor).RequestRound != 2 {
		t.Fatal("expired round did not advance")
	}
	for _, row := range rows {
		if row.CommandID == old.CommandID && !reflect.DeepEqual(row, old) {
			t.Fatal("expiry rewrote protected legacy request")
		}
	}
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	f.wantRows(t, 2)
	// A clock rollback cannot start new work from a future-created round.
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_delivery_preparation_rounds SET created_at = ?, expires_at = ? WHERE monitor_id = ? AND request_round = ?", start, start.Add(24*time.Hour), f.monitor, 2); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, fact); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("clock rollback granted new request eligibility", err)
	}
	f.wantRows(t, 2)
}
func testRoundMixed(t *testing.T, f preparationFixture) {
	_ = f.rebuildGraph(t, domain.AlertDeliveryBoth)
	_ = f.f.remote(t, probeRegistryID2, "offline-round-member")
	set, err := f.f.assignments.GetByMonitorID(t.Context(), f.monitor)
	if err != nil {
		t.Fatal(err)
	}
	store := repository.NewProbeAssignmentStore(f.f.db)
	if _, err = store.ReplaceWithBindings(t.Context(), f.monitor, set.Revision, []string{domain.LocalProbeID, f.session.ProbeID, probeRegistryID2}, set.HealthPolicy, domain.AlertDeliveryBoth, nil); err != nil {
		t.Fatal(err)
	}
	// Disabled/offline desired membership remains in the round; no fake enrollment.
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probes SET enabled = ? WHERE id = ?", false, probeRegistryID2); err != nil {
		t.Fatal(err)
	}
	fact := publishRoundFact(t, f)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	var members []domain.DeliveryPreparationMember
	if err := f.f.db.NewSelect().Table("probe_delivery_preparation_members").Column("probe_id", "generation").Where("monitor_id = ? AND request_round = ?", f.monitor, 1).Order("probe_id ASC").Scan(t.Context(), &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatal("round excluded offline/local member")
	}
	prepared := services.NewProbeConfigService(repository.NewProbeConfigStore(f.f.db), probe.ConfigInspector{}, f.protector)
	builder := services.NewLocalProbeConfigBuilder(repository.NewLocalProbeConfigSourceStore(f.f.db), probe.LocalConfigEncoder{}, prepared)
	at := time.Now().UTC()
	meta, err := builder.Prepare(t.Context(), f.session.HubID, 0, at, at)
	if err != nil {
		t.Fatal(err)
	}
	activateLocalDelivery(t, f.f, f.f.db, meta, 0)
	active := localPreparationActive(t, f.f)
	local := activationRepo(f.f, f.f.db).(ports.LocalDeliveryPreparationRoundReconciler)
	if err := local.ReconcileLocalDeliveryPreparationRounds(t.Context(), active); err != nil {
		t.Fatal("local did not join remote-created round", err)
	}
	requests := roundRequests(t, f.f.db, f.monitor, 1)
	if len(requests) != 2 || latestRound(t, f.f.db, f.monitor).RequestRound != 1 {
		t.Fatal("local and remote did not share one round")
	}
	for _, r := range requests {
		if r.ProbeID == domain.LocalProbeID && (r.CommandID != nil || r.StreamID != domain.LocalStreamID || r.ConfigRevision != active.Revision || r.SHA256 != active.SHA256) {
			t.Fatal("fabricated local remote identity")
		}
	}
	before := localPreparationRead(t, f.f, false, active)
	expireRound(t, f.f.db, f.monitor)
	if err := local.ReconcileLocalDeliveryPreparationRounds(t.Context(), active); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	if latestRound(t, f.f.db, f.monitor).RequestRound != 2 || len(roundRequests(t, f.f.db, f.monitor, 2)) != 2 {
		t.Fatal("expiry split joint round")
	}
	if !reflect.DeepEqual(before, localPreparationRead(t, f.f, false, active)) {
		t.Fatal("round reconciliation rewrote local foundational request")
	}
	facts := localRegionalRead(t, f.f, false)
	if len(facts.RegionalDeliveryApplications) != 1 || facts.RegionalDeliveryApplications[0].AlertDelivery != domain.AlertDeliveryRegional {
		t.Fatal("request round changed effective mode")
	}
}
func testRoundHistorical(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	_ = terminalRoundCommand(t, f)
	ageRound(t, f.f.db, f.monitor, 1)
	// Publish a changed config without altering the set; old-document failure is
	// historical and cannot keep rearming the still-current assignment round.
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE monitors SET name = ? WHERE id = ?", "new exact document", f.monitor); err != nil {
		t.Fatal(err)
	}
	current := publishRoundFact(t, f)
	if current.ConfigRevision <= fact.ConfigRevision {
		t.Fatal("fixture did not advance document")
	}
	if err := f.issue(t, current); err != nil {
		t.Fatal(err)
	}
	if latestRound(t, f.f.db, f.monitor).RequestRound != 1 {
		t.Fatal("historical terminal result rearmed")
	}
	f.wantRows(t, 2)
	if err := f.issue(t, current); err != nil {
		t.Fatal(err)
	}
	f.wantRows(t, 2)
	// Accepted-stream change removes the old request from terminal eligibility.
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_connections SET stream_id = ? WHERE probe_id = ?", probeRegistryID3, f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if err := f.issue(t, current); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("retired selector issued", err)
	}
}
func testRoundABA(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	_ = f.rebuildGraph(t, domain.AlertDeliveryRegional)
	current := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	f.wantRows(t, 1)
	if err := f.issue(t, current); err != nil {
		t.Fatal(err)
	}
	if latestRound(t, f.f.db, f.monitor).RequestRound != 2 || len(roundRequests(t, f.f.db, f.monitor, 1)) != 1 || len(roundRequests(t, f.f.db, f.monitor, 2)) != 1 {
		t.Fatal("ABA reused old round")
	}
}
func testRoundRollback(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	remove := injectOutboxFailure(t, f.f, "probe_delivery_preparation_requests", "INSERT")
	err := f.issue(t, fact)
	remove()
	if err == nil {
		t.Fatal("request failure returned success")
	}
	for _, table := range []string{"probe_delivery_preparation_rounds", "probe_delivery_preparation_members", "probe_delivery_preparation_requests", "probe_commands"} {
		assertTableCount(t, f.f, table, 0)
	}
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	expireRound(t, f.f.db, f.monitor)
	remove = injectOutboxFailure(t, f.f, "probe_delivery_preparation_members", "INSERT")
	err = f.issue(t, fact)
	remove()
	if err == nil {
		t.Fatal("rearm member failure returned success")
	}
	assertTableCount(t, f.f, "probe_delivery_preparation_rounds", 1)
	f.wantRows(t, 1)
}
func testRoundCorrupt(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	// Test-only corruption: detach its dependent request before editing membership.
	if _, err := f.f.db.ExecContext(t.Context(), "DELETE FROM probe_delivery_preparation_requests WHERE monitor_id = ?", f.monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_delivery_preparation_members SET generation = generation + 1 WHERE monitor_id = ?", f.monitor); err != nil {
		t.Fatal(err)
	}
	expireRound(t, f.f.db, f.monitor)
	if err := f.issue(t, fact); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expiry repaired corrupt membership", err)
	}
	f.wantRows(t, 1)
	assertTableCount(t, f.f, "probe_delivery_preparation_rounds", 1)
}
func testRoundConcurrent(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	cold := repository.NewRemoteProbeConfigSyncStore(reopenConfigDB(t, f.f), probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), f.protector)
	cold.SetDeliveryPreparations(probe.DeliveryPreparationCodec{}, f.protector)
	for cycle := 0; cycle < 2; cycle++ {
		if cycle > 0 {
			expireRound(t, f.f.db, f.monitor)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, s := range []*repository.RemoteProbeConfigSyncStore{f.syncer, cold} {
			wg.Add(1)
			go func(s *repository.RemoteProbeConfigSyncStore) {
				defer wg.Done()
				<-start
				results <- s.IssueRemoteDeliveryPreparations(ctx, f.session, []domain.ProbeDeliveryModeApplication{fact})
			}(s)
		}
		close(start)
		wg.Wait()
		cancel()
		close(results)
		for err := range results {
			if err != nil {
				t.Fatal("competing creator", err)
			}
		}
		if latestRound(t, f.f.db, f.monitor).RequestRound != int64(cycle+1) || len(roundRequests(t, f.f.db, f.monitor, int64(cycle+1))) != 1 {
			t.Fatal("competing creators split round")
		}
	}
	f.wantRows(t, 2)
}
func testRoundAuthority(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.syncer.IssueRemoteDeliveryPreparations(ctx, f.session, []domain.ProbeDeliveryModeApplication{fact}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled writer", err)
	}
	bad := f.session
	bad.OwnerID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if bad.OwnerID == f.session.OwnerID {
		t.Fatal("stale-owner fixture reused actual owner")
	}
	if err := f.syncer.IssueRemoteDeliveryPreparations(t.Context(), bad, []domain.ProbeDeliveryModeApplication{fact}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale owner created round", err)
	}
	assertTableCount(t, f.f, "probe_delivery_preparation_rounds", 0)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_delivery_preparation_rounds SET request_round = ? WHERE monitor_id = ?", math.MaxInt64, f.monitor); err == nil {
		t.Fatal("parent PK mutation ignored retained membership")
	}
}
func testRoundMigration(t *testing.T, f preparationFixture) {
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE probe_delivery_preparation_rounds SET request_round = 0",
		"UPDATE probe_delivery_preparation_rounds SET assignment_revision = 0",
		"UPDATE probe_delivery_preparation_rounds SET alert_delivery = 'regional'",
		"UPDATE probe_delivery_preparation_rounds SET expires_at = created_at",
		"UPDATE probe_delivery_preparation_members SET generation = 0",
		"UPDATE probe_delivery_preparation_requests SET config_revision = 0",
		"UPDATE probe_delivery_preparation_requests SET command_id = NULL",
		"UPDATE probe_delivery_preparation_requests SET sha256 = 'INVALID'",
	} {
		if _, err := f.f.db.ExecContext(t.Context(), q); err == nil {
			t.Fatal("schema accepted invalid request identity")
		}
	}
	if err := runEngineMigration(t, f.f.db, f.f.engine, "080_delivery_preparation_rounds", "down"); err != nil {
		t.Fatal(err)
	}
	f.wantRows(t, 1)
	if err := runEngineMigration(t, f.f.db, f.f.engine, "080_delivery_preparation_rounds", "up"); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, f.f, "probe_delivery_preparation_rounds", 0) // Explicit downgrade is destructive, never synthetic re-up/backfill.
	old := f.wantRows(t, 1)[0]
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	requests := roundRequests(t, f.f.db, f.monitor, 1)
	if len(requests) != 1 || requests[0].RequestID == old.CommandID {
		t.Fatal("downgrade/re-up aliased old request identity")
	}
	f.wantRows(t, 2)
}
