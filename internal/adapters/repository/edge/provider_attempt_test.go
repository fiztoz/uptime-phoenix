package edge

// Provider-attempt journal effects: final availability authorization journals
// one opaque grant in the same transaction as the stored claim, config, gate and
// lifecycle checks; only an exact caller release finishes it. Unknown, crashed
// or errored attempts block quiescence and drain. Certificate and capacity
// alerts keep independent scopes and never consult the gate or journal grants.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func cutoverClaim(t *testing.T, s *Store, probeID string) domain.QueuedDelivery {
	t.Helper()
	items, err := s.ClaimDeliveries(t.Context(), probeID, time.Now().UTC(), time.Minute, 10)
	if err != nil || len(items) == 0 {
		t.Fatalf("claim deliveries: %v %v", items, err)
	}
	return items[0]
}

func cutoverLease(item domain.QueuedDelivery) domain.DeliveryClaim {
	return domain.DeliveryClaim{DeliveryID: item.DeliveryID, ProbeID: item.ProbeID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
}

// TestEdgeProviderAttemptGrantLifecycle proves the grant is journaled at final
// authorization, one per claim, finished only by an exact release, immutable
// once known and unaffected by claim or lease expiry.
func TestEdgeProviderAttemptGrantLifecycle(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	if _, err := h.store.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
		t.Fatal(err)
	}
	item := cutoverClaim(t, h.store, testIdentity().ProbeID)
	claim := cutoverLease(item)
	authorized, err := h.store.AuthorizeEdgeDelivery(t.Context(), claim, h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || authorized == nil || authorized.ProviderAttemptID == "" {
		t.Fatalf("availability authorization lost its grant: %+v %v", authorized, err)
	}
	grant := authorized.ProviderAttemptID
	var state string
	if err := h.store.db.NewRaw("SELECT state FROM edge_provider_attempts WHERE attempt_id = ?", grant).Scan(t.Context(), &state); err != nil || state != string(domain.ProviderAttemptInFlight) {
		t.Fatalf("grant not journaled in_flight: %q %v", state, err)
	}
	// One grant per claimed attempt: a repeat authorization returns the same
	// grant instead of opening a second unaccounted I/O permission.
	again, err := h.store.AuthorizeEdgeDelivery(t.Context(), claim, h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || again == nil || again.ProviderAttemptID != grant {
		t.Fatalf("repeat authorization opened another grant: %+v %v", again, err)
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_provider_attempts"); n != 1 {
		t.Fatalf("duplicate grants journaled: %d", n)
	}

	// The exact release may finish after the claim's lease expired.
	if _, err := h.store.db.ExecContext(t.Context(), "UPDATE edge_delivery_outbox SET leased_at = 0, lease_until = 1 WHERE delivery_id = ?", item.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.FinishSourceProviderAttempt(t.Context(), grant, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatalf("release after lease expiry: %v", err)
	}
	if err := h.store.FinishSourceProviderAttempt(t.Context(), grant, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatalf("identical release not idempotent: %v", err)
	}
	if err := h.store.FinishSourceProviderAttempt(t.Context(), grant, domain.ProviderAttemptAmbiguous); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("known state rewritten: %v", err)
	}
	// Restore a live lease and prove the finished grant itself refuses a new
	// start for the same claimed attempt: no second unaccounted I/O permission.
	nowMicro := time.Now().UTC().UnixMicro()
	if _, err := h.store.db.ExecContext(t.Context(), "UPDATE edge_delivery_outbox SET leased_at = ?, lease_until = ? WHERE delivery_id = ?",
		nowMicro, nowMicro+int64(time.Minute/time.Microsecond), item.DeliveryID); err != nil {
		t.Fatal(err)
	}
	again, err = h.store.AuthorizeEdgeDelivery(t.Context(), claim, h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if again != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("finished grant reopened provider I/O: %+v %v", again, err)
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_provider_attempts"); n != 1 {
		t.Fatalf("finished grant grew another grant: %d", n)
	}
	// The database itself refuses state laundering, not just the service.
	if _, err := h.store.db.ExecContext(t.Context(), "UPDATE edge_provider_attempts SET state = 'ambiguous' WHERE attempt_id = ?", grant); err == nil {
		t.Fatal("storage allowed laundering a known attempt state")
	}
	if err := h.store.FinishSourceProviderAttempt(t.Context(), uuid.NewString(), domain.ProviderAttemptSucceeded); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("unknown grant finished: %v", err)
	}
	if err := h.store.FinishSourceProviderAttempt(t.Context(), grant, domain.ProviderAttemptInFlight); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unfinished state accepted as release: %v", err)
	}
}

