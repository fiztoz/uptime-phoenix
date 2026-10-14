package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type preparationReceiptFake struct {
	read, write int
	prior       domain.ProbeCommandOutcome
}

var _ ports.EdgeDeliveryPreparationRepository = (*preparationReceiptFake)(nil)

func (r *preparationReceiptFake) ReadDeliveryPreparationReceipt(context.Context, domain.EdgeCommandAuthority, domain.ProbeDeliveryPreparation) (*domain.ProbeCommandOutcome, error) {
	r.read++
	return &r.prior, nil
}
func (r *preparationReceiptFake) PrepareDelivery(context.Context, domain.EdgeCommandAuthority, domain.ProbeDeliveryPreparation, *domain.EdgeActiveConfig) (domain.ProbeCommandOutcome, error) {
	r.write++
	return domain.ProbeCommandOutcome{}, domain.ErrValidation
}

// TestDeliveryPreparationReceiptRecoveryDoesNotLoadNewConfig verifies the core
// lifecycle independently of adapters: receipt recovery is not new eligibility.
func TestDeliveryPreparationReceiptRecoveryDoesNotLoadNewConfig(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	id := "11111111-1111-4111-8111-111111111111"
	command := domain.ProbeDeliveryPreparation{ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: id, ProbeID: id}, MonitorID: 1, AssignmentRevision: 9007199254740993, Generation: 1, ConfigRevision: 2, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", StreamID: "22222222-2222-4222-8222-222222222222", AlertDelivery: domain.AlertDeliveryBoth}, CommandID: id, CreatedAt: at.Add(-2 * time.Hour), ExpiresAt: at.Add(-time.Hour), PayloadHash: [32]byte{1}}
	repo := &preparationReceiptFake{prior: domain.ProbeCommandOutcome{CommandID: id, Status: "applied", Code: "delivery_prepared", AppliedAt: &at}}
	service := &EdgeConfigService{} // No current config ports: an accidental load fails.
	service.SetDeliveryPreparations(repo)
	got, err := service.PrepareDelivery(t.Context(), domain.EdgeCommandAuthority{HubID: command.HubID, ProbeID: command.ProbeID, StreamID: command.StreamID, ConnectionGeneration: 2}, command)
	if err != nil || got.CommandID != id || got.Status != "applied" || repo.read != 1 || repo.write != 0 {
		t.Fatalf("recovery reinterpreted current state: %+v %v read=%d write=%d", got, err, repo.read, repo.write)
	}
}
