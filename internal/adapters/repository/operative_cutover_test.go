package repository_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const operativeOwner = "99999999-9999-4999-8999-999999999999"

// operativeEvaluator wires the pure evaluators exactly as the core evaluator
// port expects. Pure results confer no database authority.
type operativeEvaluator struct{}

func (operativeEvaluator) EvaluateAggregate(in domain.AggregatePagingInput) (domain.AggregatePagingDecision, error) {
	return services.EvaluateAggregatePaging(in)
}

func (operativeEvaluator) EvaluateCutover(in domain.DeliveryCutoverInput) (domain.DeliveryCutoverDecision, error) {
	return services.EvaluateDeliveryCutover(in)
}

type operativeFixture struct {
	p     reviewPipeline
	store *repository.OperativeDeliveryStore
	round bool
}

func newOperativeStore(t *testing.T, f probeRegistryFixture) *repository.OperativeDeliveryStore {
	t.Helper()
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	syncer := repository.NewRemoteProbeConfigSyncStore(f.db, probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)
	return repository.NewOperativeDeliveryStore(f.db, probe.DeliveryCutoverCodec{}, protector, syncer, probe.LocalConfigEncoder{})
}

// newOperativeFixture builds a real activated local source graph in the desired
// mode (or leaves the default regional graph when mode is empty) with the given
// channel count, and optionally commits the real shared preparation round
// through the production machinery.
func newOperativeFixture(t *testing.T, engine string, mode domain.AlertDelivery, channels int, round bool) operativeFixture {
	t.Helper()
	ctx := t.Context()
	p := newReviewPipeline(t, engine, true)
	for i := 1; i < channels; i++ {
		nid := p.f.notification(t, p.f.user(t))
		if err := p.repos.MonitorNotificationRepo.Attach(ctx, p.monitor.ID, nid, true); err != nil {
			t.Fatal(err)
		}
	}
	if mode != "" {
		replaceBindingMode(t, p.f, p.monitor.ID, 1, domain.LocalProbeID, mode)
		builder, _ := sourceConfigBuilder(t, p.f, p.f.db)
		now := time.Now().UTC()
		previous, err := repository.NewProbeConfigStore(p.f.db).Latest(ctx, domain.LocalProbeID)
		if err != nil {
			t.Fatal(err)
		}
		meta, err := builder.Prepare(ctx, "11111111-2222-4333-8444-555555555555", previous.Revision, now, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.active.ActivateLocal(ctx, ports.LocalActivationParams{Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AppliedAt: now, AssignmentCount: -1, ExpectedActiveRevision: previous.Revision}); err != nil {
			t.Fatal(err)
		}
	}
	if round {
		want, err := p.active.GetActive(ctx, domain.LocalProbeID)
		if err != nil {
			t.Fatal(err)
		}
		reconciler, ok := p.active.(ports.LocalDeliveryPreparationRoundReconciler)
		if !ok {
			t.Fatal("activation store lacks local preparation rounds")
		}
		if err := reconciler.ReconcileLocalDeliveryPreparationRounds(ctx, *want); err != nil {
			t.Fatal(err)
		}
	}
	return operativeFixture{p: p, store: newOperativeStore(t, p.f), round: round}
}

func (o operativeFixture) claim(t *testing.T) domain.AggregateStandbyLease {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_cutover_state SET lease_until=?, due_at=?", past, past); err != nil {
		t.Fatal(err)
	}
	claims, err := o.store.ClaimCutovers(t.Context(), operativeOwner, 30*time.Second, 20)
	if err != nil || len(claims) != 1 {
		t.Fatal("claim did not select operative work", err, len(claims))
	}
	return claims[0]
}

func (o operativeFixture) run(t *testing.T, enabled bool) domain.AggregateCutoverProgress {
	t.Helper()
	got, err := o.store.ReconcileCutover(t.Context(), o.claim(t), enabled, operativeEvaluator{})
	if err != nil {
		t.Fatal("operative reconcile", err)
	}
	return got
}

type operativeStateRow struct {
	Phase     string
	BarrierID *string
	Reason    string
}

