package domain

import "time"

const (
	DeliveryStatusPending = "pending"
	DeliveryStatusLeased  = "leased"

	ErrCodeNetworkTimeout      = "network_timeout"
	ErrCodeConnectionRefused   = "connection_refused"
	ErrCodeRateLimited         = "rate_limited"
	ErrCodeAuthFailed          = "auth_failed"
	ErrCodeBadRequest          = "bad_request"
	ErrCodeProviderServerError = "provider_server_error"
	ErrCodeUnknownSenderType   = "unknown_sender_type"
	ErrCodeProviderError       = "provider_error"
)

// DeliveryIntent reserves a stable source delivery identity for one channel
// version. It contains no credentials or rendered provider request. Availability
// context is captured by the recorder from the same observation and incident.
type DeliveryIntent struct {
	DeliveryID              string
	SourceAlertID           string
	SourceTransitionVersion int64
	ProbeID                 string
	NotificationID          int64
	NotificationVersion     int64
	EventKind               string
	AvailableAt             time.Time
}

// QueuedDelivery is durable source work, distinct from a mirrored delivery
// outcome. Snapshot fields are immutable, even after the incident advances or
// observation history is pruned. A lease alone does not authorize provider I/O:
// a consumer must revalidate assignment, lifecycle, and channel version first.
type QueuedDelivery struct {
	DeliveryIntent
	MonitorID            int64
	AssignmentGeneration int64
	StreamID             string
	SourceSeq            int64
	ConfigRevision       int64
	CheckStatus          Status
	CheckOutput          string
	ObservedAt           time.Time
	IncidentStatus       string
	StartedAt            time.Time
	ResolvedAt           *time.Time
	Status               string
	Attempt              int64
	LeaseToken           string
	LeasedAt             *time.Time
	LeaseUntil           *time.Time
	ErrorCode            string
	OutcomeAt            *time.Time
	CreatedAt            time.Time
}

// DeliveryClaim is the persisted attempt fence. Copy these fields from a claim;
// completion derives delivery identity and channel metadata from stored work.
type DeliveryClaim struct {
	DeliveryID string
	ProbeID    string
	Attempt    int64
	LeaseToken string
}

// DeliveryResult completes a leased attempt. Retrying requires RetryAt > At;
// terminal outcomes leave RetryAt zero. ErrorCode is a bounded diagnostic code,
// never raw provider output. Times cross persistence boundaries in UTC.
type DeliveryResult struct {
	Status    string
	ErrorCode string
	At        time.Time
	RetryAt   time.Time
}
