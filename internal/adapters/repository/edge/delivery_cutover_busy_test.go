package edge

// Issue #191: a quiesce the source refuses as delivery_provider_busy must never
// leave the monitor without a page path. While a send may still be in progress
// the barrier cannot complete at that source; if the refusal engaged the
// aggregate hold, regional paging stopped while aggregate paging could not
// start. An ambiguous attempt is permanent by contract and so must not refuse
// at all (#211): it would never let the barrier complete. These tests drive
// the production lifecycle: real recording, the real outbox, the real edge
// delivery service and provider-attempt journal, and the real cutover executor.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// busyPageSender records every alert a provider would receive and fails with a
// definite-looking connection refusal while err is set, exactly like a webhook
// that refuses connections from the probe region.
type busyPageSender struct {
	alerts []domain.AlertContext
	err    error
}

func (s *busyPageSender) Type() string                  { return "webhook" }
func (s *busyPageSender) Validate(map[string]any) error { return nil }
func (s *busyPageSender) Send(_ context.Context, _ map[string]any, alert domain.AlertContext) error {
	if s.err != nil {
		return s.err
	}
	s.alerts = append(s.alerts, alert)
	return nil
}

// busyPagingConfig is the resolved graph the edge delivery service renders
// from. It carries the exact metadata the store sealed, so final provider
// authorization proves the accepted configuration rather than a copy.
func busyPagingConfig(h *cutoverHarness) *domain.EdgeResolvedConfig {
	monitor := &domain.Monitor{ID: 17, Name: "portal", Type: "http", Active: true}
	a := domain.EdgeResolvedAssignment{Monitor: monitor, Generation: 1, AssignmentRevision: h.setID,
		NotificationLinks: []domain.MonitorNotification{{MonitorID: 17, NotificationID: 10}}}
	return &domain.EdgeResolvedConfig{
		Metadata:    h.active.Snapshot.ProbeConfigMetadata,
		Assignments: []domain.EdgeResolvedAssignment{a},
		Maintenance: map[int64]*domain.MaintenanceWindow{},
		Channels: map[int64]domain.EdgeResolvedChannel{
			10: {Notification: &domain.Notification{ID: 10, Type: "webhook", Active: true}, Version: 1},
		},
	}
}

func (h *cutoverHarness) attemptCount(state domain.ProviderAttemptState) int {
	h.t.Helper()
	return certCount(h.t, h.store, "SELECT COUNT(*) FROM edge_provider_attempts WHERE state = '"+string(state)+"'")
}

// recoveryRecord resolves checkRecord's incident with its own recovery intent.
func recoveryRecord(down domain.EdgeCheckRecord, stateSeq int64) domain.EdgeCheckRecord {
	up := down
	at := time.Now().UTC().Truncate(time.Microsecond)
	incident := *down.Incident
	up.Incident = &incident
	up.ExpectedStateSeq, up.ExpectedIncidentVersion = stateSeq, 1
	up.Observation.Status, up.Observation.RawStatus, up.Observation.DownCount = domain.StatusUp, domain.StatusUp, 0
	up.Observation.ObservedAt, up.Observation.ReceivedAt = at, at
	up.Incident.Status, up.Incident.TransitionVersion, up.Incident.ResolvedAt = domain.AlertStatusResolved, 2, &at
	up.DeliveryIntents = []domain.DeliveryIntent{{DeliveryID: uuid.NewString(), SourceAlertID: incident.SourceAlertID,
		SourceTransitionVersion: 2, ProbeID: incident.ProbeID, NotificationID: 10, NotificationVersion: 1,
		EventKind: domain.DeliveryEventStatusChange, AvailableAt: at}}
	return up
}

// freshDownRecord opens a NEW incident after the earlier one resolved: the
// "take the target down" step of the issue's reproduction.
func freshDownRecord(stateSeq int64) domain.EdgeCheckRecord {
	down := checkRecord()
	at := time.Now().UTC().Truncate(time.Microsecond)
	incident := *down.Incident
	incident.SourceAlertID, incident.StartedAt = uuid.NewString(), at
	down.Incident = &incident
	down.ExpectedStateSeq, down.ExpectedIncidentVersion = stateSeq, 2
	down.Observation.ObservedAt, down.Observation.ReceivedAt = at, at
	down.DeliveryIntents = []domain.DeliveryIntent{{DeliveryID: uuid.NewString(), SourceAlertID: incident.SourceAlertID,
		SourceTransitionVersion: 1, ProbeID: incident.ProbeID, NotificationID: 10, NotificationVersion: 1,
		EventKind: domain.DeliveryEventStatusChange, AvailableAt: at}}
	return down
}