func (o operativeFixture) record(t *testing.T, status domain.Status) {
	t.Helper()
	config, err := o.p.active.GetActive(t.Context(), domain.LocalProbeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.p.heartbeats.Record(t.Context(), o.p.monitor, ports.CheckResult{Status: status, Message: "operative sample", AssignmentGeneration: 1, ConfigRevision: config.Revision}); err != nil {
		t.Fatal("record status", status, err)
	}
	awaitProbeClock(t, o.p)
}

// drain runs the disable path until the original members release. The first
// pass only enters draining; later passes issue and settle resume.
func (o operativeFixture) drain(t *testing.T) domain.AggregateCutoverProgress {
	t.Helper()
	var got domain.AggregateCutoverProgress
	for i := 0; i < 4; i++ {
		got = o.run(t, false)
		if got.Phase == "released" {
			return got
		}
	}
	return got
}

func (o operativeFixture) state(t *testing.T) operativeStateRow {
	t.Helper()
	var row operativeStateRow
	if err := o.p.f.db.NewSelect().Table("operative_cutover_state").
		Column("phase", "barrier_id", "reason").Where("monitor_id=?", o.p.monitor.ID).Scan(t.Context(), &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func (o operativeFixture) gateCount(t *testing.T, state string) int64 {
	t.Helper()
	count, err := o.p.f.db.NewSelect().Table("operative_source_gates").ColumnExpr("COUNT(*)").
		Where("monitor_id=? AND state=?", o.p.monitor.ID, state).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func (o operativeFixture) attempts(t *testing.T) []struct {
	State string
} {
	t.Helper()
	var rows []struct {
		State string
	}
	if err := o.p.f.db.NewSelect().Table("operative_provider_attempts").Column("state").
		Where("monitor_id=?", o.p.monitor.ID).Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestOperativeCutoverContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
				t.Run(string(mode), func(t *testing.T) { testOperativeLifecycle(t, engine, mode) })
			}
			t.Run("LocalProviderJournal", func(t *testing.T) { testOperativeLocalJournal(t, engine) })
			t.Run("GateSuppression", func(t *testing.T) { testOperativeGateSuppression(t, engine) })
			t.Run("ColdMembership", func(t *testing.T) { testOperativeCold(t, engine) })
			t.Run("MissingPreparation", func(t *testing.T) { testOperativeMissing(t, engine) })
			t.Run("FencesAndStageOrder", func(t *testing.T) { testOperativeFences(t, engine) })
			t.Run("DrainPendingAndStarted", func(t *testing.T) { testOperativeDrainPending(t, engine) })
			t.Run("DrainAmbiguousBlocks", func(t *testing.T) { testOperativeDrainAmbiguous(t, engine) })
			t.Run("MigrationConstraints", func(t *testing.T) { testOperativeMigration(t, engine) })
			t.Run("RemoteMembership", func(t *testing.T) { testOperativeRemote(t, engine) })
		})
	}
}

