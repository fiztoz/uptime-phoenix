package domain

import (
	"math"
	"testing"
	"time"
)

func TestDeliveryPreparationRoundShape(t *testing.T) {
	now := time.Unix(1700000000, 123000).UTC()
	valid := DeliveryPreparationRound{HubID: "11111111-1111-4111-8111-111111111111", RoundID: "33333333-3333-4333-8333-333333333333", MonitorID: 1, RequestRound: 9007199254740993, AssignmentRevision: math.MaxInt64, AlertDelivery: AlertDeliveryBoth, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), Members: []DeliveryPreparationMember{{ProbeID: LocalProbeID, Generation: 1}, {ProbeID: "22222222-2222-4222-8222-222222222222", Generation: 2}}}
	if !ValidDeliveryPreparationRound(valid) {
		t.Fatal("exact large-counter round refused")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DeliveryPreparationRound)
	}{
		{"round", func(r *DeliveryPreparationRound) { r.RequestRound = 0 }},
		{"set", func(r *DeliveryPreparationRound) { r.AssignmentRevision = 0 }},
		{"regional", func(r *DeliveryPreparationRound) { r.AlertDelivery = AlertDeliveryRegional }},
		{"empty", func(r *DeliveryPreparationRound) { r.Members = nil }},
		{"duplicate", func(r *DeliveryPreparationRound) { r.Members = append(r.Members, r.Members[0]) }},
		{"foreign", func(r *DeliveryPreparationRound) { r.Members[0].ProbeID = "foreign" }},
		{"generation", func(r *DeliveryPreparationRound) { r.Members[0].Generation = 0 }},
		{"expiry", func(r *DeliveryPreparationRound) { r.ExpiresAt = r.CreatedAt }},
		{"duration", func(r *DeliveryPreparationRound) { r.ExpiresAt = r.CreatedAt.Add(48 * time.Hour) }},
		{"precision", func(r *DeliveryPreparationRound) {
			r.CreatedAt = r.CreatedAt.Add(time.Nanosecond)
			r.ExpiresAt = r.CreatedAt.Add(24 * time.Hour)
		}},
		{"timezone", func(r *DeliveryPreparationRound) { r.CreatedAt = r.CreatedAt.In(time.FixedZone("offset", 3600)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			r.Members = append([]DeliveryPreparationMember(nil), valid.Members...)
			tc.mutate(&r)
			if ValidDeliveryPreparationRound(r) {
				t.Fatal("malformed round accepted")
			}
		})
	}
}