func (h *cutoverHarness) stateSeq() int64 {
	h.t.Helper()
	var seq int64
	if err := h.store.db.NewRaw("SELECT seq FROM edge_regional_state WHERE monitor_id = 17").Scan(h.t.Context(), &seq); err != nil {
		h.t.Fatal(err)
	}
	return seq
}

// inFlightGrant journals a provider grant that never finishes: a send still in
// progress, or one whose process crashed mid-send.
func (h *cutoverHarness) inFlightGrant(grantedAt time.Time) {
	h.t.Helper()
	row := providerAttemptRow{AttemptID: uuid.NewString(), DeliveryID: uuid.NewString(), Attempt: 1, MonitorID: 17,
		State: domain.ProviderAttemptInFlight, GrantedAt: grantedAt.UnixMicro()}
	if _, err := h.store.db.NewInsert().Model(&row).Exec(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
}

// TestEdgeBusyQuiesceRefusalKeepsRegionalPagePath reproduces #191 on one
// source: a send is still in progress, the aggregate cutover's quiesce is
// refused busy, and the target then goes down. The refused barrier cannot
// activate here, so regional paging must continue — the DOWN, its recovery and
// a fresh DOWN after a rearmed refusal. Once that send can no longer be
// running, the same barrier quiesces (#211).
func TestEdgeBusyQuiesceRefusalKeepsRegionalPagePath(t *testing.T) {
	ctx := t.Context()
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	sender := &busyPageSender{}
	delivery := services.NewEdgeDeliveryService(h.store, certConfigReader{busyPagingConfig(h)}, h.store, nil,
		func(kind string) (ports.NotificationSender, bool) { return sender, kind == "webhook" })
	probeID := testIdentity().ProbeID
	h.inFlightGrant(time.Now())

	// The hub cuts the monitor over to aggregate; the send in progress makes
	// this source refuse the quiesce, and the refusal is not quiescence.
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	refused := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate))
	if refused.Status != "rejected" || refused.Code != "delivery_provider_busy" || refused.AppliedAt != nil {
		t.Fatalf("a send in progress faked quiescence: %+v", refused)
	}

	// The target goes down and recovers. Both page regionally.
	down := checkRecord()
	if _, err := h.store.CommitEdgeCheck(ctx, down); err != nil {
		t.Fatal(err)
	}
	if worked, err := delivery.ProcessNext(ctx, probeID); err != nil || !worked {
		t.Fatalf("DOWN page after a refused quiesce: %v %v", worked, err)
	}
	if _, err := h.store.CommitEdgeCheck(ctx, recoveryRecord(down, h.stateSeq())); err != nil {
		t.Fatal(err)
	}
	if worked, err := delivery.ProcessNext(ctx, probeID); err != nil || !worked {
		t.Fatalf("recovery page: %v %v", worked, err)
	}

	// The hub rearms the same barrier with a new immutable command; it is
	// refused again, and a fresh outage still pages.
	again := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate))
	if again.Status != "rejected" || again.Code != "delivery_provider_busy" {
		t.Fatalf("rearmed quiesce while the send may still run: %+v", again)
	}
	if _, err := h.store.CommitEdgeCheck(ctx, freshDownRecord(h.stateSeq())); err != nil {
		t.Fatal(err)
	}
	if n := h.outboxCount(domain.DeliveryEventStatusChange); n != 3 {
		t.Fatalf("fresh DOWN intent was suppressed by a refused barrier: %d intents", n)
	}
	if worked, err := delivery.ProcessNext(ctx, probeID); err != nil || !worked {
		t.Fatalf("fresh DOWN page: %v %v", worked, err)
	}
	if len(sender.alerts) != 3 || sender.alerts[0].Status != domain.StatusDown || sender.alerts[1].Status != domain.StatusUp || sender.alerts[2].Status != domain.StatusDown {
		t.Fatalf("regional pages after a refused quiesce: %+v", sender.alerts)
	}

	// The send never completes and its window passes: it crashed. Waiting can
	// never resolve it, so the next immutable command quiesces the barrier.
	if _, err := h.store.db.ExecContext(ctx, "UPDATE edge_provider_attempts SET granted_at = ? WHERE state = 'in_flight'", time.Now().Add(-providerAttemptSettleWindow-time.Second).UnixMicro()); err != nil {
		t.Fatal(err)
	}
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	if held, err := aggregateSourceHoldActive(ctx, h.store.db, 17); err != nil || !held {
		t.Fatalf("quiesce after the send expired did not engage the hold: %v %v", held, err)
	}
	// The crashed send's fact is untouched: nothing fabricated its outcome.
	if n := h.attemptCount(domain.ProviderAttemptInFlight); n != 1 {
		t.Fatalf("in_flight attempt was rewritten: %d", n)
	}
}