// testOperativeLifecycle drives the complete barrier for the hub-local member:
// preparing → quiescing (hold persisted) → quiesced → applying (application +
// kind receipt) → active (pure cutover evidence) → genuine aggregate incident
// and delivery → drain on opt-in disable → resume → released.
func testOperativeLifecycle(t *testing.T, engine string, mode domain.AlertDelivery) {
	o := newOperativeFixture(t, engine, mode, 1, true)
	if got := o.run(t, true); got.Phase != "quiescing" || got.BarrierID == "" {
		t.Fatalf("preparing did not append a barrier: %+v", got)
	}
	if o.gateCount(t, "held") != 1 {
		t.Fatal("local source hold was not persisted before the attempt check")
	}
	if mode == domain.AlertDeliveryAggregate && !o.suppressed(t) {
		t.Fatal("aggregate hold did not suppress local availability enqueue")
	}
	if mode == domain.AlertDeliveryBoth && o.suppressed(t) {
		t.Fatal("both-mode hold suppressed retained regional work")
	}
	if got := o.run(t, true); got.Phase != "quiesced" {
		t.Fatalf("quiescing did not reach quiesced: %+v", got)
	}
	if got := o.run(t, true); got.Phase != "applying" {
		t.Fatalf("quiesced did not issue application: %+v", got)
	}
	if got := o.run(t, true); got.Phase != "active" {
		t.Fatalf("applying did not reach active: %+v", got)
	}
	o.record(t, domain.StatusDown)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("fresh DOWN did not create genuine aggregate work")
	}
	var incidents int
	if err := o.p.f.db.NewSelect().Table("operative_aggregate_incidents").ColumnExpr("COUNT(*)").Where("monitor_id=?", o.p.monitor.ID).Scan(t.Context(), &incidents); err != nil {
		t.Fatal(err)
	}
	if incidents != 1 {
		t.Fatalf("genuine aggregate incidents = %d, want 1", incidents)
	}
	claimed, err := o.store.ClaimAggregateDelivery(t.Context(), operativeOwner, 2*time.Minute)
	if err != nil || claimed == nil {
		t.Fatal("aggregate delivery claim", err)
	}
	grant, err := o.store.AuthorizeAggregateDelivery(t.Context(), receipt(*claimed), time.Minute)
	if err != nil || grant == nil || grant.Notification == nil || grant.Monitor == nil || len(grant.Regions) != 1 {
		t.Fatal("aggregate provider grant", err, grant)
	}
	if len(o.attempts(t)) != 1 || o.attempts(t)[0].State != string(domain.ProviderAttemptInFlight) {
		t.Fatal("aggregate attempt grant not journaled in flight")
	}
	if err := o.store.FinishAggregateDelivery(t.Context(), receipt(*claimed), domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: time.Now().UTC()}); err != nil {
		t.Fatal("finish aggregate delivery", err)
	}
	if err := o.store.FinishSourceProviderAttempt(t.Context(), grant.AttemptID, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatal("finish provider attempt", err)
	}
	if got := o.run(t, false); got.Phase != "draining" {
		t.Fatalf("opt-in disable did not drain: %+v", got)
	}
	if got := o.drain(t); got.Phase != "released" {
		t.Fatalf("drain did not release original members: %+v", got)
	}
	if o.gateCount(t, "held") != 0 || o.gateCount(t, "released") != 1 {
		t.Fatal("resume did not release exactly this barrier's hold")
	}
	if o.suppressed(t) {
		t.Fatal("released gate still suppresses")
	}
}

func (o operativeFixture) suppressed(t *testing.T) bool {
	t.Helper()
	out := true
	if err := o.p.f.db.RunInTx(t.Context(), nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = repository.LocalAvailabilityEnqueueSuppressedTx(ctx, tx, o.p.monitor.ID)
		if err == nil {
			var escalation bool
			escalation, err = repository.LocalAvailabilityEscalationSuppressedTx(ctx, tx, o.p.monitor.ID)
			if escalation != out {
				t.Fatal("heartbeat and escalation suppression disagree")
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// testOperativeLocalJournal exercises the real local recording path into the
// provider journal: recorded availability work is claimed, finally authorized
// in one fenced transaction with the captured graph, and finished exactly once.
func testOperativeLocalJournal(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, "", 1, false)
	o.record(t, domain.StatusDown)
	claimed, err := o.p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), 5*time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatal("claim local availability work", err, len(claimed))
	}
	config, err := o.p.active.GetActive(ctx, domain.LocalProbeID)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := o.store.AuthorizeLocalDelivery(ctx, receipt(claimed[0]), *config, time.Minute)
	if err != nil || grant == nil || grant.Config == nil || grant.AttemptID == "" || grant.Delivery.DeliveryID != claimed[0].DeliveryID {
		t.Fatal("local provider grant", err, grant)
	}
	if got := o.attempts(t); len(got) != 1 || got[0].State != string(domain.ProviderAttemptInFlight) {
		t.Fatal("local attempt not journaled in flight", got)
	}
	// One provider budget per claim attempt.
	if _, err := o.store.AuthorizeLocalDelivery(ctx, receipt(claimed[0]), *config, time.Minute); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("duplicate provider grant accepted", err)
	}
	// A budget the stored lease cannot cover is refused.
	if _, err := o.store.AuthorizeLocalDelivery(ctx, receipt(claimed[0]), *config, 30*time.Minute); err == nil {
		t.Fatal("provider budget beyond the stored lease accepted")
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatal("finish provider attempt", err)
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatal("identical repeat finish is not a no-op", err)
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptAmbiguous); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("contradictory outcome rewrite accepted", err)
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, probeRegistryID3, domain.ProviderAttemptSucceeded); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("unknown grant accepted", err)
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptInFlight); err == nil {
		t.Fatal("unfinished state accepted as a finish")
	}
}