// TestEdgeProviderAttemptBusyBlocksQuiescence proves a live grant leaves a
// truthful quiescing hold behind a durable rejected receipt, and a later
// immutable command finishes the same barrier once the attempt resolves.
func TestEdgeProviderAttemptBusyBlocksQuiescence(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	if _, err := h.store.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
		t.Fatal(err)
	}
	item := cutoverClaim(t, h.store, testIdentity().ProbeID)
	authorized, err := h.store.AuthorizeEdgeDelivery(t.Context(), cutoverLease(item), h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || authorized == nil {
		t.Fatalf("authorize: %v %v", authorized, err)
	}
	grant := authorized.ProviderAttemptID

	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	out := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate))
	if out.Status != "rejected" || out.Code != "delivery_provider_busy" || out.AppliedAt != nil {
		t.Fatalf("busy provider attempt faked quiescence: %+v", out)
	}
	if got := h.gateState(barrier); got != deliveryGateQuiescing {
		t.Fatalf("busy refusal lost the hold: %q", got)
	}

	// An ambiguous error outcome stays unresolved and keeps blocking.
	if err := h.store.FinishSourceProviderAttempt(t.Context(), grant, domain.ProviderAttemptAmbiguous); err != nil {
		t.Fatal(err)
	}
	out = h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate))
	if out.Status != "rejected" || out.Code != "delivery_provider_busy" {
		t.Fatalf("ambiguous attempt cleared: %+v", out)
	}
	if got := h.gateState(barrier); got != deliveryGateQuiescing {
		t.Fatalf("ambiguous attempt dropped the hold: %q", got)
	}
}

// TestEdgeDeliveryCutoverQuiesceAfterFinishedAttempt proves a resolved grant
// lets a later immutable command finish the same barrier without rewriting the
// busy receipt.
func TestEdgeDeliveryCutoverQuiesceAfterFinishedAttempt(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	if _, err := h.store.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
		t.Fatal(err)
	}
	item := cutoverClaim(t, h.store, testIdentity().ProbeID)
	authorized, err := h.store.AuthorizeEdgeDelivery(t.Context(), cutoverLease(item), h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || authorized == nil {
		t.Fatalf("authorize: %v %v", authorized, err)
	}
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	busy := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate))
	if busy.Status != "rejected" {
		t.Fatalf("expected busy refusal: %+v", busy)
	}
	if err := h.store.FinishSourceProviderAttempt(t.Context(), authorized.ProviderAttemptID, domain.ProviderAttemptSucceeded); err != nil {
		t.Fatal(err)
	}
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	if got := h.gateState(barrier); got != deliveryGateQuiesced {
		t.Fatalf("gate not quiesced: %q", got)
	}
	var original edgeCommandRow
	if err := h.store.db.NewSelect().Model(&original).Where("command_id = ?", busy.CommandID).Scan(t.Context()); err != nil || original.Status != "rejected" {
		t.Fatalf("busy receipt rewritten: %+v %v", original, err)
	}
}

