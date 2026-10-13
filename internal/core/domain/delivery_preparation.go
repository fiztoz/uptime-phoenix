package domain

import "time"

// ProbeDeliveryPreparation requests preparation only, never mode application.
type ProbeDeliveryPreparation struct {
	ProbeDeliveryModeApplication
	CommandID   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	PayloadHash [32]byte
}

// LocalDeliveryPreparation records a protected local pending request, not an
// effective application, lease, suppression decision or provider-I/O permit.
// It has no remote command/session identity or expiry authority.
type LocalDeliveryPreparation struct {
	ProbeDeliveryModeApplication
	PreparedAt time.Time
}

// ValidLocalDeliveryPreparation validates shape only, never current eligibility.
func ValidLocalDeliveryPreparation(p LocalDeliveryPreparation) bool {
	return ValidProbeDeliveryModeApplication(p.ProbeDeliveryModeApplication) &&
		p.ProbeID == LocalProbeID && p.StreamID == LocalStreamID &&
		(p.AlertDelivery == AlertDeliveryAggregate || p.AlertDelivery == AlertDeliveryBoth) &&
		!p.PreparedAt.IsZero() && p.PreparedAt.Location() == time.UTC &&
		p.PreparedAt.Nanosecond()%1000 == 0
}

// ValidProbeDeliveryPreparation validates a remote, exactly bound request shape.
func ValidProbeDeliveryPreparation(c ProbeDeliveryPreparation) bool {
	return ValidProbeDeliveryModeApplication(c.ProbeDeliveryModeApplication) &&
		ValidHubID(c.CommandID) && ValidHubID(c.ProbeID) && c.StreamID != LocalStreamID &&
		(c.AlertDelivery == AlertDeliveryAggregate || c.AlertDelivery == AlertDeliveryBoth) &&
		c.PayloadHash != [32]byte{} && !c.CreatedAt.IsZero() && c.ExpiresAt.After(c.CreatedAt) &&
		c.ExpiresAt.Sub(c.CreatedAt) <= 7*24*time.Hour &&
		c.CreatedAt.Nanosecond()%1000 == 0 && c.ExpiresAt.Nanosecond()%1000 == 0
}
