package probe

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func preparationRequest(t *testing.T, h *regionalReadHarness, cfg *domain.EdgeResolvedConfig, mode domain.AlertDelivery) domain.ProbeDeliveryPreparation {
	t.Helper()
	fact := h.expectedFact(cfg)
	fact.AlertDelivery = mode
	now := time.Now().UTC().Truncate(time.Microsecond)
	c := domain.ProbeDeliveryPreparation{ProbeDeliveryModeApplication: fact, CommandID: uuid.NewString(), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	payload, err := (DeliveryPreparationCodec{}).EncodeDeliveryPreparation(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	c, err = (DeliveryPreparationCodec{}).DecodeDeliveryPreparation(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func preparationAuthority(h *regionalReadHarness) domain.EdgeCommandAuthority {
	return domain.EdgeCommandAuthority{HubID: h.hubID, ProbeID: h.id.ProbeID, StreamID: h.id.StreamID, ConnectionGeneration: h.generation}
}
func preparationCount(t *testing.T, h *regionalReadHarness) int {
	t.Helper()
	db := regionalReadOpen(t, h.dir)
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM edge_delivery_preparations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEdgeDeliveryPreparationContract(t *testing.T) {
	for _, mode := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(mode), func(t *testing.T) {
			h := newRegionalReadHarness(t)
			h.configs.SetDeliveryPreparations(h.store)
			cfg := h.apply(h.document(mode, exactRegionalAssignmentRevision))
			c := preparationRequest(t, h, cfg, mode)
			authority := preparationAuthority(h)
			before, err := h.store.ReadActiveConfig(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			result, err := h.configs.PrepareDelivery(t.Context(), authority, c)
			if err != nil || result.Status != "applied" || result.Code != "delivery_prepared" || result.AppliedAt == nil || preparationCount(t, h) != 1 {
				t.Fatalf("prepare effect missing: %+v %v", result, err)
			}
			db := regionalReadOpen(t, h.dir)
			var revision, generation, docRevision int64
			var hash, selected string
			err = db.QueryRowContext(t.Context(), "SELECT assignment_revision,generation,config_revision,sha256,alert_delivery FROM edge_delivery_preparations WHERE command_id=?", c.CommandID).Scan(&revision, &generation, &docRevision, &hash, &selected)
			_ = db.Close()
			if err != nil || revision != exactRegionalAssignmentRevision || generation != c.Generation || docRevision != c.ConfigRevision || hash != c.SHA256 || selected != string(mode) {
				t.Fatalf("exact pending tuple lost %d %d %d %v", revision, generation, docRevision, err)
			}
			after, err := h.store.ReadActiveConfig(t.Context())
			if err != nil || !bytes.Equal(before.Snapshot.ProtectedPayload, after.Snapshot.ProtectedPayload) || !after.AppliedAt.Equal(before.AppliedAt) || len(after.RegionalDeliveryApplications) != 1 || after.RegionalDeliveryApplications[0].AlertDelivery != domain.AlertDeliveryRegional {
				t.Fatalf("preparation changed execution: %+v %v", after, err)
			}
			duplicate, err := h.configs.PrepareDelivery(t.Context(), authority, c)
			if err != nil || duplicate.AppliedAt == nil || !duplicate.AppliedAt.Equal(*result.AppliedAt) || preparationCount(t, h) != 1 {
				t.Fatalf("duplicate replaced receipt %+v %v", duplicate, err)
			}
			changed := c
			changed.AssignmentRevision++
			if _, err := h.configs.PrepareDelivery(t.Context(), authority, changed); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("same ID changed request %v", err)
			}
			stale := authority
			stale.ConnectionGeneration--
			if _, err := h.configs.PrepareDelivery(t.Context(), stale, c); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("stale duplicate authority %v", err)
			}
			h.closeStore()
			reopened, err := edge.Open(t.Context(), h.dir, domain.EdgeIdentity{ProbeID: h.id.ProbeID, StreamID: h.id.StreamID, Fingerprint: h.id.Fingerprint})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			cold := services.NewEdgeConfigService(reopened, reopened, NewEdgeConfigDecoder(checker.Get, notifier.Get), h.protector)
			cold.SetDeliveryPreparations(reopened)
			duplicate, err = cold.PrepareDelivery(t.Context(), authority, c)
			if err != nil || duplicate.AppliedAt == nil || !duplicate.AppliedAt.Equal(*result.AppliedAt) {
				t.Fatalf("cold receipt lost %+v %v", duplicate, err)
			}
		})
	}
	mutations := map[string]func(*domain.ProbeDeliveryPreparation){"set_revision": func(c *domain.ProbeDeliveryPreparation) { c.AssignmentRevision++ }, "generation": func(c *domain.ProbeDeliveryPreparation) { c.Generation++ }, "document": func(c *domain.ProbeDeliveryPreparation) { c.ConfigRevision++ }, "hash": func(c *domain.ProbeDeliveryPreparation) { c.SHA256 = string(bytes.Repeat([]byte{'a'}, 64)) }, "monitor": func(c *domain.ProbeDeliveryPreparation) { c.MonitorID++ }, "mode": func(c *domain.ProbeDeliveryPreparation) { c.AlertDelivery = domain.AlertDeliveryBoth }}
	for name, mutate := range mutations {
		t.Run("Mismatch/"+name, func(t *testing.T) {
			h := newRegionalReadHarness(t)
			h.configs.SetDeliveryPreparations(h.store)
			cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
			c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
			mutate(&c)
			result, err := h.configs.PrepareDelivery(t.Context(), preparationAuthority(h), c)
			if err != nil || result.Status != "rejected" || result.AppliedAt != nil || preparationCount(t, h) != 0 {
				t.Fatalf("mismatched preparation accepted %+v %v", result, err)
			}
			repeat, err := h.configs.PrepareDelivery(t.Context(), preparationAuthority(h), c)
			if err != nil || repeat != result {
				t.Fatalf("rejection not durable %+v %v", repeat, err)
			}
		})
	}
	t.Run("MissingFacts", func(t *testing.T) {
		h := newRegionalReadHarness(t)
		h.configs.SetDeliveryPreparations(h.store)
		cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
		c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
		regionalReadExec(t, h.dir, "DELETE FROM edge_regional_delivery_applications")
		result, err := h.configs.PrepareDelivery(t.Context(), preparationAuthority(h), c)
		if err != nil || result.Status != "rejected" || preparationCount(t, h) != 0 {
			t.Fatalf("missing facts synthesized %+v %v", result, err)
		}
	})
	t.Run("CorruptAuthentication", func(t *testing.T) {
		h := newRegionalReadHarness(t)
		h.configs.SetDeliveryPreparations(h.store)
		cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
		c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
		regionalReadExec(t, h.dir, "UPDATE edge_config SET protected_payload=zeroblob(length(protected_payload))")
		if _, err := h.configs.PrepareDelivery(t.Context(), preparationAuthority(h), c); err == nil {
			t.Fatal("corrupt snapshot accepted")
		}
		if preparationCount(t, h) != 0 {
			t.Fatal("corruption persisted preparation")
		}
	})
	t.Run("Expired", func(t *testing.T) {
		h := newRegionalReadHarness(t)
		h.configs.SetDeliveryPreparations(h.store)
		cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
		c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
		c.CreatedAt = c.CreatedAt.Add(-2 * time.Hour)
		c.ExpiresAt = c.ExpiresAt.Add(-2 * time.Hour)
		result, err := h.configs.PrepareDelivery(t.Context(), preparationAuthority(h), c)
		if err != nil || result.Status != "expired" || preparationCount(t, h) != 0 {
			t.Fatalf("expired request accepted %+v %v", result, err)
		}
	})
	t.Run("Canceled", func(t *testing.T) {
		h := newRegionalReadHarness(t)
		h.configs.SetDeliveryPreparations(h.store)
		cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
		c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := h.configs.PrepareDelivery(ctx, preparationAuthority(h), c); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request %v", err)
		}
		if preparationCount(t, h) != 0 {
			t.Fatal("canceled preparation stored")
		}
	})
}