// TestEdgeAmbiguousAttemptDoesNotRefuseQuiesce: a failed regional send is
// ambiguous forever and used to refuse every quiesce as busy, so the monitor
// could never cut over again (#211). It has already returned, so the quiesce
// succeeds and the ambiguous fact is kept.
func TestEdgeAmbiguousAttemptDoesNotRefuseQuiesce(t *testing.T) {
	ctx := t.Context()
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	sender := &busyPageSender{err: errors.New("dial tcp 203.0.113.7:443: connect: connection refused")}
	delivery := services.NewEdgeDeliveryService(h.store, certConfigReader{busyPagingConfig(h)}, h.store, nil,
		func(kind string) (ports.NotificationSender, bool) { return sender, kind == "webhook" })
	if _, err := h.store.CommitEdgeCheck(ctx, checkRecord()); err != nil {
		t.Fatal(err)
	}
	if worked, err := delivery.ProcessNext(ctx, testIdentity().ProbeID); err != nil || !worked {
		t.Fatalf("regional attempt: %v %v", worked, err)
	}
	if n := h.attemptCount(domain.ProviderAttemptAmbiguous); n != 1 {
		t.Fatalf("failed send did not leave one ambiguous attempt: %d", n)
	}
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), uuid.NewString(), preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	if n := h.attemptCount(domain.ProviderAttemptAmbiguous); n != 1 {
		t.Fatalf("ambiguous attempt was rewritten: %d", n)
	}
}

// TestEdgeBusyRefusedBarrierStillQuiescesAndSuppresses proves the fix does not
// weaken duplicate-page suppression: once every attempt is resolved, a later
// immutable command quiesces the same refused barrier, and from then on the
// aggregate hold suppresses new regional availability work.
func TestEdgeBusyRefusedBarrierStillQuiescesAndSuppresses(t *testing.T) {
	ctx := t.Context()
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	down := checkRecord()
	if _, err := h.store.CommitEdgeCheck(ctx, down); err != nil {
		t.Fatal(err)
	}
	item := cutoverClaim(t, h.store, testIdentity().ProbeID)
	authorized, err := h.store.AuthorizeEdgeDelivery(ctx, cutoverLease(item), h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || authorized == nil {
		t.Fatalf("authorize: %v %v", authorized, err)
	}
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	busy := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate))
	if busy.Status != "rejected" || busy.Code != "delivery_provider_busy" {
		t.Fatalf("live attempt faked quiescence: %+v", busy)
	}
	if held, err := aggregateSourceHoldActive(ctx, h.store.db, 17); err != nil || held {
		t.Fatalf("a refused quiesce engaged the aggregate hold: %v %v", held, err)
	}
	if err := h.store.FinishSourceProviderAttempt(ctx, authorized.ProviderAttemptID, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatal(err)
	}
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	if held, err := aggregateSourceHoldActive(ctx, h.store.db, 17); err != nil || !held {
		t.Fatalf("a successful quiesce did not engage the aggregate hold: %v %v", held, err)
	}
	if _, err := h.store.CommitEdgeCheck(ctx, recoveryRecord(down, h.stateSeq())); err != nil {
		t.Fatal(err)
	}
	if n := h.outboxCount(domain.DeliveryEventStatusChange); n != 1 {
		t.Fatalf("availability intent enqueued under a quiesced aggregate hold: %d", n)
	}
}