// testOperativeGateSuppression proves the monitor-wide hold blocks final local
// authorization and the enqueue/escalation helpers, and that a config change
// never discards the hold.
func testOperativeGateSuppression(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	o.record(t, domain.StatusDown)
	claimed, err := o.p.outbox.ClaimDeliveries(ctx, domain.LocalProbeID, time.Now().UTC(), 5*time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatal("claim local availability work", err, len(claimed))
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier did not reach quiescing: %+v", got)
	}
	if !o.suppressed(t) {
		t.Fatal("aggregate hold did not suppress enqueue/escalation")
	}
	config, err := o.p.active.GetActive(ctx, domain.LocalProbeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.store.AuthorizeLocalDelivery(ctx, receipt(claimed[0]), *config, time.Minute); !errors.Is(err, repository.ErrLocalDeliverySuppressed) {
		t.Fatal("held gate did not refuse final authorization", err)
	}
	// A config change must not discard the hold.
	replaceBindingMode(t, o.p.f, o.p.monitor.ID, 2, domain.LocalProbeID, domain.AlertDeliveryAggregate)
	if !o.suppressed(t) {
		t.Fatal("config change erased the monitor-wide hold")
	}
}

func testOperativeCold(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, false)
	if got := o.run(t, true); got.Phase != "preparing" || got.Changed {
		t.Fatalf("cold membership fabricated progress: %+v", got)
	}
	if count := o.barrierCount(t); count != 0 {
		t.Fatalf("cold membership created %d barriers, want 0", count)
	}
}

func testOperativeMissing(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	if _, err := o.p.f.db.ExecContext(t.Context(), "DELETE FROM probe_delivery_preparation_requests WHERE monitor_id=?", o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "preparing" || got.Changed {
		t.Fatalf("missing preparation fabricated progress: %+v", got)
	}
	if count := o.barrierCount(t); count != 0 {
		t.Fatalf("missing preparation created %d barriers, want 0", count)
	}
}

func (o operativeFixture) barrierCount(t *testing.T) int64 {
	t.Helper()
	count, err := o.p.f.db.NewSelect().Table("operative_cutover_members").ColumnExpr("COUNT(*)").Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// testOperativeFences proves independent DB-clock claims: current owner/epoch,
// expiry, clock rollback, concurrent claims, stage order (no apply before the
// full quiesced barrier) and an assignment ABA that forces drain with no rebind.
func testOperativeFences(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	first := o.run(t, true)
	if first.Phase != "quiescing" {
		t.Fatalf("barrier did not reach quiescing: %+v", first)
	}
	// Stage order: application commands exist only after a full quiesced barrier.
	var kinds []string
	if err := o.p.f.db.NewSelect().Table("operative_cutover_commands").Column("kind").OrderExpr("kind ASC").Scan(t.Context(), &kinds); err != nil {
		t.Fatal(err)
	}
	for _, kind := range kinds {
		if kind != domain.DeliveryQuiesceKind {
			t.Fatalf("stage order issued %s before quiesced", kind)
		}
	}
	lease := o.claim(t)
	if _, err := o.store.ReconcileCutover(t.Context(), domain.AggregateStandbyLease{MonitorID: lease.MonitorID, OwnerID: probeRegistryID3, Epoch: lease.Epoch, Until: lease.Until}, true, operativeEvaluator{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("foreign owner reconciled stored work", err)
	}
	// Expiry: the stored lease, not a caller clock, fences authority.
	expired := o.claim(t)
	if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_cutover_state SET lease_until=? WHERE monitor_id=?", time.Now().UTC().Add(-time.Minute), o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.store.ReconcileCutover(t.Context(), expired, true, operativeEvaluator{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("expired lease reconciled stored work", err)
	}
	// Clock rollback supplies no new permit.
	if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_cutover_state SET evaluated_at=? WHERE monitor_id=?", time.Now().UTC().Add(time.Hour), o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.store.ReconcileCutover(t.Context(), o.claim(t), true, operativeEvaluator{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("backward DB clock admitted new work", err)
	}
	if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_cutover_state SET evaluated_at=NULL WHERE monitor_id=?", o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	// Concurrent claimers cannot share the stored lease.
	current := o.claim(t)
	extra, err := o.store.ClaimCutovers(t.Context(), operativeOwner, 30*time.Second, 20)
	if err != nil || len(extra) != 0 {
		t.Fatal("second claimer stole a live lease", err, len(extra))
	}
	next := o.claim(t)
	if next.Epoch <= current.Epoch {
		t.Fatal("reacquisition did not advance the epoch")
	}
	if _, err := o.store.ReconcileCutover(t.Context(), current, true, operativeEvaluator{}); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale epoch reconciled stored work", err)
	}
	// Assignment ABA stops admission and forces drain with no rebind.
	replaceBindingMode(t, o.p.f, o.p.monitor.ID, 2, domain.LocalProbeID, domain.AlertDeliveryBoth)
	got := o.run(t, true)
	if got.Phase != "draining" {
		t.Fatalf("ABA did not force drain: %+v", got)
	}
}

