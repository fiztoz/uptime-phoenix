package repository_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestOperativeRefinementContract covers the hub-side operative refinements:
// conservative evidence adoption at the barrier activation boundary (standby
// never prevents a genuine operative open), genuine repeat/resend/throttle with
// digest-only queued plans, opt-in disable draining in-flight barriers with
// queue withdrawal at resume, and the exact rearmable busy refusal code versus
// corruption. All assertions go through the production entry points.
func TestOperativeRefinementContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("AdoptionFutureReceipt", func(t *testing.T) { testAdoptionFutureReceipt(t, engine) })
			t.Run("AdoptionReceiptBoundary", func(t *testing.T) { testAdoptionReceiptBoundary(t, engine) })
			t.Run("AdoptionObservationBoundary", func(t *testing.T) { testAdoptionObservationBoundary(t, engine) })
			t.Run("StandbyNeverPreventsOperativeOpen", func(t *testing.T) { testStandbyNeverPreventsOperativeOpen(t, engine) })
			t.Run("RepeatThrottle", func(t *testing.T) { testRepeatThrottle(t, engine) })
			t.Run("DigestOnlyPlans", func(t *testing.T) { testDigestOnlyPlans(t, engine) })
			t.Run("DigestMismatchRefused", func(t *testing.T) { testDigestMismatchRefused(t, engine) })
			t.Run("DisableDrainsQuiescing", func(t *testing.T) { testDisableDrainsPhase(t, engine, "quiescing") })
			t.Run("DisableDrainsQuiesced", func(t *testing.T) { testDisableDrainsPhase(t, engine, "quiesced") })
			t.Run("DisableDrainsApplying", func(t *testing.T) { testDisableDrainsPhase(t, engine, "applying") })
			t.Run("ResumeWithdrawsQueuedPhase", func(t *testing.T) { testResumeWithdrawsQueuedPhase(t, engine) })
			t.Run("BusyCodeAlignsAndRearms", func(t *testing.T) { testBusyCodeAlignsAndRearms(t, engine) })
			t.Run("RemoteBusyRearmsFromReceipt", func(t *testing.T) { testRemoteBusyRearmsFromReceipt(t, engine) })
			t.Run("CorruptionBlocks", func(t *testing.T) { testCorruptionBlocks(t, engine) })
		})
	}
}

// operativeIntentRow is the plan history one incident carries.
type operativeIntentRow struct {
	Kind   string
	Status string
	Reason string
}

