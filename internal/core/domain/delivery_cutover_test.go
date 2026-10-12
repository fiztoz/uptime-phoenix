package domain

import (
	"strings"
	"testing"
)

func TestDeliveryModeApplicationShape(t *testing.T) {
	valid := ProbeDeliveryModeApplication{
		ProbeConfigTarget: ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: LocalProbeID},
		MonitorID:         1, AssignmentRevision: 9007199254740993, Generation: 3, ConfigRevision: 47,
		SHA256: strings.Repeat("a", 64), StreamID: LocalStreamID, AlertDelivery: AlertDeliveryAggregate,
	}
	for _, mode := range []AlertDelivery{AlertDeliveryRegional, AlertDeliveryAggregate, AlertDeliveryBoth} {
		for _, probe := range []string{LocalProbeID, "22222222-2222-4222-8222-222222222222"} {
			a := valid
			a.AlertDelivery, a.ProbeID = mode, probe
			if !ValidProbeDeliveryModeApplication(a) {
				t.Fatal("well-formed local/remote mode attestation rejected")
			}
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*ProbeDeliveryModeApplication)
	}{
		{"empty", func(a *ProbeDeliveryModeApplication) { *a = ProbeDeliveryModeApplication{} }},
		{"hub", func(a *ProbeDeliveryModeApplication) { a.HubID = "local" }},
		{"probe", func(a *ProbeDeliveryModeApplication) { a.ProbeID = "" }},
		{"monitor", func(a *ProbeDeliveryModeApplication) { a.MonitorID = 0 }},
		{"assignment revision", func(a *ProbeDeliveryModeApplication) { a.AssignmentRevision = 0 }},
		{"generation", func(a *ProbeDeliveryModeApplication) { a.Generation = -1 }},
		{"config revision", func(a *ProbeDeliveryModeApplication) { a.ConfigRevision = 0 }},
		{"short hash", func(a *ProbeDeliveryModeApplication) { a.SHA256 = "a" }},
		{"nonhex hash", func(a *ProbeDeliveryModeApplication) { a.SHA256 = strings.Repeat("g", 64) }},
		{"noncanonical hash", func(a *ProbeDeliveryModeApplication) { a.SHA256 = strings.Repeat("A", 64) }},
		{"empty stream", func(a *ProbeDeliveryModeApplication) { a.StreamID = "" }},
		{"malformed stream", func(a *ProbeDeliveryModeApplication) { a.StreamID = "current" }},
		{"zero stream", func(a *ProbeDeliveryModeApplication) { a.StreamID = "00000000-0000-0000-0000-000000000000" }},
		{"implicit mode", func(a *ProbeDeliveryModeApplication) { a.AlertDelivery = "" }},
		{"unknown mode", func(a *ProbeDeliveryModeApplication) { a.AlertDelivery = "future" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := valid
			tc.change(&a)
			if ValidProbeDeliveryModeApplication(a) {
				t.Fatal("malformed attestation shape accepted")
			}
		})
	}
}
