package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	// MaxBatchBytes includes the complete telemetry.batch envelope.
	MaxBatchBytes = 512 << 10
	// MaxBatchEvents bounds both batch events and acknowledgement outcomes.
	MaxBatchEvents = 256
	// MaxEventBytes bounds each serialized event before decoding its contents.
	MaxEventBytes = 64 << 10
	// MaxMessageBytes bounds redacted observation and condition messages.
	MaxMessageBytes = 4096
	// MaxMetadataBytes bounds each condition metadata string and TLS issuer.
	MaxMetadataBytes = 256
	// MaxErrorCodeBytes bounds machine-readable rejection codes.
	MaxErrorCodeBytes = 128
	// MaxGapMonitorIDs bounds explicit coverage in a gap; empty means unknown.
	MaxGapMonitorIDs = 256
)

// ErrUnsupportedPayload means a documented message/event has no implemented
// typed decoder in this staged slice. Callers must not acknowledge it as accepted.
var ErrUnsupportedPayload = errors.New("payload decoder is not implemented")

// TelemetryBatch contains only observations in the initial executable slice.
// Other V1 event kinds are explicitly rejected until their typed decoders exist.
type TelemetryBatch struct {
	StreamID string             `json:"stream_id"`
	FirstSeq Decimal            `json:"first_seq"`
	LastSeq  Decimal            `json:"last_seq"`
	Events   []ObservationEvent `json:"events"`
}

// ObservationEvent is immutable source evidence identified by stream and sequence.
type ObservationEvent struct {
	Seq        Decimal     `json:"seq"`
	Kind       string      `json:"kind"`
	ObservedAt Timestamp   `json:"observed_at"`
	Data       Observation `json:"data"`
}

// Observation preserves source retry evaluation without generating hub evidence.
type Observation struct {
	MonitorID            int64                  `json:"monitor_id"`
	AssignmentGeneration Decimal                `json:"assignment_generation"`
	ConfigRevision       Decimal                `json:"config_revision"`
	Status               string                 `json:"status"`
	RawStatus            string                 `json:"raw_status"`
	DownCount            int64                  `json:"down_count"`
	Ping                 int64                  `json:"ping"`
	DurationMS           int64                  `json:"duration_ms"`
	Message              string                 `json:"message"`
	Important            bool                   `json:"important"`
	Conditions           []ConditionObservation `json:"conditions"`
	TLS                  *TLSObservation        `json:"tls"`
}

// ConditionObservation carries raw checker values, not promoted condition state.
// Evaluated state snapshots need a separate schema and are not implemented here.
// The wire builder must derive StaleAfter; checker results may leave it unset.
type ConditionObservation struct {
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Message    string    `json:"message"`
	Used       *float64  `json:"used"`
	Limit      *float64  `json:"limit"`
	Percent    *float64  `json:"percent"`
	Threshold  *float64  `json:"threshold"`
	Unit       string    `json:"unit"`
	Resource   string    `json:"resource"`
	Scope      string    `json:"scope"`
	Source     string    `json:"source"`
	ObservedAt Timestamp `json:"observed_at"`
	StaleAfter Timestamp `json:"stale_after"`
}

// TLSObservation exposes sanitized certificate health without raw certificate data.
type TLSObservation struct {
	NotAfter      Timestamp `json:"not_after"`
	DaysRemaining int64     `json:"days_remaining"`
	Issuer        string    `json:"issuer"`
}

// TelemetryACK describes durable outcomes; decoding does not establish durability.
type TelemetryACK struct {
	StreamID       string             `json:"stream_id"`
	CommittedSeq   Decimal            `json:"committed_seq"`
	AcceptedCount  int64              `json:"accepted_count"`
	DuplicateCount int64              `json:"duplicate_count"`
	Rejected       []RejectedSequence `json:"rejected"`
}

// RejectedSequence records a permanent rejection at an acknowledged sequence.
type RejectedSequence struct {
	Seq  Decimal `json:"seq"`
	Code string  `json:"code"`
}

