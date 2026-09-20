package edge

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestEdgeWatchdogSendAuthorization(t *testing.T) {
	s, _, a := watchdogFixture(t)
	ctx := t.Context()
	r := watchdogOpening()
	state, err := s.CommitWatchdog(ctx, a, r)
	if err != nil {
		t.Fatal(err)
	}
	a.HealthGeneration = 0
	active, err := s.ReadActiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	meta := active.Snapshot.ProbeConfigMetadata
	items, err := s.ClaimDeliveries(ctx, a.ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatal("claim", err)
	}
	item := items[0]
	claim := domain.DeliveryClaim{ProbeID: item.ProbeID, DeliveryID: item.DeliveryID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
	if got, err := s.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); err != nil || got == nil || got.SourceAlertID != item.SourceAlertID {
		t.Fatal("valid send rejected", err)
	}
	bad := claim
	bad.LeaseToken = "old"
	if _, err := s.AuthorizeWatchdogDelivery(ctx, a, bad, meta, 10*time.Second); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale claim authorized", err)
	}
	old := meta
	old.Revision++
	if got, err := s.AuthorizeWatchdogDelivery(ctx, a, claim, old, 10*time.Second); err != nil || got != nil {
		t.Fatal("config change authorized stale I/O", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_delivery_outbox SET lease_until=? WHERE delivery_id=?", time.Now().UTC().Add(5*time.Second).UnixMicro(), claim.DeliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("claim did not cover I/O", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE edge_delivery_outbox SET lease_until=? WHERE delivery_id=?", time.Now().UTC().Add(time.Minute).UnixMicro(), claim.DeliveryID); err != nil {
		t.Fatal(err)
	}
	ack := *state.Incident
	at := time.Now().UTC().Truncate(time.Microsecond)
	ack.Status, ack.TransitionVersion, ack.AckedAt, ack.AckCommandID, ack.AckActorDisplayName = domain.AlertStatusAcked, 2, &at, "44444444-4444-4444-8444-444444444444", "Operator"
	r.ExpectedVersion, r.Incident, r.DeliveryIntents = state.Version, &ack, nil
	state, err = s.CommitWatchdog(ctx, a, r)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); err != nil || got != nil {
		t.Fatal("ACK did not suppress DOWN", err)
	}
	up := *state.Incident
	up.Status, up.TransitionVersion, up.ResolvedAt = domain.AlertStatusResolved, 3, &at
	r.ExpectedVersion, r.Status, r.Checkpoint, r.Incident, r.At = state.Version, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogCheckpoint{Armed: true}, &up, at
	intent := item.DeliveryIntent
	intent.DeliveryID = "55555555-5555-4555-8555-555555555555"
	intent.SourceTransitionVersion = 3
	intent.AvailableAt = at
	r.DeliveryIntents = []domain.DeliveryIntent{intent}
	if _, err := s.CommitWatchdog(ctx, a, r); err != nil {
		t.Fatal(err)
	}
	items, err = s.ClaimDeliveries(ctx, a.ProbeID, time.Now().UTC(), time.Minute, 1)
	if err != nil || len(items) != 1 {
		t.Fatal("recovery claim", err)
	}
	item = items[0]
	claim = domain.DeliveryClaim{ProbeID: item.ProbeID, DeliveryID: item.DeliveryID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
	if got, err := s.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); err != nil || got == nil || got.CheckStatus != domain.StatusUp {
		t.Fatal("ACK suppressed recovery", err)
	}
}