func (o operativeFixture) intents(t *testing.T) []operativeIntentRow {
	t.Helper()
	var rows []operativeIntentRow
	if err := o.p.f.db.NewSelect().Table("operative_aggregate_intents").
		Column("kind", "status", "reason").Where("monitor_id=?", o.p.monitor.ID).
		OrderExpr("kind ASC").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func (o operativeFixture) transitionVersion(t *testing.T) int64 {
	t.Helper()
	var version int64
	if err := o.p.f.db.NewSelect().Table("operative_cutover_state").Column("transition_version").
		Where("monitor_id=?", o.p.monitor.ID).Scan(t.Context(), &version); err != nil {
		t.Fatal(err)
	}
	return version
}

func (o operativeFixture) openIncidents(t *testing.T) int64 {
	t.Helper()
	count, err := o.p.f.db.NewSelect().Table("operative_aggregate_incidents").ColumnExpr("COUNT(*)").
		Where("monitor_id=? AND status='open'", o.p.monitor.ID).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func (o operativeFixture) activatedAt(t *testing.T) time.Time {
	t.Helper()
	var at time.Time
	if err := o.p.f.db.NewSelect().Table("operative_cutover_barriers").Column("activated_at").
		Where("monitor_id=?", o.p.monitor.ID).Scan(t.Context(), &at); err != nil {
		t.Fatal(err)
	}
	return at.UTC()
}

// setEvidenceClocks rewrites one member's stored observation/receipt clocks so
// the conservative adoption boundary can be exercised exactly.
func (o operativeFixture) setEvidenceClocks(t *testing.T, observedAt, receivedAt time.Time) {
	t.Helper()
	if _, err := o.p.f.db.ExecContext(t.Context(),
		"UPDATE monitor_probe_state SET observed_at = ?, received_at = ? WHERE monitor_id = ?",
		observedAt.UTC(), receivedAt.UTC(), o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
}

func (o operativeFixture) quiesceLog(t *testing.T) []struct {
	Status string
	Code   string
} {
	t.Helper()
	var rows []struct {
		Status string
		Code   string
	}
	if err := o.p.f.db.NewSelect().Table("operative_cutover_commands").
		ColumnExpr("outcome_status AS status, outcome_code AS code").Where("kind = ?", domain.DeliveryQuiesceKind).
		OrderExpr("issued_at ASC").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func backdateOperativeOutcomes(t *testing.T, db *bun.DB, kind string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		"UPDATE operative_cutover_commands SET outcome_at = ? WHERE kind = ?",
		time.Now().UTC().Add(-2*time.Minute), kind); err != nil {
		t.Fatal(err)
	}
}

func countTable(t *testing.T, db *bun.DB, table, column, value string) int64 {
	t.Helper()
	count, err := db.NewSelect().Table(table).ColumnExpr("COUNT(*)").
		Where(column+" = ?", value).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// driveOperativeActive walks the full barrier lifecycle and returns the
// activation boundary.
func driveOperativeActive(t *testing.T, o operativeFixture) time.Time {
	t.Helper()
	for _, phase := range []string{"quiescing", "quiesced", "applying", "active"} {
		if got := o.run(t, true); got.Phase != phase {
			t.Fatalf("lifecycle stage %s, got %+v", phase, got)
		}
	}
	return o.activatedAt(t)
}

// testAdoptionFutureReceipt proves a receipt from the future (clock skew) is
// never adopted: it cannot open an incident, while the same sample with its
// real receipt opens the genuine operative incident.
func testAdoptionFutureReceipt(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	activated := driveOperativeActive(t, o)
	o.record(t, domain.StatusDown)
	o.setEvidenceClocks(t, activated.Add(time.Millisecond), activated.Add(time.Hour))
	if got := o.run(t, true); got.Changed {
		t.Fatal("future receipt established an operative transition")
	}
	if n := o.openIncidents(t); n != 0 {
		t.Fatalf("future receipt opened %d incidents, want 0", n)
	}
	o.setEvidenceClocks(t, activated.Add(time.Millisecond), activated.Add(2*time.Millisecond))
	if got := o.run(t, true); !got.Changed {
		t.Fatal("fresh post-activation sample did not open the genuine incident")
	}
	if n := o.openIncidents(t); n != 1 {
		t.Fatalf("genuine operative incidents = %d, want 1", n)
	}
}

// testAdoptionReceiptBoundary proves a receipt at or before the activation
// boundary is never adopted even when the observation itself is post-activation.
func testAdoptionReceiptBoundary(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	activated := driveOperativeActive(t, o)
	o.record(t, domain.StatusDown)
	o.setEvidenceClocks(t, activated.Add(time.Millisecond), activated.Add(-time.Second))
	if got := o.run(t, true); got.Changed {
		t.Fatal("pre-activation receipt established an operative transition")
	}
	if n := o.openIncidents(t); n != 0 {
		t.Fatalf("pre-activation receipt opened %d incidents, want 0", n)
	}
	o.setEvidenceClocks(t, activated.Add(time.Millisecond), activated.Add(2*time.Millisecond))
	if got := o.run(t, true); !got.Changed {
		t.Fatal("post-activation receipt did not open the genuine incident")
	}
	if n := o.openIncidents(t); n != 1 {
		t.Fatalf("genuine operative incidents = %d, want 1", n)
	}
}

// testAdoptionObservationBoundary proves an observation at or before the
// activation boundary is never adopted even with a post-activation receipt.
func testAdoptionObservationBoundary(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	activated := driveOperativeActive(t, o)
	o.record(t, domain.StatusDown)
	o.setEvidenceClocks(t, activated.Add(-time.Second), activated.Add(time.Millisecond))
	if got := o.run(t, true); got.Changed {
		t.Fatal("pre-activation observation established an operative transition")
	}
	if n := o.openIncidents(t); n != 0 {
		t.Fatalf("pre-activation observation opened %d incidents, want 0", n)
	}
	o.setEvidenceClocks(t, activated.Add(time.Millisecond), activated.Add(2*time.Millisecond))
	if got := o.run(t, true); !got.Changed {
		t.Fatal("post-activation observation did not open the genuine incident")
	}
	if n := o.openIncidents(t); n != 1 {
		t.Fatalf("genuine operative incidents = %d, want 1", n)
	}
}

// testStandbyNeverPreventsOperativeOpen drives the real standby planner to an
// open standby incident first and then proves the operative path still opens
// its own genuine incident: standby state never gates operative admission.
func testStandbyNeverPreventsOperativeOpen(t *testing.T, engine string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	ctx := t.Context()
	store := repository.NewAggregateStandbyStore(o.p.f.db)
	svc := services.NewAggregateStandbyService(store, standbyOwner)
	if _, err := svc.ProcessBatch(ctx); err != nil {
		t.Fatal(err)
	}
	// The first standby pass establishes its own adoption boundary.
	standbyClaimRun(t, o.p.f.db, store, svc, o.p.monitor.ID)
	o.record(t, domain.StatusDown)
	standbyClaimRun(t, o.p.f.db, store, svc, o.p.monitor.ID)
	if n := standbyOpenIncidents(t, o.p.f.db, o.p.monitor.ID); n != 1 {
		t.Fatalf("standby open incidents = %d, want 1", n)
	}
	driveOperativeActive(t, o)
	o.record(t, domain.StatusDown)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("a standby open prevented the genuine operative open")
	}
	if n := o.openIncidents(t); n != 1 {
		t.Fatalf("operative incidents despite standby open = %d, want 1", n)
	}
	if n := standbyOpenIncidents(t, o.p.f.db, o.p.monitor.ID); n != 1 {
		t.Fatalf("standby incidents after operative open = %d, want 1", n)
	}
}

func standbyClaimRun(t *testing.T, db *bun.DB, store *repository.AggregateStandbyStore, svc *services.AggregateStandbyService, monitorID int64) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := db.ExecContext(t.Context(), "UPDATE aggregate_standby_state SET lease_until=?, due_at=? WHERE monitor_id=?", past, past, monitorID); err != nil {
		t.Fatal(err)
	}
	claims, err := store.ClaimStandby(t.Context(), standbyOwner, 30*time.Second, 20)
	if err != nil || len(claims) != 1 {
		t.Fatal("standby claim did not select current work", err, len(claims))
	}
	if _, err := store.ReconcileStandby(t.Context(), claims[0], svc); err != nil {
		t.Fatal("standby reconcile", err)
	}
}

func standbyOpenIncidents(t *testing.T, db *bun.DB, monitorID int64) int64 {
	t.Helper()
	count, err := db.NewSelect().Table("aggregate_standby_incidents").ColumnExpr("COUNT(*)").
		Where("monitor_id=? AND status='open'", monitorID).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// testRepeatThrottle proves the genuine repeat/resend feature: a fresh DOWN
// repeats only after the durable last-planned admission plus the resend
// interval, the repeat supersedes obsolete unstarted work, and UNKNOWN health
// neither repeats nor recovers.
func testRepeatThrottle(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	if _, err := o.p.f.db.ExecContext(ctx, "UPDATE monitors SET resend_interval = 1 WHERE id = ?", o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := refreshService(t, o.p.f, o.p.f.db).Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.p.active.(ports.LocalDeliveryPreparationRoundReconciler).ReconcileLocalDeliveryPreparationRounds(ctx, *fresh); err != nil {
		t.Fatal(err)
	}
	activated := driveOperativeActive(t, o)
	o.record(t, domain.StatusDown)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("fresh DOWN did not open the genuine incident")
	}
	if got := o.transitionVersion(t); got != 1 {
		t.Fatalf("open transition version = %d, want 1", got)
	}
	if rows := o.intents(t); len(rows) != 1 || rows[0].Kind != "down" || rows[0].Status != "pending" {
		t.Fatalf("open plans = %+v, want one pending down", rows)
	}
	// Throttled before the durable deadline.
	if got := o.run(t, true); got.Changed {
		t.Fatal("repeat admitted before the resend interval")
	}
	if rows := o.intents(t); len(rows) != 1 {
		t.Fatalf("throttled repeat changed plans: %+v", rows)
	}
	// The queued intents are the durable last-planned record.
	if _, err := o.p.f.db.ExecContext(ctx, "UPDATE operative_aggregate_intents SET created_at = ? WHERE monitor_id = ?",
		time.Now().UTC().Add(-2*time.Minute), o.p.monitor.ID); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); !got.Changed {
		t.Fatal("repeat not admitted after the resend interval")
	}
	if got := o.transitionVersion(t); got != 2 {
		t.Fatalf("repeat transition version = %d, want 2", got)
	}
	rows := o.intents(t)
	if len(rows) != 2 || rows[0].Kind != "down" || rows[0].Status != "superseded" || rows[1].Kind != "repeat" || rows[1].Status != "pending" {
		t.Fatalf("repeat plans = %+v, want superseded down plus pending repeat", rows)
	}
	statuses := o.deliveryStatuses(t)
	if len(statuses) != 2 || statuses[domain.DeliveryStatusSuperseded] != 1 || statuses[domain.DeliveryStatusPending] != 1 {
		t.Fatalf("repeat did not supersede obsolete unstarted work: %v", statuses)
	}
	// UNKNOWN health neither repeats nor recovers.
	o.setEvidenceClocks(t, activated.Add(time.Millisecond), activated.Add(time.Hour))
	if got := o.run(t, true); got.Changed {
		t.Fatal("UNKNOWN health transitioned an open incident")
	}
	if n := o.openIncidents(t); n != 1 {
		t.Fatalf("UNKNOWN health closed the incident: %d open", n)
	}
	if rows := o.intents(t); len(rows) != 2 || rows[1].Kind != "repeat" {
		t.Fatalf("UNKNOWN health planned work: %+v", rows)
	}
	// A fresh DOWN is still throttled from the repeat's own planning time.
	o.setEvidenceClocks(t, activated.Add(time.Millisecond), activated.Add(2*time.Millisecond))
	if got := o.run(t, true); got.Changed {
		t.Fatal("repeat admitted before the new interval")
	}
}

// testDigestOnlyPlans proves the queue never carries plaintext provider
// settings and that the grant is the current channel read under the digest
// proof.
func testDigestOnlyPlans(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	driveOperativeActive(t, o)
	o.record(t, domain.StatusDown)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("fresh DOWN did not open the genuine incident")
	}
	var row struct {
		NotificationID       int64
		SettingsSHA256       string
		NotificationSnapshot string
		TemplateSnapshot     *string
	}
	if err := o.p.f.db.NewSelect().Table("operative_aggregate_intents").
		Column("notification_id", "settings_sha256", "notification_snapshot", "template_snapshot").
		Where("monitor_id=?", o.p.monitor.ID).Scan(ctx, &row); err != nil {
		t.Fatal(err)
	}
	var digest struct {
		ID             int64  `json:"id"`
		Type           string `json:"type"`
		SettingsSHA256 string `json:"settings_sha256"`
	}
	if err := json.Unmarshal([]byte(row.NotificationSnapshot), &digest); err != nil {
		t.Fatal("queued plan is not a digest snapshot", err)
	}
	if digest.ID != row.NotificationID || digest.Type != "webhook" || digest.SettingsSHA256 != row.SettingsSHA256 {
		t.Fatalf("digest snapshot does not match the planned identity: %+v", digest)
	}
	if strings.Contains(row.NotificationSnapshot, "example.com") || strings.Contains(row.NotificationSnapshot, `"config"`) {
		t.Fatal("plaintext provider settings entered the queue:", row.NotificationSnapshot)
	}
	if row.TemplateSnapshot != nil && *row.TemplateSnapshot != "" {
		t.Fatal("template settings entered the queue:", *row.TemplateSnapshot)
	}
	claimed, err := o.store.ClaimAggregateDelivery(ctx, operativeOwner, 2*time.Minute)
	if err != nil || claimed == nil {
		t.Fatal("aggregate delivery claim", err)
	}
	grant, err := o.store.AuthorizeAggregateDelivery(ctx, receipt(*claimed), time.Minute)
	if err != nil || grant == nil || grant.Notification == nil {
		t.Fatal("aggregate provider grant", err)
	}
	if grant.Notification.Type != "webhook" {
		t.Fatalf("grant channel type = %q, want webhook", grant.Notification.Type)
	}
	if url, _ := grant.Notification.Config["url"].(string); url != "https://example.com" {
		t.Fatal("grant did not read the current channel under the digest proof", grant.Notification.Config)
	}
}

// testDigestMismatchRefused proves a channel whose settings changed after the
// plan can never be granted: the digest proof fails and the obsolete plan is
// superseded rather than sent with drifted credentials.
func testDigestMismatchRefused(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	driveOperativeActive(t, o)
	o.record(t, domain.StatusDown)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("fresh DOWN did not open the genuine incident")
	}
	claimed, err := o.store.ClaimAggregateDelivery(ctx, operativeOwner, 2*time.Minute)
	if err != nil || claimed == nil {
		t.Fatal("aggregate delivery claim", err)
	}
	var notificationID int64
	if err := o.p.f.db.NewSelect().Table("notifications").Column("id").Limit(1).Scan(ctx, &notificationID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.p.f.db.ExecContext(ctx, "UPDATE notifications SET config = ? WHERE id = ?",
		`{"url":"https://changed.example"}`, notificationID); err != nil {
		t.Fatal(err)
	}
	if _, err := o.store.AuthorizeAggregateDelivery(ctx, receipt(*claimed), time.Minute); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("drifted channel settings were granted", err)
	}
	if rows := o.intents(t); len(rows) != 1 || rows[0].Status != "superseded" || rows[0].Reason != "channel_changed" {
		t.Fatalf("drifted plan not superseded as unstarted work: %+v", rows)
	}
}

