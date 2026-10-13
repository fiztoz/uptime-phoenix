package probe

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type preparationInterleaver struct {
	ports.ProbeConfigProtector
	before func()
}

func (p preparationInterleaver) Open(ctx context.Context, m domain.ProbeConfigMetadata, b []byte) ([]byte, error) {
	p.before()
	return p.ProbeConfigProtector.Open(ctx, m, b)
}

func TestDeliveryPreparationCapturedTuple(t *testing.T) {
	h := newRegionalReadHarness(t)
	cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
	active, err := h.store.ReadActiveConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	poison := active
	poison.Snapshot.KeyConfirmation = "invalid"
	mock := &regionalReadConfigMock{primary: active, secondary: &poison}
	service := h.service(h.store, mock)
	service.SetDeliveryPreparations(h.store)
	out, err := service.PrepareDelivery(t.Context(), preparationAuthority(h), c)
	if err != nil || out.Status != "applied" || mock.reads != 1 {
		t.Fatalf("authenticated a second tuple: reads=%d %+v %v", mock.reads, out, err)
	}
}

// TestDeliveryPreparationCapturedCipherChange proves a stored snapshot cannot
// be replaced between authentication and the transaction while keeping metadata.
func TestDeliveryPreparationCapturedCipherChange(t *testing.T) {
	h := newRegionalReadHarness(t)
	cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
	p := preparationInterleaver{ProbeConfigProtector: h.protector, before: func() {
		regionalReadExec(t, h.dir, "UPDATE edge_config SET protected_payload=zeroblob(length(protected_payload))")
	}}
	service := services.NewEdgeConfigService(h.store, h.store, NewEdgeConfigDecoder(checker.Get, notifier.Get), p)
	service.SetDeliveryPreparations(h.store)
	out, err := service.PrepareDelivery(t.Context(), preparationAuthority(h), c)
	if err != nil || out.Status != "rejected" || out.AppliedAt != nil || preparationCount(t, h) != 0 {
		t.Fatalf("unauthenticated replacement accepted: %+v %v", out, err)
	}
}

func TestDeliveryPreparationStoredSessionChange(t *testing.T) {
	h := newRegionalReadHarness(t)
	cfg := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	c := preparationRequest(t, h, cfg, domain.AlertDeliveryAggregate)
	p := preparationInterleaver{ProbeConfigProtector: h.protector, before: func() {
		if err := h.store.AcceptConnectionGeneration(t.Context(), h.hubID, h.generation+1); err != nil {
			t.Fatal(err)
		}
	}}
	// Constructor injection keeps production instrumentation unnecessary.
	service := services.NewEdgeConfigService(h.store, h.store, NewEdgeConfigDecoder(checker.Get, notifier.Get), p)
	service.SetDeliveryPreparations(h.store)
	out, err := service.PrepareDelivery(t.Context(), preparationAuthority(h), c)
	if !errors.Is(err, ports.ErrConflict) || out.Status != "" || preparationCount(t, h) != 0 {
		t.Fatalf("captured authority substituted for stored fence: %+v %v", out, err)
	}
}
