package domain

import "time"

// DeliveryCutoverCommand names a protected exact source-gate effect.
type DeliveryCutoverCommand struct {
	ProbeDeliveryModeApplication
	CommandID, BarrierID, PreparationCommandID, Kind string
	CreatedAt, ExpiresAt                             time.Time
	PayloadHash                                      [32]byte
}

const (
	DeliveryQuiesceKind       = "delivery.quiesce"
	DeliveryApplyKind         = "delivery.apply"
	DeliveryResumeKind        = "delivery.resume"
	DeliveryQuiescedCode      = "delivery_quiesced"
	DeliveryAppliedCode       = "delivery_applied"
	DeliveryResumedCode       = "delivery_resumed"
	DeliveryCutoverCapability = "availability.delivery_cutover.v1"
)

// DeliveryCutoverSuccessCode is kind-specific and never preparation success.
func DeliveryCutoverSuccessCode(kind string) string {
	switch kind {
	case DeliveryQuiesceKind:
		return DeliveryQuiescedCode
	case DeliveryApplyKind:
		return DeliveryAppliedCode
	case DeliveryResumeKind:
		return DeliveryResumedCode
	}
	return ""
}

// ValidDeliveryCutoverCommand validates shape, not authority or runtime support.
func ValidDeliveryCutoverCommand(c DeliveryCutoverCommand) bool {
	return ValidProbeDeliveryModeApplication(c.ProbeDeliveryModeApplication) &&
		c.ProbeID != LocalProbeID && configUUID(c.CommandID) && configUUID(c.BarrierID) &&
		configUUID(c.PreparationCommandID) && DeliveryCutoverSuccessCode(c.Kind) != "" &&
		(c.AlertDelivery == AlertDeliveryAggregate || c.AlertDelivery == AlertDeliveryBoth) &&
		!c.CreatedAt.IsZero() && c.ExpiresAt.After(c.CreatedAt) &&
		c.ExpiresAt.Sub(c.CreatedAt) <= 24*time.Hour
}

// ProviderAttemptState is an I/O fact, never implied by lease expiry.
type ProviderAttemptState string

const (
	ProviderAttemptInFlight   ProviderAttemptState = "in_flight"
	ProviderAttemptSucceeded  ProviderAttemptState = "succeeded"
	ProviderAttemptAmbiguous  ProviderAttemptState = "ambiguous"
	ProviderAttemptNotStarted ProviderAttemptState = "not_started"
)

// AggregateCutoverProgress exposes bounded coordinator progress, not permission.
type AggregateCutoverProgress struct {
	MonitorID        int64
	BarrierID, Phase string
	Changed          bool
}

// AggregateSourceSummary is nonsecret current evidence rendered in aggregate pages.
type AggregateSourceSummary struct {
	ProbeID, Name, Location string
	Status                  Status
}

// AggregateProviderGrant is an internal authenticated stored-work capture.
// Its provider settings must never be returned through an HTTP View or logged.
type AggregateProviderGrant struct {
	AttemptID     string
	Delivery      QueuedDelivery
	Monitor       *Monitor
	Notification  *Notification
	Template      *NotificationTemplate
	IncludeTarget bool
	Regions       []AggregateSourceSummary
	Policy        HealthPolicy
}

// LocalProviderGrant carries the exact whole-source graph proved during final
// authorization, not a second read's settings.
type LocalProviderGrant struct {
	AttemptID string
	Delivery  QueuedDelivery
	Config    *LocalProbeConfigDefinition
}