// testDisableDrainsPhase proves a disabled opt-in drains a quiescing/quiesced/
// applying barrier instead of progressing it: no new apply work, no apply
// success, no activation, and the original members release in the drain.
func testDisableDrainsPhase(t *testing.T, engine string, phase string) {
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
	for _, stage := range []string{"quiescing", "quiesced", "applying"} {
		if got := o.run(t, true); got.Phase != stage {
			t.Fatalf("lifecycle stage %s, got %+v", stage, got)
		}
		if stage == phase {
			break
		}
	}
	wantApplies := countTable(t, o.p.f.db, "operative_cutover_commands", "kind", domain.DeliveryApplyKind)
	if got := o.run(t, false); got.Phase != "draining" {
		t.Fatalf("disabled opt-in did not drain the %s barrier: %+v", phase, got)
	}
	if reason := o.state(t).Reason; reason != "feature_disabled" {
		t.Fatalf("drain reason = %q, want feature_disabled", reason)
	}
	if got := countTable(t, o.p.f.db, "operative_cutover_commands", "kind", domain.DeliveryApplyKind); got != wantApplies {
		t.Fatalf("apply commands = %d after disable, want %d (no new apply work)", got, wantApplies)
	}
	if phase == "applying" {
		var appliedAt *time.Time
		if err := o.p.f.db.NewSelect().Table("operative_cutover_members").Column("applied_at").
			Where("monitor_id=?", o.p.monitor.ID).Scan(t.Context(), &appliedAt); err != nil {
			t.Fatal(err)
		}
		if appliedAt != nil {
			t.Fatal("apply success recorded after the opt-in was disabled")
		}
	}
	if got := o.drain(t); got.Phase != "released" {
		t.Fatalf("drain did not release the %s barrier: %+v", phase, got)
	}
	if o.gateCount(t, "held") != 0 || o.gateCount(t, "released") != 1 {
		t.Fatal("drain did not release exactly this barrier's hold")
	}
}

