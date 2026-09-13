package probe

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecimalExactRoundTrip(t *testing.T) {
	for _, value := range []Decimal{0, 1, 9007199254740993, math.MaxInt64} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Decimal
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded != value {
			t.Fatalf("lost counter precision: %s decoded to %d, want %d", encoded, decoded, value)
		}
	}
	if _, err := json.Marshal(Decimal(-1)); err == nil {
		t.Fatal("negative counter was marshaled")
	}
}

func TestDecimalRejectsInvalidRepresentation(t *testing.T) {
	for _, value := range []string{`0`, `1`, `null`, `true`, `[]`, `{}`, `""`, `"01"`, `"00"`, `"-1"`, `"+1"`, `"1.0"`, `"1e3"`, `" 1"`, `"1 "`, `"١"`, `"9223372036854775808"`} {
		t.Run(value, func(t *testing.T) {
			var counter Decimal
			if err := json.Unmarshal([]byte(value), &counter); err == nil {
				t.Fatalf("accepted invalid counter %s", value)
			}
		})
	}
}

func TestTimestampRejectsNonUTCorNonRFC3339(t *testing.T) {
	for _, value := range []string{"2026-09-13T09:00:00+00:00", "2026-09-13T16:00:00+07:00", "2026-09-13T9:00:00Z", "2026-09-13T09:00:00,1Z", "2026-09-13T09:00:00.Z", "2026-09-13t09:00:00z", "2026-09-31T09:00:00Z", ""} {
		t.Run(value, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var timestamp Timestamp
			if err := json.Unmarshal(encoded, &timestamp); err == nil {
				t.Fatalf("accepted invalid timestamp %s", value)
			}
		})
	}
	var timestamp Timestamp
	if err := json.Unmarshal([]byte(`"2026-09-13T09:00:00.123456789Z"`), &timestamp); err != nil {
		t.Fatal(err)
	}
	if time.Time(timestamp).Location() != time.UTC || time.Time(timestamp).Nanosecond() != 123456789 {
		t.Fatalf("timestamp lost UTC or precision: %v", timestamp)
	}
}

func TestEnvelopeRecognizesOnlyDocumentedMessageTypes(t *testing.T) {
	fixture := readFixture(t, "valid", "envelope-hello.json")
	for _, kind := range []string{
		"hello", "welcome", "health", "config.begin", "config.chunk", "config.commit", "config.applied", "config.rejected",
		"state.begin", "state.chunk", "state.commit", "state.applied", "telemetry.batch", "telemetry.ack", "telemetry.retry", "telemetry.gap", "command.request", "command.result",
	} {
		t.Run(kind, func(t *testing.T) {
			frame := mutateJSON(t, fixture, func(frame map[string]any) {
				frame["type"] = kind
				if kind != "hello" {
					frame["connection_generation"] = "1"
				}
			})
			if _, err := DecodeEnvelope(frame); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnvelopeRequiredFieldsAndUnknownFields(t *testing.T) {
	fixture := readFixture(t, "valid", "envelope-hello.json")
	for _, field := range []string{"protocol_version", "type", "message_id", "sent_at", "connection_generation", "payload"} {
		t.Run(field, func(t *testing.T) {
			for _, absent := range []bool{true, false} {
				frame := mutateJSON(t, fixture, func(frame map[string]any) {
					if absent {
						delete(frame, field)
					} else {
						frame[field] = nil
					}
				})
				if _, err := DecodeEnvelope(frame); err == nil {
					t.Fatalf("accepted missing/null %s", field)
				}
			}
		})
	}
	frame := mutateJSON(t, fixture, func(frame map[string]any) {
		frame["future_optional"] = map[string]any{"extension": []any{1, false, nil}}
		frame["PROTOCOL_VERSION"] = 99
	})
	if _, err := DecodeEnvelope(frame); err != nil {
		t.Fatalf("unknown optional field interfered with explicit fields: %v", err)
	}
}

func TestEnvelopeBoundsDuplicateKeysAndTrailingJSON(t *testing.T) {
	fixture := readFixture(t, "valid", "envelope-hello.json")
	for name, frame := range map[string][]byte{
		"root duplicate":   []byte(strings.Replace(string(fixture), `"protocol_version": 1`, `"protocol_version": 1, "protocol_version": 1`, 1)),
		"nested duplicate": []byte(strings.Replace(string(fixture), `"payload": {}`, `"payload": {"nested": [{"a": 1, "\u0061": 2}]}`, 1)),
		"trailing value":   append(append([]byte(nil), fixture...), []byte(` {}`)...),
		"trailing junk":    append(append([]byte(nil), fixture...), 'x'),
		"invalid UTF8":     append([]byte{0xff}, fixture...),
		"nil UUID":         []byte(strings.Replace(string(fixture), "637c39e5-0d90-41db-82c8-4a8bc65f16e5", "00000000-0000-0000-0000-000000000000", 1)),
		"oversize":         []byte(strings.Repeat(" ", MaxFrameBytes+1)),
		"deep unknown":     []byte(strings.Replace(string(fixture), `"payload": {}`, `"payload": {"optional": `+strings.Repeat("[", MaxJSONDepth)+"0"+strings.Repeat("]", MaxJSONDepth)+`}`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEnvelope(frame); err == nil {
				t.Fatal("invalid frame was accepted")
			}
		})
	}
}

func readFixture(t *testing.T, category, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "v1", category, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mutateJSON(t *testing.T, data []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var frame map[string]any // Test mutations deliberately exercise arbitrary malformed JSON shapes.
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	mutate(frame)
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
