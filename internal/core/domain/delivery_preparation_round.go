package domain

import "time"

// DeliveryPreparationRound is immutable all-source request membership, not an
// application barrier, readiness fact or provider-send/expiry permit.
type DeliveryPreparationRound struct {
	HubID              string
	RoundID            string
	MonitorID          int64
	RequestRound       int64
	AssignmentRevision int64
	AlertDelivery      AlertDelivery
	CreatedAt          time.Time
	ExpiresAt          time.Time
	Members            []DeliveryPreparationMember
}

// DeliveryPreparationMember retains every assigned source and generation.
type DeliveryPreparationMember struct {
	ProbeID    string
	Generation int64
}

// ValidDeliveryPreparationRound checks bounded shape only, never authority.
func ValidDeliveryPreparationRound(r DeliveryPreparationRound) bool {
	if !ValidHubID(r.HubID) || !ValidHubID(r.RoundID) || r.MonitorID <= 0 || r.RequestRound <= 0 || r.AssignmentRevision <= 0 ||
		(r.AlertDelivery != AlertDeliveryAggregate && r.AlertDelivery != AlertDeliveryBoth) ||
		r.CreatedAt.IsZero() || r.CreatedAt.Location() != time.UTC || r.ExpiresAt.Location() != time.UTC ||
		!r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) != 24*time.Hour ||
		r.CreatedAt.Nanosecond()%1000 != 0 || r.ExpiresAt.Nanosecond()%1000 != 0 || len(r.Members) == 0 {
		return false
	}
	seen := make(map[string]bool, len(r.Members))
	for _, m := range r.Members {
		if (m.ProbeID != LocalProbeID && !ValidHubID(m.ProbeID)) || m.Generation <= 0 || seen[m.ProbeID] {
			return false
		}
		seen[m.ProbeID] = true
	}
	return true
}