// TelemetryRetry requests backoff without advancing the durable cursor.
type TelemetryRetry struct {
	StreamID     string  `json:"stream_id"`
	CommittedSeq Decimal `json:"committed_seq"`
	RetryAfterMS int64   `json:"retry_after_ms"`
}

// TelemetryGap explicitly identifies unavailable history, never successful checks.
type TelemetryGap struct {
	StreamID           string    `json:"stream_id"`
	FromSeq            Decimal   `json:"from_seq"`
	ThroughSeq         Decimal   `json:"through_seq"`
	Reason             string    `json:"reason"`
	ObservedFrom       Timestamp `json:"observed_from"`
	ObservedThrough    Timestamp `json:"observed_through"`
	AffectedMonitorIDs []int64   `json:"affected_monitor_ids"`
}

// DecodeTelemetryBatch validates complete framing and observation payloads.
// Assignment authorization, configured retries, clock bounds, and durable cursor
// continuity require service state and must be checked before committing evidence.
func DecodeTelemetryBatch(data []byte) (Envelope, TelemetryBatch, error) {
	var batch TelemetryBatch
	if len(data) > MaxBatchBytes {
		return Envelope{}, batch, errors.New("telemetry batch exceeds maximum bytes")
	}
	envelope, fields, err := telemetryFields(data, "telemetry.batch")
	if err != nil {
		return envelope, batch, err
	}
	if err := requiredUUID(fields, "stream_id", &batch.StreamID); err != nil {
		return envelope, batch, err
	}
	if err := required(fields, "first_seq", &batch.FirstSeq); err != nil {
		return envelope, batch, err
	}
	if err := required(fields, "last_seq", &batch.LastSeq); err != nil {
		return envelope, batch, err
	}
	var events []json.RawMessage
	if err := required(fields, "events", &events); err != nil {
		return envelope, batch, err
	}
	if len(events) == 0 || len(events) > MaxBatchEvents {
		return envelope, batch, errors.New("batch requires 1 to 256 events")
	}
	if batch.FirstSeq <= 0 || batch.LastSeq < batch.FirstSeq || int64(batch.LastSeq-batch.FirstSeq) != int64(len(events)-1) {
		return envelope, batch, errors.New("batch sequence bounds must exactly cover its events")
	}
	batch.Events = make([]ObservationEvent, 0, len(events))
	for index, raw := range events {
		event, err := decodeObservationEvent(raw)
		if err != nil {
			return envelope, TelemetryBatch{}, fmt.Errorf("events[%d]: %w", index, err)
		}
		if event.Seq != batch.FirstSeq+Decimal(index) {
			return envelope, TelemetryBatch{}, errors.New("event sequences must be contiguous and ordered")
		}
		batch.Events = append(batch.Events, event)
	}
	return envelope, batch, nil
}

func decodeObservationEvent(data []byte) (ObservationEvent, error) {
	var event ObservationEvent
	if len(data) > MaxEventBytes {
		return event, errors.New("event exceeds maximum bytes")
	}
	fields, err := objectFields(data)
	if err != nil {
		return event, err
	}
	if err := required(fields, "seq", &event.Seq); err != nil {
		return event, err
	}
	if event.Seq <= 0 {
		return event, errors.New("event seq must be positive")
	}
	if err := required(fields, "kind", &event.Kind); err != nil {
		return event, err
	}
	if event.Kind != "observation" {
		return event, fmt.Errorf("unsupported telemetry event kind: %w", ErrUnsupportedPayload)
	}
	if err := required(fields, "observed_at", &event.ObservedAt); err != nil {
		return event, err
	}
	var raw json.RawMessage
	if err := required(fields, "data", &raw); err != nil {
		return event, err
	}
	event.Data, err = decodeObservation(raw)
	return event, err
}

