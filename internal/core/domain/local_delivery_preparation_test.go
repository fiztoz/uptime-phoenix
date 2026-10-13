package domain

import (
	"strings"
	"testing"
	"time"
)

func TestLocalDeliveryPreparationShape(t *testing.T) {
	p := LocalDeliveryPreparation{ProbeDeliveryModeApplication: ProbeDeliveryModeApplication{ProbeConfigTarget: ProbeConfigTarget{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: LocalProbeID}, MonitorID: 1, AssignmentRevision: 9007199254740993, Generation: 1, ConfigRevision: 2, SHA256: strings.Repeat("a", 64), StreamID: LocalStreamID, AlertDelivery: AlertDeliveryAggregate}, PreparedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if !ValidLocalDeliveryPreparation(p) {
		t.Fatal("valid request rejected")
	}
	p.AlertDelivery = AlertDeliveryBoth
	if !ValidLocalDeliveryPreparation(p) {
		t.Fatal("both request rejected")
	}
	for name, change := range map[string]func(*LocalDeliveryPreparation){
		"remote":               func(p *LocalDeliveryPreparation) { p.ProbeID = p.HubID },
		"remote_stream":        func(p *LocalDeliveryPreparation) { p.StreamID = p.HubID },
		"unbound":              func(p *LocalDeliveryPreparation) { p.AssignmentRevision = 0 },
		"invalid_generation":   func(p *LocalDeliveryPreparation) { p.Generation = 0 },
		"regional_not_request": func(p *LocalDeliveryPreparation) { p.AlertDelivery = AlertDeliveryRegional },
		"timestamp":            func(p *LocalDeliveryPreparation) { p.PreparedAt = time.Time{} },
		"precision":            func(p *LocalDeliveryPreparation) { p.PreparedAt = p.PreparedAt.Add(time.Nanosecond) },
		"utc":                  func(p *LocalDeliveryPreparation) { p.PreparedAt = p.PreparedAt.In(time.FixedZone("fixture", 7*3600)) },
	} {
		t.Run(name, func(t *testing.T) {
			q := p
			change(&q)
			if ValidLocalDeliveryPreparation(q) {
				t.Fatal("invalid local request shape accepted")
			}
		})
	}
}
