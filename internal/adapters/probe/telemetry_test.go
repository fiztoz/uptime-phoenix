package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenTelemetryFixtures(t *testing.T) {
	for _, category := range []string{"valid", "invalid"} {
		paths, err := filepath.Glob(filepath.Join("testdata", "v1", category, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("missing fixtures for %s: %v", category, err)
		}
		for _, path := range paths {
			name := filepath.Base(path)
			t.Run(category+"/"+name, func(t *testing.T) {
				data := readFixture(t, category, name)
				var decodeError error
				switch {
				case strings.HasPrefix(name, "batch-"):
					_, _, decodeError = DecodeTelemetryBatch(data)
				case strings.HasPrefix(name, "ack-"):
					_, _, decodeError = DecodeTelemetryACK(data)
				case strings.HasPrefix(name, "retry-"):
					_, _, decodeError = DecodeTelemetryRetry(data)
				case strings.HasPrefix(name, "gap-"):
					_, _, decodeError = DecodeTelemetryGap(data)
				case strings.HasPrefix(name, "envelope-"):
					_, decodeError = DecodeEnvelope(data)
				case strings.HasPrefix(name, "hello-"):
					_, _, decodeError = DecodeHello(data)
				case strings.HasPrefix(name, "welcome-"):
					_, _, decodeError = DecodeWelcome(data)
				case strings.HasPrefix(name, "health-"):
					_, _, decodeError = DecodeHealth(data)
				case strings.HasPrefix(name, "state-snapshot-"):
					_, decodeError = DecodeStateSnapshot(data)
				case strings.HasPrefix(name, "state-begin-"):
					_, _, decodeError = DecodeStateBegin(data)
				case strings.HasPrefix(name, "state-chunk-"):
					_, _, decodeError = DecodeStateChunk(data)
				case strings.HasPrefix(name, "state-commit-"):
					_, _, decodeError = DecodeStateCommit(data)
				case strings.HasPrefix(name, "state-applied-"):
					_, _, decodeError = DecodeStateApplied(data)
				default:
					t.Fatalf("fixture has no decoder: %s", name)
				}
				if category == "valid" && decodeError != nil {
					t.Fatalf("valid fixture rejected: %v", decodeError)
				}
				if category == "invalid" && decodeError == nil {
					t.Fatal("invalid fixture accepted")
				}
			})
		}
	}
}

func TestObservationPreservesWireEvidence(t *testing.T) {
	_, batch, err := DecodeTelemetryBatch(readFixture(t, "valid", "batch-max-sequence.json"))
	if err != nil {
		t.Fatal(err)
	}
	if batch.LastSeq != math.MaxInt64 || batch.Events[1].Seq != math.MaxInt64 {
		t.Fatal("maximum sequence lost precision")
	}
	if batch.Events[0].ObservedAt != batch.Events[1].ObservedAt {
		t.Fatal("fixture does not demonstrate tied observation timestamps")
	}
	if batch.Events[0].Data.(Observation).Status != "PENDING" || batch.Events[1].Data.(Observation).Status != "DOWN" || batch.Events[1].Data.(Observation).DownCount != 2 {
		t.Fatalf("source retry evidence changed: %+v", batch.Events)
	}
	_, batch, err = DecodeTelemetryBatch(readFixture(t, "valid", "batch-conditions-tls.json"))
	if err != nil {
		t.Fatal(err)
	}
	observation := batch.Events[0].Data.(Observation)
	if observation.Status != "UP" || observation.Conditions[0].State != "warning" || observation.Conditions[1].Used != nil || observation.Conditions[1].Threshold == nil || *observation.Conditions[1].Threshold != 90 {
		t.Fatalf("condition evidence/nullability changed: %+v", observation)
	}
	if observation.TLS == nil || observation.TLS.DaysRemaining != -1 {
		t.Fatal("expired certificate evidence changed")
	}
	encoded, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"MonitorID"`) || !strings.Contains(string(encoded), `"message":"HTTP 200"`) {
		t.Fatalf("transport leaked domain field dialect: %s", encoded)
	}
}

func TestObservationAllRequiredFieldsDistinguishZeroAndFalse(t *testing.T) {
	fixture := readFixture(t, "valid", "batch-up.json")
	for _, name := range []string{"monitor_id", "assignment_generation", "config_revision", "status", "raw_status", "down_count", "ping", "duration_ms", "message", "important", "conditions", "tls"} {
		t.Run(name, func(t *testing.T) {
			for _, absent := range []bool{true, false} {
				if name == "tls" && !absent {
					continue
				}
				frame := mutateJSON(t, fixture, func(frame map[string]any) {
					data := observationData(frame)
					if absent {
						delete(data, name)
					} else {
						data[name] = nil
					}
				})
				if _, _, err := DecodeTelemetryBatch(frame); err == nil {
					t.Fatalf("accepted missing/null %s", name)
				}
			}
		})
	}
	_, batch, err := DecodeTelemetryBatch(fixture)
	if err != nil || batch.Events[0].Data.(Observation).Important || batch.Events[0].Data.(Observation).DownCount != 0 {
		t.Fatalf("valid false/zero rejected or changed: %v", err)
	}
}

func TestObservationStatusCoherence(t *testing.T) {
	fixture := readFixture(t, "valid", "batch-up.json")
	for _, raw := range []string{"UP", "DOWN", "PENDING", "MAINTENANCE", "UNKNOWN", "up"} {
		for _, status := range []string{"UP", "DOWN", "PENDING", "MAINTENANCE", "UNKNOWN"} {
			for _, count := range []int{0, 1, -1} {
				t.Run(fmt.Sprintf("%s/%s/%d", raw, status, count), func(t *testing.T) {
					frame := mutateJSON(t, fixture, func(frame map[string]any) {
						data := observationData(frame)
						data["raw_status"], data["status"], data["down_count"] = raw, status, count
					})
					valid := (raw == "UP" || raw == "PENDING" || raw == "MAINTENANCE") && raw == status && count == 0 || raw == "DOWN" && (status == "DOWN" || status == "PENDING") && count > 0
					_, _, err := DecodeTelemetryBatch(frame)
					if (err == nil) != valid {
						t.Fatalf("status coherence valid=%v, error=%v", valid, err)
					}
				})
			}
		}
	}
}

func TestBatchBoundsAndUnknownOptionalFields(t *testing.T) {
	fixture := readFixture(t, "valid", "batch-up.json")
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		valid  bool
	}{
		{"unknown optional", func(frame map[string]any) { observationData(frame)["future"] = map[string]any{"enabled": true} }, true},
		{"message at limit", func(frame map[string]any) { observationData(frame)["message"] = strings.Repeat("x", MaxMessageBytes) }, true},
		{"UTF8 message over byte limit", func(frame map[string]any) {
			observationData(frame)["message"] = strings.Repeat("é", MaxMessageBytes/2+1)
		}, false},
		{"negative duration", func(frame map[string]any) { observationData(frame)["duration_ms"] = -1 }, false},
		{"fractional latency", func(frame map[string]any) { observationData(frame)["ping"] = 1.5 }, false},
		{"oversize unknown event field", func(frame map[string]any) { observationData(frame)["future"] = strings.Repeat("x", MaxEventBytes) }, false},
		{"oversize unknown batch field", func(frame map[string]any) { frame["future"] = strings.Repeat("x", MaxBatchBytes) }, false},
		{"empty events", func(frame map[string]any) { frame["payload"].(map[string]any)["events"] = []any{} }, false},
		{"null events", func(frame map[string]any) { frame["payload"].(map[string]any)["events"] = nil }, false},
		{"maximum events", func(frame map[string]any) { setEventCount(frame, MaxBatchEvents) }, true},
		{"excess events", func(frame map[string]any) { setEventCount(frame, MaxBatchEvents+1) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := mutateJSON(t, fixture, test.mutate)
			_, _, err := DecodeTelemetryBatch(frame)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}

func TestUnsupportedPayloadsFailExplicitly(t *testing.T) {
	_, _, err := DecodeTelemetryBatch(readFixture(t, "invalid", "batch-unsupported-event.json"))
	if !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("unsupported event did not fail explicitly: %v", err)
	}
	_, _, err = DecodeTelemetryBatch(readFixture(t, "valid", "ack-mixed-outcomes.json"))
	if !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("wrong message decoder did not fail explicitly: %v", err)
	}
}

func TestACKAndGapRejectInconsistentOutcomes(t *testing.T) {
	ackFixture := readFixture(t, "valid", "ack-mixed-outcomes.json")
	for name, mutate := range map[string]func(map[string]any){
		"negative count":    func(payload map[string]any) { payload["accepted_count"] = -1 },
		"oversize count":    func(payload map[string]any) { payload["accepted_count"] = math.MaxInt64 },
		"too many outcomes": func(payload map[string]any) { payload["accepted_count"] = 256 },
		"null rejections":   func(payload map[string]any) { payload["rejected"] = nil },
		"duplicate rejection": func(payload map[string]any) {
			payload["rejected"] = []any{map[string]any{"seq": "104", "code": "deleted"}, map[string]any{"seq": "104", "code": "deleted"}}
		},
		"unredacted code shape": func(payload map[string]any) {
			payload["rejected"] = []any{map[string]any{"seq": "104", "code": "Error: secret goes here"}}
		},
		"zero cursor with events": func(payload map[string]any) { payload["committed_seq"] = "0"; payload["rejected"] = []any{} },
	} {
		t.Run("ack/"+name, func(t *testing.T) {
			frame := mutateJSON(t, ackFixture, func(frame map[string]any) { mutate(frame["payload"].(map[string]any)) })
			if _, _, err := DecodeTelemetryACK(frame); err == nil {
				t.Fatal("invalid ACK accepted")
			}
		})
	}
	gapFixture := readFixture(t, "valid", "gap-unknown-coverage.json")
	for name, value := range map[string]any{
		"null": nil, "negative": []int{-1}, "duplicate": []int{42, 42}, "too many": make([]int, MaxGapMonitorIDs+1),
	} {
		t.Run("gap/"+name, func(t *testing.T) {
			frame := mutateJSON(t, gapFixture, func(frame map[string]any) { frame["payload"].(map[string]any)["affected_monitor_ids"] = value })
			if _, _, err := DecodeTelemetryGap(frame); err == nil {
				t.Fatal("invalid gap accepted")
			}
		})
	}
}

func observationData(frame map[string]any) map[string]any {
	return frame["payload"].(map[string]any)["events"].([]any)[0].(map[string]any)["data"].(map[string]any)
}

func setEventCount(frame map[string]any, count int) {
	payload := frame["payload"].(map[string]any)
	source := payload["events"].([]any)[0].(map[string]any)
	events := make([]any, count)
	for index := range count {
		event := make(map[string]any, len(source))
		for key, value := range source {
			event[key] = value
		}
		event["seq"] = fmt.Sprint(index + 1)
		events[index] = event
	}
	payload["first_seq"], payload["last_seq"], payload["events"] = "1", fmt.Sprint(count), events
}