func decodeObservation(data []byte) (Observation, error) {
	var observation Observation
	fields, err := objectFields(data)
	if err != nil {
		return observation, err
	}
	if err := decodeRequiredFields(fields,
		field{"monitor_id", &observation.MonitorID},
		field{"assignment_generation", &observation.AssignmentGeneration},
		field{"config_revision", &observation.ConfigRevision},
		field{"status", &observation.Status},
		field{"raw_status", &observation.RawStatus},
		field{"down_count", &observation.DownCount},
		field{"ping", &observation.Ping},
		field{"duration_ms", &observation.DurationMS},
		field{"message", &observation.Message},
		field{"important", &observation.Important},
	); err != nil {
		return observation, err
	}
	if observation.MonitorID <= 0 || observation.AssignmentGeneration <= 0 || observation.ConfigRevision <= 0 {
		return observation, errors.New("monitor identity and assignment/config generations must be positive")
	}
	if observation.DownCount < 0 || observation.Ping < 0 || observation.DurationMS < 0 {
		return observation, errors.New("observation counts and latencies must be nonnegative")
	}
	if len(observation.Message) > MaxMessageBytes {
		return observation, errors.New("observation message exceeds maximum bytes")
	}
	switch observation.RawStatus {
	case "UP", "PENDING", "MAINTENANCE":
		if observation.Status != observation.RawStatus || observation.DownCount != 0 {
			return observation, errors.New("non-DOWN raw status requires matching status and zero down_count")
		}
	case "DOWN":
		if observation.Status != "DOWN" && observation.Status != "PENDING" || observation.DownCount == 0 {
			return observation, errors.New("raw DOWN requires DOWN/PENDING status and positive down_count")
		}
	default:
		return observation, errors.New("invalid raw observation status")
	}
	var conditions []json.RawMessage
	if err := required(fields, "conditions", &conditions); err != nil {
		return observation, err
	}
	if len(conditions) > 2 {
		return observation, errors.New("at most two distinct condition kinds are supported")
	}
	observation.Conditions = make([]ConditionObservation, 0, len(conditions))
	seen := make(map[string]struct{}, len(conditions))
	for _, raw := range conditions {
		condition, err := decodeCondition(raw)
		if err != nil {
			return observation, fmt.Errorf("condition: %w", err)
		}
		if _, duplicate := seen[condition.Kind]; duplicate {
			return observation, errors.New("duplicate condition kind")
		}
		seen[condition.Kind] = struct{}{}
		observation.Conditions = append(observation.Conditions, condition)
	}
	var tlsRaw *json.RawMessage
	if err := nullable(fields, "tls", &tlsRaw); err != nil {
		return observation, err
	}
	if tlsRaw != nil {
		tls, err := decodeTLS(*tlsRaw)
		if err != nil {
			return observation, fmt.Errorf("tls: %w", err)
		}
		observation.TLS = &tls
	}
	return observation, nil
}

func decodeCondition(data []byte) (ConditionObservation, error) {
	var condition ConditionObservation
	fields, err := objectFields(data)
	if err != nil {
		return condition, err
	}
	if err := decodeRequiredFields(fields,
		field{"kind", &condition.Kind}, field{"state", &condition.State},
		field{"message", &condition.Message}, field{"unit", &condition.Unit},
		field{"resource", &condition.Resource}, field{"scope", &condition.Scope},
		field{"source", &condition.Source}, field{"observed_at", &condition.ObservedAt},
		field{"stale_after", &condition.StaleAfter},
	); err != nil {
		return condition, err
	}
	if condition.Kind != "session_pool" && condition.Kind != "storage" {
		return condition, errors.New("invalid condition kind")
	}
	if condition.State != "ok" && condition.State != "warning" && condition.State != "error" {
		return condition, errors.New("invalid raw condition state")
	}
	for _, metric := range []struct {
		name   string
		target **float64
	}{
		{"used", &condition.Used}, {"limit", &condition.Limit},
		{"percent", &condition.Percent}, {"threshold", &condition.Threshold},
	} {
		if err := nullable(fields, metric.name, metric.target); err != nil {
			return condition, err
		}
		if value := *metric.target; value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return condition, fmt.Errorf("%s must be finite and nonnegative when present", metric.name)
		}
	}
	if len(condition.Message) > MaxMessageBytes {
		return condition, errors.New("condition message exceeds maximum bytes")
	}
	for _, metadata := range []string{condition.Unit, condition.Resource, condition.Scope, condition.Source} {
		if len(metadata) > MaxMetadataBytes {
			return condition, errors.New("condition metadata exceeds maximum bytes")
		}
	}
	if time.Time(condition.StaleAfter).Before(time.Time(condition.ObservedAt)) {
		return condition, errors.New("condition stale_after precedes observed_at")
	}
	return condition, nil
}

