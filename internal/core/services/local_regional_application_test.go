package services

import (
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestLocalRegionalApplicationRefreshCompleteness(t *testing.T) {
	target := domain.ProbeConfigTarget{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: domain.LocalProbeID}
	definition := func() *domain.LocalProbeConfigDefinition {
		return &domain.LocalProbeConfigDefinition{Target: target, Revision: 3,
			Assignments: []domain.ProbeConfigAssignment{{Monitor: &domain.Monitor{ID: 7}, Generation: 1, AssignmentRevision: 9007199254740993,
				AlertDelivery: domain.AlertDeliveryAggregate}},
			RegionalDeliveryApplications: []domain.ProbeDeliveryModeApplication{{ProbeConfigTarget: target, MonitorID: 7, Generation: 1,
				AssignmentRevision: 9007199254740993, ConfigRevision: 3, SHA256: strings.Repeat("1", 64), StreamID: domain.LocalStreamID,
				AlertDelivery: domain.AlertDeliveryRegional}}}
	}
	if !localConfigHasAssignmentBindings(definition()) || !localConfigHasRegionalApplications(definition()) {
		t.Fatal("separate set/document identities or desired aggregate blocked implemented regional mode")
	}
	for name, change := range map[string]func(*domain.LocalProbeConfigDefinition){
		"missing": func(d *domain.LocalProbeConfigDefinition) { d.RegionalDeliveryApplications = nil },
		"extra": func(d *domain.LocalProbeConfigDefinition) {
			d.RegionalDeliveryApplications = append(d.RegionalDeliveryApplications, d.RegionalDeliveryApplications[0])
		},
		"foreign_monitor": func(d *domain.LocalProbeConfigDefinition) { d.RegionalDeliveryApplications[0].MonitorID++ },
		"target": func(d *domain.LocalProbeConfigDefinition) {
			d.RegionalDeliveryApplications[0].HubID = "22222222-3333-4444-8555-666666666666"
		},
		"generation":  func(d *domain.LocalProbeConfigDefinition) { d.RegionalDeliveryApplications[0].Generation++ },
		"set_binding": func(d *domain.LocalProbeConfigDefinition) { d.RegionalDeliveryApplications[0].AssignmentRevision++ },
		"document":    func(d *domain.LocalProbeConfigDefinition) { d.RegionalDeliveryApplications[0].ConfigRevision++ },
		"unimplemented_mode": func(d *domain.LocalProbeConfigDefinition) {
			d.RegionalDeliveryApplications[0].AlertDelivery = domain.AlertDeliveryAggregate
		},
		"stream": func(d *domain.LocalProbeConfigDefinition) {
			d.RegionalDeliveryApplications[0].StreamID = "22222222-3333-4444-8555-666666666666"
		},
		"legacy":      func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].AssignmentRevision = 0 },
		"nil_monitor": func(d *domain.LocalProbeConfigDefinition) { d.Assignments[0].Monitor = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := definition()
			change(d)
			if localConfigHasRegionalApplications(d) {
				t.Fatal("incomplete/unimplemented state satisfied refresh fast path")
			}
		})
	}
	if !localConfigHasRegionalApplications(&domain.LocalProbeConfigDefinition{}) {
		t.Fatal("empty membership required fabricated per-monitor facts")
	}
}
