package repository_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestHubWatchdogSendAuthorization(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			h := newHubWatchdogFixture(t, engine)
			ctx := t.Context()
			r := h.opening()
			state, err := h.store.CommitWatchdog(ctx, h.authority, r)
			if err != nil {
				t.Fatal(err)
			}
			outbox := repository.NewRegionalCommitStore(h.f.db)
			items, err := outbox.ClaimDeliveries(ctx, h.authority.ProbeID, time.Now().UTC().Add(time.Hour), time.Minute, 1)
			if err != nil || len(items) != 1 {
				t.Fatal("claim", err)
			}
			item := items[0]
			claim := domain.DeliveryClaim{ProbeID: item.ProbeID, DeliveryID: item.DeliveryID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
			snapshot, err := repository.NewProbeConfigStore(h.f.db).Latest(ctx, item.ProbeID)
			if err != nil {
				t.Fatal(err)
			}
			meta := snapshot.ProbeConfigMetadata
			a := h.authority
			a.HealthGeneration = 0
			if got, err := h.store.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); err != nil || got == nil || got.SourceAlertID != item.SourceAlertID {
				t.Fatal("valid send rejected", err)
			}
			if reclaimed, err := outbox.ClaimDeliveries(ctx, a.ProbeID, time.Now().UTC().Add(2*time.Hour), time.Minute, 1); err != nil || len(reclaimed) != 0 {
				t.Fatal("caller clock reclaimed live DB lease", err)
			}
			for _, mutate := range []func(*domain.ProbeWatchdogAuthority, *domain.DeliveryClaim){
				func(a *domain.ProbeWatchdogAuthority, _ *domain.DeliveryClaim) { a.RuntimeOwner.Epoch++ },
				func(a *domain.ProbeWatchdogAuthority, _ *domain.DeliveryClaim) { a.StreamID = a.ProbeID },
				func(_ *domain.ProbeWatchdogAuthority, c *domain.DeliveryClaim) { c.LeaseToken = "stale" },
				func(_ *domain.ProbeWatchdogAuthority, c *domain.DeliveryClaim) { c.Attempt++ },
			} {
				aa, cc := a, claim
				mutate(&aa, &cc)
				if _, err := h.store.AuthorizeWatchdogDelivery(ctx, aa, cc, meta, 10*time.Second); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("stale authority authorized provider", err)
				}
			}
			bad := meta
			bad.SHA256 = strings.Repeat("f", 64)
			if got, err := h.store.AuthorizeWatchdogDelivery(ctx, a, claim, bad, 10*time.Second); err != nil || got != nil {
				t.Fatal("different applied hash allowed", err)
			}
			if _, err := h.f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until=? WHERE probe_id=?", time.Now().UTC().Add(5*time.Second).Unix(), a.ProbeID); err != nil {
				t.Fatal(err)
			}
			if _, err := h.store.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); !errors.Is(err, ports.ErrConflict) {
				t.Fatal("parent did not cover I/O budget", err)
			}
			if _, err := h.f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until=? WHERE probe_id=?", time.Now().UTC().Add(time.Minute).Unix(), a.ProbeID); err != nil {
				t.Fatal(err)
			}
			if _, err := h.f.db.ExecContext(ctx, "UPDATE probe_delivery_intents SET lease_until=? WHERE delivery_id=?", time.Now().UTC().Add(5*time.Second), claim.DeliveryID); err != nil {
				t.Fatal(err)
			}
			if _, err := h.store.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); !errors.Is(err, ports.ErrConflict) {
				t.Fatal("claim did not cover I/O budget", err)
			}
			if _, err := h.f.db.ExecContext(ctx, "UPDATE probe_delivery_intents SET lease_until=? WHERE delivery_id=?", time.Now().UTC().Add(time.Minute), claim.DeliveryID); err != nil {
				t.Fatal(err)
			}
			ack := *state.Incident
			at := time.Now().UTC().Truncate(time.Microsecond)
			ack.Status, ack.TransitionVersion, ack.AckedAt, ack.AckCommandID, ack.AckActorDisplayName = domain.AlertStatusAcked, 2, &at, "44444444-4444-4444-8444-444444444444", "Operator"
			r.ExpectedVersion, r.Incident, r.DeliveryIntents = state.Version, &ack, nil
			state, err = h.store.CommitWatchdog(ctx, a, r)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := h.store.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); err != nil || got != nil {
				t.Fatal("ACK did not suppress DOWN", err)
			}
			up := *state.Incident
			up.Status, up.TransitionVersion, up.ResolvedAt = domain.AlertStatusResolved, 3, &at
			r.ExpectedVersion, r.Status, r.Checkpoint, r.Incident, r.At = state.Version, domain.ProbeWatchdogHealthy, domain.ProbeWatchdogCheckpoint{Armed: true}, &up, at
			intent := item.DeliveryIntent
			intent.DeliveryID = "55555555-5555-4555-8555-555555555555"
			intent.SourceTransitionVersion = 3
			intent.AvailableAt = at.Add(-time.Second)
			r.DeliveryIntents = []domain.DeliveryIntent{intent}
			if _, err := h.store.CommitWatchdog(ctx, a, r); err != nil {
				t.Fatal(err)
			}
			items, err = outbox.ClaimDeliveries(ctx, a.ProbeID, time.Now().UTC(), time.Minute, 1)
			if err != nil || len(items) != 1 {
				t.Fatal("recovery claim", err)
			}
			item = items[0]
			claim = domain.DeliveryClaim{ProbeID: item.ProbeID, DeliveryID: item.DeliveryID, Attempt: item.Attempt, LeaseToken: item.LeaseToken}
			if got, err := h.store.AuthorizeWatchdogDelivery(ctx, a, claim, meta, 10*time.Second); err != nil || got == nil || got.CheckStatus != domain.StatusUp || got.SourceTransitionVersion != 3 {
				t.Fatal("ACK suppressed recovery", err)
			}
			if err := outbox.FinishDelivery(ctx, claim, domain.DeliveryResult{Status: domain.DeliveryStatusSent, At: time.Now().UTC().Add(-time.Minute)}); err != nil {
				t.Fatal("source clock used as claim authority", err)
			}
		})
	}
}