func decodeTLS(data []byte) (TLSObservation, error) {
	var tls TLSObservation
	fields, err := objectFields(data)
	if err != nil {
		return tls, err
	}
	if err := decodeRequiredFields(fields,
		field{"not_after", &tls.NotAfter}, field{"days_remaining", &tls.DaysRemaining}, field{"issuer", &tls.Issuer},
	); err != nil {
		return tls, err
	}
	if len(tls.Issuer) > MaxMetadataBytes {
		return tls, errors.New("TLS issuer exceeds maximum bytes")
	}
	return tls, nil
}

// DecodeTelemetryACK validates an ACK shape, not whether a transaction committed.
// The receiving service must also correlate it with its stream and in-flight batch.
func DecodeTelemetryACK(data []byte) (Envelope, TelemetryACK, error) {
	var ack TelemetryACK
	envelope, fields, err := telemetryFields(data, "telemetry.ack")
	if err != nil {
		return envelope, ack, err
	}
	if err := requiredUUID(fields, "stream_id", &ack.StreamID); err != nil {
		return envelope, ack, err
	}
	if err := decodeRequiredFields(fields,
		field{"committed_seq", &ack.CommittedSeq}, field{"accepted_count", &ack.AcceptedCount}, field{"duplicate_count", &ack.DuplicateCount},
	); err != nil {
		return envelope, ack, err
	}
	var rejected []json.RawMessage
	if err := required(fields, "rejected", &rejected); err != nil {
		return envelope, ack, err
	}
	if ack.AcceptedCount < 0 || ack.AcceptedCount > MaxBatchEvents || ack.DuplicateCount < 0 || ack.DuplicateCount > MaxBatchEvents || len(rejected) > MaxBatchEvents || ack.AcceptedCount+ack.DuplicateCount+int64(len(rejected)) > MaxBatchEvents {
		return envelope, ack, errors.New("ACK outcomes exceed batch bounds")
	}
	ack.Rejected = make([]RejectedSequence, 0, len(rejected))
	seen := make(map[Decimal]struct{}, len(rejected))
	for _, raw := range rejected {
		fields, err := objectFields(raw)
		if err != nil {
			return envelope, ack, err
		}
		var rejection RejectedSequence
		if err := decodeRequiredFields(fields, field{"seq", &rejection.Seq}, field{"code", &rejection.Code}); err != nil {
			return envelope, ack, err
		}
		if rejection.Seq <= 0 || rejection.Seq > ack.CommittedSeq || !validErrorCode(rejection.Code) {
			return envelope, ack, errors.New("invalid rejected sequence or error code")
		}
		if _, duplicate := seen[rejection.Seq]; duplicate {
			return envelope, ack, errors.New("duplicate rejected sequence")
		}
		seen[rejection.Seq] = struct{}{}
		ack.Rejected = append(ack.Rejected, rejection)
	}
	if ack.CommittedSeq == 0 && (ack.AcceptedCount != 0 || ack.DuplicateCount != 0) {
		return envelope, ack, errors.New("initial cursor cannot acknowledge durable events")
	}
	return envelope, ack, nil
}

