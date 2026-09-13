// Package probe defines the bounded wire contracts for the staged probe protocol.
// It does not register routes, establish sessions, or enable remote execution.
package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxFrameBytes limits a decoded, uncompressed application frame.
	MaxFrameBytes = 1 << 20
	// MaxJSONDepth bounds nesting, including unknown optional fields.
	MaxJSONDepth = 64
)

// Decimal is a nonnegative signed-64-bit counter encoded as a decimal string.
// Durable sequences and negotiated generations additionally require positivity.
type Decimal int64

// UnmarshalJSON rejects JSON numbers, signs, leading zeros, and overflow.
func (d *Decimal) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decoding decimal string: %w", err)
	}
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return errors.New("decimal must be a canonical nonnegative string")
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return errors.New("decimal must contain only ASCII digits")
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("decimal exceeds signed 64-bit range: %w", err)
	}
	*d = Decimal(parsed)
	return nil
}

// MarshalJSON serializes the counter without a floating-point conversion.
func (d Decimal) MarshalJSON() ([]byte, error) {
	if d < 0 {
		return nil, errors.New("decimal must be nonnegative")
	}
	return []byte(`"` + strconv.FormatInt(int64(d), 10) + `"`), nil
}

// Timestamp is an RFC3339 timestamp whose wire representation is UTC with Z.
type Timestamp time.Time

// UnmarshalJSON requires an explicit UTC Z suffix, allowing fractional seconds.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decoding timestamp string: %w", err)
	}
	if !strings.HasSuffix(value, "Z") {
		return errors.New("timestamp must use UTC Z suffix")
	}
	if !canonicalTimestampShape(value) {
		return errors.New("timestamp must have RFC3339 date/time fields and optional decimal fraction")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fmt.Errorf("decoding RFC3339 timestamp: %w", err)
	}
	*t = Timestamp(parsed.UTC())
	return nil
}

func canonicalTimestampShape(value string) bool {
	if len(value) < 20 {
		return false
	}
	for index := 0; index < 19; index++ {
		want := byte(0)
		switch index {
		case 4, 7:
			want = '-'
		case 10:
			want = 'T'
		case 13, 16:
			want = ':'
		}
		if want != 0 && value[index] != want || want == 0 && (value[index] < '0' || value[index] > '9') {
			return false
		}
	}
	if len(value) == 20 {
		return value[19] == 'Z'
	}
	if len(value) < 22 || value[19] != '.' {
		return false
	}
	for index := 20; index < len(value)-1; index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// MarshalJSON normalizes a timestamp to UTC and preserves nanosecond precision.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return time.Time(t).UTC().MarshalJSON()
}

// Envelope is the common application frame. Payload remains unvalidated here;
// callers must select a supported typed decoder before acting on its contents.
type Envelope struct {
	ProtocolVersion      int             `json:"protocol_version"`
	Type                 string          `json:"type"`
	MessageID            string          `json:"message_id"`
	SentAt               Timestamp       `json:"sent_at"`
	ConnectionGeneration Decimal         `json:"connection_generation"`
	Payload              json.RawMessage `json:"payload"`
}

// DecodeEnvelope validates framing and known message names, not payload schemas
// or authenticated session identity. Unknown optional fields remain compatible.
func DecodeEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if len(data) > MaxFrameBytes {
		return envelope, errors.New("frame exceeds maximum bytes")
	}
	if err := validateJSON(data); err != nil {
		return envelope, err
	}
	fields, err := objectFields(data)
	if err != nil {
		return envelope, err
	}
	if err := required(fields, "protocol_version", &envelope.ProtocolVersion); err != nil {
		return envelope, err
	}
	if envelope.ProtocolVersion != 1 {
		return envelope, errors.New("unsupported protocol_version")
	}
	if err := required(fields, "type", &envelope.Type); err != nil {
		return envelope, err
	}
	if !knownMessageType(envelope.Type) {
		return envelope, errors.New("unsupported message type")
	}
	if err := requiredUUID(fields, "message_id", &envelope.MessageID); err != nil {
		return envelope, err
	}
	if err := required(fields, "sent_at", &envelope.SentAt); err != nil {
		return envelope, err
	}
	if err := required(fields, "connection_generation", &envelope.ConnectionGeneration); err != nil {
		return envelope, err
	}
	if (envelope.Type == "hello") != (envelope.ConnectionGeneration == 0) {
		return envelope, errors.New("hello requires generation zero; other messages require a positive generation")
	}
	if err := required(fields, "payload", &envelope.Payload); err != nil {
		return envelope, err
	}
	if _, err := objectFields(envelope.Payload); err != nil {
		return envelope, fmt.Errorf("payload: %w", err)
	}
	return envelope, nil
}

func knownMessageType(kind string) bool {
	switch kind {
	case "hello", "welcome", "health",
		"config.begin", "config.chunk", "config.commit", "config.applied", "config.rejected",
		"state.begin", "state.chunk", "state.commit", "state.applied",
		"telemetry.batch", "telemetry.ack", "telemetry.retry", "telemetry.gap",
		"command.request", "command.result":
		return true
	default:
		return false
	}
}

func validateJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("frame is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := readJSONValue(decoder, 0); err != nil {
		return fmt.Errorf("invalid JSON frame: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("reading trailing JSON: %w", err)
		}
		return errors.New("frame contains trailing JSON")
	}
	return nil
}

func readJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if depth >= MaxJSONDepth {
		return errors.New("JSON exceeds maximum nesting depth")
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key must be a string")
			}
			if _, exists := keys[key]; exists {
				return errors.New("duplicate JSON object key")
			}
			keys[key] = struct{}{}
			if err := readJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := readJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func objectFields(data []byte) (map[string]json.RawMessage, error) {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("expected JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("decoding object: %w", err)
	}
	return fields, nil
}

func required[T any](fields map[string]json.RawMessage, name string, target *T) error {
	value, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("%s is required and cannot be null", name)
	}
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func nullable[T any](fields map[string]json.RawMessage, name string, target **T) error {
	value, ok := fields[name]
	if !ok {
		return fmt.Errorf("%s is required; use null when unavailable", name)
	}
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func requiredUUID(fields map[string]json.RawMessage, name string, target *string) error {
	if err := required(fields, name, target); err != nil {
		return err
	}
	if len(*target) != 36 || *target == "00000000-0000-0000-0000-000000000000" {
		return fmt.Errorf("%s must be a canonical lowercase UUID", name)
	}
	for index, character := range *target {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character == '-' {
				continue
			}
		} else if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return fmt.Errorf("%s must be a canonical lowercase UUID", name)
	}
	return nil
}