// testOperativeDrainPending drains with one started (granted) and one unstarted
// plan: only the unstarted plan is superseded, and resume is issued only after
// the admitted grant resolves.
func testOperativeDrainPending(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 2, true)
	for _, phase := range []string{"quiescing", "quiesced", "applying", "active"} {
		if got := o.run(t, true); got.Phase != phase {
			t.Fatalf("lifecycle stage %s, got %+v", phase, got)
		}
	}
	o.record(t, domain.StatusDown)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("genuine aggregate work not admitted")
	}
	started, err := o.store.ClaimAggregateDelivery(ctx, operativeOwner, 2*time.Minute)
	if err != nil || started == nil {
		t.Fatal("aggregate claim", err)
	}
	grant, err := o.store.AuthorizeAggregateDelivery(ctx, receipt(*started), time.Minute)
	if err != nil || grant == nil {
		t.Fatal("aggregate grant", err)
	}
	if got := o.run(t, false); got.Phase != "draining" {
		t.Fatalf("disable did not drain: %+v", got)
	}
	o.run(t, false) // The drain pass supersedes unstarted work and blocks on I/O.
	statuses := o.deliveryStatuses(t)
	if len(statuses) != 2 || statuses[domain.DeliveryStatusSuperseded] != 1 || statuses[domain.DeliveryStatusLeased] != 1 {
		t.Fatalf("drain rewrote started work or kept unstarted work: %v", statuses)
	}
	if o.resumeIssued(t) {
		t.Fatal("resume issued while a provider grant is in flight")
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatal(err)
	}
	var got domain.AggregateCutoverProgress
	for i := 0; i < 4 && !o.resumeIssued(t); i++ {
		got = o.run(t, false)
	}
	if !o.resumeIssued(t) {
		t.Fatalf("resolved grant did not resume original members: %+v", got)
	}
	if got := o.drain(t); got.Phase != "released" {
		t.Fatalf("drain did not release: %+v", got)
	}
}

// testOperativeDrainAmbiguous proves an ambiguous provider outcome blocks
// resume indefinitely rather than being fabricated into completion.
func testOperativeDrainAmbiguous(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	for _, phase := range []string{"quiescing", "quiesced", "applying", "active"} {
		if got := o.run(t, true); got.Phase != phase {
			t.Fatalf("lifecycle stage %s, got %+v", phase, got)
		}
	}
	o.record(t, domain.StatusDown)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("genuine aggregate work not admitted")
	}
	started, err := o.store.ClaimAggregateDelivery(ctx, operativeOwner, 2*time.Minute)
	if err != nil || started == nil {
		t.Fatal("aggregate claim", err)
	}
	grant, err := o.store.AuthorizeAggregateDelivery(ctx, receipt(*started), time.Minute)
	if err != nil || grant == nil {
		t.Fatal("aggregate grant", err)
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptAmbiguous); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, false); got.Phase != "draining" {
		t.Fatalf("disable did not drain: %+v", got)
	}
	if got := o.run(t, false); got.Phase != "draining" || o.resumeIssued(t) {
		t.Fatalf("ambiguous grant did not block resume: %+v", got)
	}
}

func (o operativeFixture) deliveryStatuses(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	var rows []string
	if err := o.p.f.db.NewSelect().Table("operative_aggregate_deliveries").Column("status").Where("monitor_id=?", o.p.monitor.ID).Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, status := range rows {
		out[status]++
	}
	return out
}

func (o operativeFixture) resumeIssued(t *testing.T) bool {
	t.Helper()
	count, err := o.p.f.db.NewSelect().Table("operative_cutover_commands").ColumnExpr("COUNT(*)").
		Where("kind = ?", domain.DeliveryResumeKind).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return count > 0
}