func (o operativeRemoteFixture) barrierState(t *testing.T) (string, string) {
	t.Helper()
	var row struct {
		Phase  string
		Reason string
	}
	if err := o.f.f.db.NewSelect().Table("operative_cutover_state").Column("phase", "reason").
		Where("monitor_id = ?", o.f.monitor).Scan(t.Context(), &row); err != nil {
		t.Fatal(err)
	}
	return row.Phase, row.Reason
}

// testResumeWithdrawsQueuedPhase proves an old queued phase command can never
// re-hold a source after its resume: the queued request is withdrawn at resume
// issuance (canceled, unclaimable) and its late receipt is refused.
func testResumeWithdrawsQueuedPhase(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeRemoteFixture(t, engine)
	if err := o.store.SetOperativeSourceCapability(ctx, o.f.session, true); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("compiled capability did not admit the member: %+v", got)
	}
	commands := o.f.commands(t)
	claimed, err := commands.ClaimCommand(ctx, o.f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryCutover: true})
	if err != nil || claimed == nil || claimed.Kind != domain.DeliveryQuiesceKind {
		t.Fatal("queued phase command not claimable", err, claimed)
	}
	quiesceID := claimed.CommandID
	if got := o.run(t, false); got.Phase != "draining" {
		t.Fatalf("disable did not drain the in-flight barrier: %+v", got)
	}
	for i := 0; i < 3 && o.remoteResumes(t) == 0; i++ {
		o.run(t, false)
	}
	if o.remoteResumes(t) != 1 {
		t.Fatal("resume not issued for the queued member")
	}
	var row struct {
		Status        string
		NextAttemptAt *time.Time
	}
	if err := o.f.f.db.NewSelect().Table("probe_commands").Column("status", "next_attempt_at").
		Where("command_id = ?", quiesceID).Scan(ctx, &row); err != nil {
		t.Fatal(err)
	}
	if row.Status != "canceled" || row.NextAttemptAt != nil {
		t.Fatalf("old phase command still queued after resume: %+v", row)
	}
	// A late phase receipt can never re-hold after the resume.
	now := time.Now().UTC().Truncate(time.Microsecond)
	late := domain.ProbeCommandOutcome{CommandID: quiesceID, Status: "applied", AppliedAt: &now, Code: domain.DeliveryQuiescedCode}
	if err := commands.CompleteCommand(ctx, o.f.session, late); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("late phase receipt re-held after resume", err)
	}
	next, err := commands.ClaimCommand(ctx, o.f.session, time.Second, domain.ProbeCommandCapabilities{DeliveryCutover: true})
	if err != nil || next == nil || next.Kind != domain.DeliveryResumeKind {
		t.Fatal("withdrawn phase command still claimable instead of the resume", err, next)
	}
	resumed := time.Now().UTC().Truncate(time.Microsecond)
	if err := commands.CompleteCommand(ctx, o.f.session, domain.ProbeCommandOutcome{CommandID: next.CommandID, Status: "applied", AppliedAt: &resumed, Code: domain.DeliveryResumedCode}); err != nil {
		t.Fatal(err)
	}
	if got := drainRemote(t, o); got.Phase != "released" {
		t.Fatalf("drain did not release: %+v", got)
	}
}

