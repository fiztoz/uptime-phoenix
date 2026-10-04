package probe

import "errors"

// MaxHealthErrors bounds redacted diagnostic codes in a health frame.
const MaxHealthErrors = 32

// Health reports role-specific durable progress and readiness. It cannot replace
// a telemetry ACK, a configuration receipt, or a current monitor observation.
type Health struct {
	Role             string     `json:"role"`
	Ready            bool       `json:"ready"`
	DBWritable       bool       `json:"db_writable"`
	SchedulerHealthy *bool      `json:"scheduler_healthy"`
	ConfigRevision   Decimal    `json:"config_revision"`
	CommittedSeq     Decimal    `json:"committed_seq"`
	QueueBytes       *int64     `json:"queue_bytes"`
	OldestQueuedAt   *Timestamp `json:"oldest_queued_at"`
	ClockTime        Timestamp  `json:"clock_time"`
	Errors           []string   `json:"errors"`
}

// DecodeHealth validates role/nullability/readiness coherence and bounded codes.
// The session must also check the expected sender role and connection generation.
func DecodeHealth(data []byte) (Envelope, Health, error) {
	var health Health
	envelope, fields, err := telemetryFields(data, "health")
	if err != nil {
		return envelope, health, err
	}
	if err := decodeRequiredFields(fields,
		field{"role", &health.Role}, field{"ready", &health.Ready}, field{"db_writable", &health.DBWritable},
		field{"config_revision", &health.ConfigRevision}, field{"committed_seq", &health.CommittedSeq},
		field{"clock_time", &health.ClockTime}, field{"errors", &health.Errors},
	); err != nil {
		return envelope, health, err
	}
	if err := nullable(fields, "scheduler_healthy", &health.SchedulerHealthy); err != nil {
		return envelope, health, err
	}
	if err := nullable(fields, "queue_bytes", &health.QueueBytes); err != nil {
		return envelope, health, err
	}
	if err := nullable(fields, "oldest_queued_at", &health.OldestQueuedAt); err != nil {
		return envelope, health, err
	}
	switch health.Role {
	case "probe":
		if health.SchedulerHealthy == nil || health.QueueBytes == nil || *health.QueueBytes < 0 {
			return envelope, health, errors.New("probe requires scheduler health and nonnegative queue bytes")
		}
		if (*health.QueueBytes == 0) != (health.OldestQueuedAt == nil) {
			return envelope, health, errors.New("probe queue bytes and oldest time disagree")
		}
		if health.Ready && (!*health.SchedulerHealthy || health.ConfigRevision == 0) {
			return envelope, health, errors.New("ready probe requires scheduling and active config")
		}
	case "hub":
		if health.QueueBytes != nil || health.OldestQueuedAt != nil {
			return envelope, health, errors.New("hub telemetry queue fields must be null")
		}
	default:
		return envelope, health, errors.New("invalid health role")
	}
	if health.Ready && !health.DBWritable {
		return envelope, health, errors.New("ready sender requires writable persistence")
	}
	if len(health.Errors) > MaxHealthErrors {
		return envelope, health, errors.New("too many health error codes")
	}
	seen := make(map[string]struct{}, len(health.Errors))
	for _, code := range health.Errors {
		if !validErrorCode(code) {
			return envelope, health, errors.New("invalid health error code")
		}
		if _, exists := seen[code]; exists {
			return envelope, health, errors.New("duplicate health error code")
		}
		seen[code] = struct{}{}
	}
	return envelope, health, nil
}