// TestEdgeDeliveryCutoverCertCapacityIndependence proves certificate and
// capacity alerts keep their independent scopes under a live aggregate hold:
// their intents still record, still authorize and never journal a provider
// grant, while new availability intents stay filtered.
func TestEdgeDeliveryCutoverCertCapacityIndependence(t *testing.T) {
	newHold := func(t *testing.T) *cutoverHarness {
		t.Helper()
		h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
		preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
		barrier := uuid.NewString()
		requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
		requireCutover(t, h.execute(h.command(domain.DeliveryApplyKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryAppliedCode)
		if _, err := h.store.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
			t.Fatal(err)
		}
		if n := h.outboxCount(domain.DeliveryEventStatusChange); n != 0 {
			t.Fatalf("availability intent enqueued under hold: %d", n)
		}
		return h
	}
	authorize := func(t *testing.T, h *cutoverHarness, eventKind string) {
		t.Helper()
		items, err := h.store.ClaimDeliveries(t.Context(), testIdentity().ProbeID, time.Now().UTC(), time.Minute, 10)
		if err != nil || len(items) != 1 || items[0].EventKind != eventKind {
			t.Fatalf("claim independent work: %v %v", items, err)
		}
		authorized, err := h.store.AuthorizeEdgeDelivery(t.Context(), cutoverLease(items[0]), h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
		if err != nil || authorized == nil {
			t.Fatalf("independent scope blocked by hold: %+v %v", authorized, err)
		}
		if authorized.ProviderAttemptID != "" {
			t.Fatalf("independent scope journaled a provider grant: %q", authorized.ProviderAttemptID)
		}
	}
	t.Run("certificate", func(t *testing.T) {
		h := newHold(t)
		at := time.Now().UTC().Truncate(time.Microsecond)
		c, a := certAssignment()
		certRecord(t, h.store, c, a, at, 10, at.Add(10*24*time.Hour))
		if n := h.outboxCount(domain.DeliveryEventCertificateExpiry); n != 1 {
			t.Fatalf("certificate intent suppressed: %d", n)
		}
		authorize(t, h, domain.DeliveryEventCertificateExpiry)
	})
	t.Run("capacity", func(t *testing.T) {
		h := newHold(t)
		at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
		c, a := capacityAssignment(true)
		record := func(result ports.CheckResult, when time.Time) {
			t.Helper()
			if _, err := services.NewEdgeRecordingService(h.store, h.store, nil).Record(t.Context(), c, a, result, when); err != nil {
				t.Fatal(err)
			}
		}
		record(capacityResult(domain.ConditionStateWarning, 84), at)
		record(capacityResult(domain.ConditionStateWarning, 85), at.Add(time.Minute))
		if n := h.outboxCount(domain.DeliveryEventCapacityCondition); n != 1 {
			t.Fatalf("capacity intent suppressed: %d", n)
		}
		authorize(t, h, domain.DeliveryEventCapacityCondition)
	})
}

// TestEdgeDeliveryCutoverEscalationIntentsFiltered proves the ladder keeps
// committing as incident truth under an aggregate hold while its new
// availability intents are filtered until the gate releases.
func TestEdgeDeliveryCutoverEscalationIntentsFiltered(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)

	rec := checkRecord()
	step := int64(1)
	rec.Incident.EscalationPolicyID = 30
	rec.Incident.EscalationPolicyVersion = 1
	rec.Incident.EscalationStatus = domain.EscalationStatePending
	rec.Incident.EscalationNextStep = &step
	rec.Incident.EscalationNextRunAt = &rec.Observation.ObservedAt
	if _, err := h.store.CommitEdgeCheck(t.Context(), rec); err != nil {
		t.Fatal(err)
	}

	policy := &domain.EscalationPolicy{ID: 30, Enabled: true, Steps: []domain.EscalationStep{{StepOrder: 1, NotificationIDs: []int64{10}}, {StepOrder: 2, NotificationIDs: []int64{10}}}}
	current := *rec.Incident
	at := rec.Observation.ObservedAt
	if _, changed := domain.AdvanceAvailabilityEscalation(&current, policy, 1, at); !changed {
		t.Fatal("ladder did not advance")
	}
	intent := domain.DeliveryIntent{DeliveryID: uuid.NewString(), SourceAlertID: current.SourceAlertID,
		SourceTransitionVersion: current.TransitionVersion, ProbeID: current.ProbeID, NotificationID: 10,
		NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at,
		EscalationPolicyID: 30, EscalationStep: 1}
	if err := h.store.CommitEscalationAdvance(t.Context(), 1, current, []domain.DeliveryIntent{intent}, "step one", at); err != nil {
		t.Fatal(err)
	}
	if n := h.outboxCount(domain.DeliveryEventStatusChange); n != 0 {
		t.Fatalf("escalation intent enqueued under hold: %d", n)
	}
	var escalationStatus string
	if err := h.store.db.NewRaw("SELECT escalation_status FROM edge_alerts WHERE source_alert_id = ?", current.SourceAlertID).Scan(t.Context(), &escalationStatus); err != nil || escalationStatus != domain.EscalationStatePending {
		t.Fatalf("ladder truth did not continue: %q %v", escalationStatus, err)
	}

	requireCutover(t, h.execute(h.command(domain.DeliveryResumeKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryResumedCode)
	if _, changed := domain.AdvanceAvailabilityEscalation(&current, policy, 1, at); !changed {
		t.Fatal("second rung did not advance")
	}
	next := domain.DeliveryIntent{DeliveryID: uuid.NewString(), SourceAlertID: current.SourceAlertID,
		SourceTransitionVersion: current.TransitionVersion, ProbeID: current.ProbeID, NotificationID: 10,
		NotificationVersion: 1, EventKind: domain.DeliveryEventStatusChange, AvailableAt: at,
		EscalationPolicyID: 30, EscalationStep: 2}
	if err := h.store.CommitEscalationAdvance(t.Context(), current.TransitionVersion-1, current, []domain.DeliveryIntent{next}, "step two", at); err != nil {
		t.Fatal(err)
	}
	if n := h.outboxCount(domain.DeliveryEventStatusChange); n != 1 {
		t.Fatalf("released gate still filtered escalation work: %d", n)
	}
}