// testOperativeMigration asserts schema constraints, the downgrade guard that
// refuses to erase unresolved authority, and retention of gate/journal history
// across monitor deletion.
func testOperativeMigration(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier did not reach quiescing: %+v", got)
	}
	if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_cutover_state SET phase='bogus' WHERE monitor_id=?", o.p.monitor.ID); err == nil {
		t.Fatal("schema permitted an unknown barrier phase")
	}
	if _, err := o.p.f.db.ExecContext(t.Context(), "UPDATE operative_source_gates SET state='bogus'"); err == nil {
		t.Fatal("schema permitted an unknown source-hold state")
	}
	if _, err := o.p.f.db.ExecContext(t.Context(), "INSERT INTO operative_provider_attempts (attempt_id,scope,monitor_id,probe_id,delivery_id,claim_attempt,notification_id,settings_sha256,budget_until,state,created_at) VALUES ('44444444-4444-4444-8444-444444444444','source',?,'local','55555555-5555-4555-8555-555555555555',0,0,'',?, 'in_flight', ?)", o.p.monitor.ID, time.Now().UTC(), time.Now().UTC()); err == nil {
		t.Fatal("schema permitted a zero claim attempt")
	}
	// The downgrade guard must fail loudly while authority is unresolved.
	if err := runEngineMigration(t, o.p.f.db, engine, "082_operative_cutover", "down"); err == nil {
		t.Fatal("downgrade erased a held source hold")
	}
	for _, phase := range []string{"quiesced", "applying", "active"} {
		if got := o.run(t, true); got.Phase != phase {
			t.Fatalf("lifecycle stage %s, got %+v", phase, got)
		}
	}
	if got := o.run(t, false); got.Phase != "draining" {
		t.Fatalf("drain stage, got %+v", got)
	}
	if got := o.drain(t); got.Phase != "released" {
		t.Fatalf("release stage, got %+v", got)
	}
	// Gate and journal history survive monitor deletion: unresolved authority
	// must never be erased by a cascading delete.
	if _, err := o.p.f.db.ExecContext(t.Context(), "DELETE FROM monitors WHERE id=?", o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if o.gateCount(t, "released") != 1 {
		t.Fatal("monitor deletion erased source-hold history")
	}
	var barriers int
	if err := o.p.f.db.NewSelect().Table("operative_cutover_barriers").ColumnExpr("COUNT(*)").Scan(t.Context(), &barriers); err != nil {
		t.Fatal(err)
	}
	if barriers != 1 {
		t.Fatal("monitor deletion erased barrier history")
	}
	if err := runEngineMigration(t, o.p.f.db, engine, "082_operative_cutover", "down"); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, o.p.f.db, engine, "082_operative_cutover", "up"); err != nil {
		t.Fatal(err)
	}
}

// operativeRemoteFixture drives the remote member path through the real
// retained document/ACK/regional-report/preparation machinery.
type operativeRemoteFixture struct {
	f     preparationFixture
	store *repository.OperativeDeliveryStore
}

func newOperativeRemoteFixture(t *testing.T, engine string) operativeRemoteFixture {
	t.Helper()
	f := newPreparationFixture(t, engine)
	fact := f.rebuildGraph(t, domain.AlertDeliveryAggregate)
	if err := f.issue(t, fact); err != nil {
		t.Fatal(err)
	}
	// The shared preparation must succeed exactly before a barrier may bind it.
	commands := f.commands(t)
	claimed, err := commands.ClaimCommand(t.Context(), f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true})
	if err != nil || claimed == nil || claimed.Kind != "delivery.prepare" {
		t.Fatal("preparation request not claimable", err, claimed)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: claimed.CommandID, Status: "applied", AppliedAt: &now, Code: "delivery_prepared"}); err != nil {
		t.Fatal(err)
	}
	return operativeRemoteFixture{f: f, store: newOperativeStore(t, f.f)}
}

func (o operativeRemoteFixture) claim(t *testing.T) domain.AggregateStandbyLease {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := o.f.f.db.ExecContext(t.Context(), "UPDATE operative_cutover_state SET lease_until=?, due_at=?", past, past); err != nil {
		t.Fatal(err)
	}
	claims, err := o.store.ClaimCutovers(t.Context(), operativeOwner, 30*time.Second, 20)
	if err != nil || len(claims) != 1 {
		t.Fatal("claim did not select operative work", err, len(claims))
	}
	return claims[0]
}