// drainRemote runs the disable path for a remote barrier until its original
// members release.
func drainRemote(t *testing.T, o operativeRemoteFixture) domain.AggregateCutoverProgress {
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

// testBusyCodeAlignsAndRearms proves the local source refusal carries the exact
// real edge code (delivery_provider_busy), stays rearmable instead of blocking,
// and succeeds once the provider work resolves.
func testBusyCodeAlignsAndRearms(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeFixture(t, engine, domain.AlertDeliveryAggregate, 1, true)
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
	if err != nil || grant == nil {
		t.Fatal("local provider grant", err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("barrier did not reach quiescing: %+v", got)
	}
	log := o.quiesceLog(t)
	if len(log) != 1 || log[0].Status != "rejected" || log[0].Code != "delivery_provider_busy" {
		t.Fatalf("local refusal = %+v, want rejected delivery_provider_busy", log)
	}
	// Rearmable, not corruption: the barrier is not blocked and the coordinator
	// reissues with a fresh immutable command ID after the cooldown.
	backdateOperativeOutcomes(t, o.p.f.db, domain.DeliveryQuiesceKind)
	if got := o.run(t, true); got.Phase != "quiescing" || !got.Changed {
		t.Fatalf("busy refusal did not rearm after the cooldown: %+v", got)
	}
	if log = o.quiesceLog(t); len(log) != 2 {
		t.Fatalf("rearm did not issue a fresh command: %+v", log)
	}
	if err := o.store.FinishSourceProviderAttempt(ctx, grant.AttemptID, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatal(err)
	}
	backdateOperativeOutcomes(t, o.p.f.db, domain.DeliveryQuiesceKind)
	if got := o.run(t, true); got.Phase != "quiescing" || !got.Changed {
		t.Fatalf("resolved provider work did not reissue quiesce: %+v", got)
	}
	if log = o.quiesceLog(t); len(log) != 3 || log[2].Status != "applied" || log[2].Code != domain.DeliveryQuiescedCode {
		t.Fatalf("resolved quiesce = %+v, want applied delivery_quiesced", log)
	}
	if got := o.run(t, true); got.Phase != "quiesced" {
		t.Fatalf("exact quiesce receipt did not quiesce: %+v", got)
	}
}

// testRemoteBusyRearmsFromReceipt proves a remote busy refusal is rearmable and
// that its cooldown runs from the receipt itself, never from the command's 24h
// expiry.
func testRemoteBusyRearmsFromReceipt(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeRemoteFixture(t, engine)
	if err := o.store.SetOperativeSourceCapability(ctx, o.f.session, true); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("compiled capability did not admit the member: %+v", got)
	}
	refuseOperativeCommand(t, o, domain.DeliveryQuiesceKind, "delivery_provider_busy")
	var outcomeAt time.Time
	if err := o.f.f.db.NewSelect().Table("operative_cutover_commands").Column("outcome_at").
		Where("kind = ?", domain.DeliveryQuiesceKind).Scan(ctx, &outcomeAt); err != nil {
		t.Fatal(err)
	}
	if outcomeAt.After(time.Now().UTC().Add(time.Hour)) {
		t.Fatal("busy rearm cooldown was measured from the command expiry, not the receipt")
	}
	if got := o.run(t, true); got.Phase != "quiescing" || got.Changed {
		t.Fatalf("busy refusal blocked or advanced the barrier: %+v", got)
	}
	backdateOperativeOutcomes(t, o.f.f.db, domain.DeliveryQuiesceKind)
	if got := o.run(t, true); !got.Changed {
		t.Fatal("busy refusal did not rearm after the cooldown")
	}
	if n := countTable(t, o.f.f.db, "probe_commands", "kind", domain.DeliveryQuiesceKind); n != 2 {
		t.Fatalf("rearm issued %d commands, want 2 (fresh immutable ID)", n)
	}
}

// testCorruptionBlocks proves the only rearmable refusal is the busy code: any
// other refusal is corruption and blocks the barrier.
func testCorruptionBlocks(t *testing.T, engine string) {
	ctx := t.Context()
	o := newOperativeRemoteFixture(t, engine)
	if err := o.store.SetOperativeSourceCapability(ctx, o.f.session, true); err != nil {
		t.Fatal(err)
	}
	if got := o.run(t, true); got.Phase != "quiescing" {
		t.Fatalf("compiled capability did not admit the member: %+v", got)
	}
	refuseOperativeCommand(t, o, domain.DeliveryQuiesceKind, "delivery_cutover_corrupt")
	if got := o.run(t, true); got.Phase != "blocked" {
		t.Fatalf("corrupt refusal did not block the barrier: %+v", got)
	}
	phase, reason := o.barrierState(t)
	if phase != "blocked" || reason != "quiesce_refused" {
		t.Fatalf("corrupt refusal = %s/%s, want blocked/quiesce_refused", phase, reason)
	}
}

// refuseOperativeCommand completes one queued cutover command with a typed
// rejection receipt.
func refuseOperativeCommand(t *testing.T, o operativeRemoteFixture, kind, code string) {
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
	if err := commands.CompleteCommand(t.Context(), o.f.session, domain.ProbeCommandOutcome{CommandID: claimed.CommandID, Status: "rejected", Code: code}); err != nil {
		t.Fatal(err)
	}
}