// DecodeTelemetryRetry validates retry framing without scheduling a timer.
func DecodeTelemetryRetry(data []byte) (Envelope, TelemetryRetry, error) {
	var retry TelemetryRetry
	envelope, fields, err := telemetryFields(data, "telemetry.retry")
	if err != nil {
		return envelope, retry, err
	}
	if err := requiredUUID(fields, "stream_id", &retry.StreamID); err != nil {
		return envelope, retry, err
	}
	if err := decodeRequiredFields(fields,
		field{"committed_seq", &retry.CommittedSeq}, field{"retry_after_ms", &retry.RetryAfterMS},
	); err != nil {
		return envelope, retry, err
	}
	if retry.RetryAfterMS <= 0 {
		return envelope, retry, errors.New("retry_after_ms must be positive")
	}
	return envelope, retry, nil
}

// DecodeTelemetryGap validates loss-range framing. Only an authorized transaction
// can determine whether it overlaps the next cursor or may clear any history.
func DecodeTelemetryGap(data []byte) (Envelope, TelemetryGap, error) {
	var gap TelemetryGap
	envelope, fields, err := telemetryFields(data, "telemetry.gap")
	if err != nil {
		return envelope, gap, err
	}
	if err := requiredUUID(fields, "stream_id", &gap.StreamID); err != nil {
		return envelope, gap, err
	}
	if err := decodeRequiredFields(fields,
		field{"from_seq", &gap.FromSeq}, field{"through_seq", &gap.ThroughSeq},
		field{"reason", &gap.Reason}, field{"observed_from", &gap.ObservedFrom},
		field{"observed_through", &gap.ObservedThrough}, field{"affected_monitor_ids", &gap.AffectedMonitorIDs},
	); err != nil {
		return envelope, gap, err
	}
	if gap.FromSeq <= 0 || gap.ThroughSeq < gap.FromSeq {
		return envelope, gap, errors.New("gap sequence range must be positive and ordered")
	}
	switch gap.Reason {
	case "retention_bytes", "retention_age", "disk_pressure", "restore_loss", "history_cleared":
	default:
		return envelope, gap, errors.New("invalid gap reason")
	}
	if time.Time(gap.ObservedThrough).Before(time.Time(gap.ObservedFrom)) {
		return envelope, gap, errors.New("gap observation range must be ordered")
	}
	if len(gap.AffectedMonitorIDs) > MaxGapMonitorIDs {
		return envelope, gap, errors.New("gap exceeds maximum monitor IDs")
	}
	seen := make(map[int64]struct{}, len(gap.AffectedMonitorIDs))
	for _, monitorID := range gap.AffectedMonitorIDs {
		if monitorID <= 0 {
			return envelope, gap, errors.New("gap monitor IDs must be positive")
		}
		if _, duplicate := seen[monitorID]; duplicate {
			return envelope, gap, errors.New("duplicate gap monitor ID")
		}
		seen[monitorID] = struct{}{}
	}
	return envelope, gap, nil
}

func telemetryFields(data []byte, expected string) (Envelope, map[string]json.RawMessage, error) {
	envelope, err := DecodeEnvelope(data)
	if err != nil {
		return envelope, nil, err
	}
	if envelope.Type != expected {
		return envelope, nil, fmt.Errorf("expected %s payload: %w", expected, ErrUnsupportedPayload)
	}
	fields, err := objectFields(envelope.Payload)
	return envelope, fields, err
}

type field struct {
	name   string
	target any // Explicit typed DTO destinations supplied by each decoder.
}

func decodeRequiredFields(fields map[string]json.RawMessage, destinations ...field) error {
	for _, destination := range destinations {
		var raw json.RawMessage
		if err := required(fields, destination.name, &raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, destination.target); err != nil {
			return fmt.Errorf("%s: %w", destination.name, err)
		}
	}
	return nil
}

func validErrorCode(code string) bool {
	if len(code) == 0 || len(code) > MaxErrorCodeBytes {
		return false
	}
	for _, character := range code {
		if character != '_' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