func (o operativeRemoteFixture) run(t *testing.T, enabled bool) domain.AggregateCutoverProgress {
	t.Helper()
	got, err := o.store.ReconcileCutover(t.Context(), o.claim(t), enabled, operativeEvaluator{})
	if err != nil {
		t.Fatal("operative reconcile", err)
	}
	return got
}

func (o operativeRemoteFixture) remoteResumes(t *testing.T) int64 {
	t.Helper()
	var resumes int64
	if err := o.f.f.db.NewSelect().Table("operative_cutover_commands").ColumnExpr("COUNT(*)").Where("kind=?", domain.DeliveryResumeKind).Scan(t.Context(), &resumes); err != nil {
		t.Fatal(err)
	}
	return resumes
}

func (o operativeRemoteFixture) complete(t *testing.T, kind string) {
	t.Helper()
	commands := o.f.commands(t)
	var rows []struct {
		CommandID string
	}
	if err := o.f.f.db.NewSelect().Table("operative_cutover_commands").Column("command_id").
		Where("kind = ? AND outcome_status = ''", kind).Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("open %s commands = %d, want 1", kind, len(rows))
	}
	claimed, err := commands.ClaimCommand(t.Context(), o.f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryCutover: true})
	if err != nil || claimed == nil || claimed.Kind != kind {
		t.Fatal("cutover command not claimable", err, claimed)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	code := domain.DeliveryCutoverSuccessCode(kind)
	if err := commands.CompleteCommand(t.Context(), o.f.session, domain.ProbeCommandOutcome{CommandID: claimed.CommandID, Status: "applied", AppliedAt: &now, Code: code}); err != nil {
		t.Fatal(err)
	}
}

// testOperativeRemote covers unsupported versus compiled-strong-capability
// membership, the per-peer command kind filter, exact remote receipts, and
// offline members that are never dropped from the barrier.
func testOperativeRemote(t *testing.T, engine string) {
	o := newOperativeRemoteFixture(t, engine)
	// Without the compiled strong capability the membership is unsupported and
	// no barrier may be created.
	if got := o.run(t, true); got.Phase != "preparing" || got.Changed {
		t.Fatalf("unsupported membership fabricated progress: %+v", got)
	}
	if count, err := o.f.f.db.NewSelect().Table("operative_cutover_members").ColumnExpr("COUNT(*)").Count(t.Context()); err != nil || count != 0 {
		t.Fatal("unsupported membership created a barrier", err, count)
	}
	if err := o.store.SetOperativeSourceCapability(t.Context(), o.f.session, true); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("compiled capability did not admit the member: %+v", got)
	}
	// delivery.prepare support alone must never substitute for the cutover
	// command capability.
	commands := o.f.commands(t)
	if stray, err := commands.ClaimCommand(t.Context(), o.f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryPreparation: true}); err != nil || stray != nil {
		t.Fatal("preparation capability substituted for cutover dispatch", err, stray)
	}
	o.complete(t, domain.DeliveryQuiesceKind)
	if got := o.run(t, true); got.Phase != "quiesced" {
		t.Fatalf("exact quiesce receipt did not quiesce: %+v", got)
	}
	if got := o.run(t, true); got.Phase != "applying" {
		t.Fatalf("quiesced did not issue application: %+v", got)
	}
	o.complete(t, domain.DeliveryApplyKind)
	if got := o.run(t, true); got.Phase != "active" {
		t.Fatalf("exact application receipt did not activate: %+v", got)
	}
	// An offline member is still drain membership: the stored accepted stream
	// survives a disconnect, and resume must be issued for the original tuple.
	if _, err := o.f.f.db.ExecContext(t.Context(), "UPDATE probe_connections SET state='prepared' WHERE probe_id=?", o.f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, false); got.Phase != "draining" {
		t.Fatalf("disable did not drain: %+v", got)
	}
	for i := 0; i < 3; i++ {
		if o.remoteResumes(t) > 0 {
			break
		}
		o.run(t, false)
	}
	var resumes int64
	if err := o.f.f.db.NewSelect().Table("operative_cutover_commands").ColumnExpr("COUNT(*)").Where("kind=?", domain.DeliveryResumeKind).Scan(t.Context(), &resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 1 {
		t.Fatalf("offline member dropped from drain membership: %d resume commands", resumes)
	}
	if err := o.store.SetOperativeSourceCapability(t.Context(), o.f.session, false); err != nil {
		t.Fatal(err)
	}
}
