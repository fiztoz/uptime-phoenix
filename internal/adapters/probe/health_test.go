package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestHealthReadinessCoherence(t *testing.T) {
	fixture := readFixture(t, "valid", "health-probe-empty.json")
	for _, role := range []string{"hub", "probe"} {
		for _, ready := range []bool{false, true} {
			for _, writable := range []bool{false, true} {
				for _, scheduler := range []any{nil, false, true} {
					for _, revision := range []string{"0", "1"} {
						name := fmt.Sprintf("%s/ready=%v/db=%v/scheduler=%v/config=%s", role, ready, writable, scheduler, revision)
						t.Run(name, func(t *testing.T) {
							changes := map[string]any{"role": role, "ready": ready, "db_writable": writable, "scheduler_healthy": scheduler, "config_revision": revision}
							if role == "hub" {
								changes["queue_bytes"] = nil
							}
							valid := !ready || writable
							if role == "probe" {
								valid = valid && scheduler != nil && (!ready || scheduler == true && revision != "0")
							}
							_, _, err := DecodeHealth(changePayload(t, fixture, changes))
							if (err == nil) != valid {
								t.Fatalf("valid=%v, error=%v", valid, err)
							}
						})
					}
				}
			}
		}
	}
}

func TestHealthDiagnosticBoundsAndSourceClock(t *testing.T) {
	_, health, err := DecodeHealth(readFixture(t, "valid", "health-max-counters-clock-rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	if health.CommittedSeq != math.MaxInt64 || health.ConfigRevision != math.MaxInt64 || health.QueueBytes == nil || *health.QueueBytes != math.MaxInt64 || !time.Time(health.ClockTime).Before(time.Time(*health.OldestQueuedAt)) {
		t.Fatal("health rewrote exact progress or source clock evidence")
	}
	fixture := readFixture(t, "valid", "health-probe-backlog.json")
	for _, count := range []int{MaxHealthErrors, MaxHealthErrors + 1} {
		codes := make([]string, count)
		for i := range codes {
			codes[i] = fmt.Sprintf("code_%d", i)
		}
		_, _, err := DecodeHealth(changePayload(t, fixture, map[string]any{"errors": codes}))
		if (err == nil) != (count == MaxHealthErrors) {
			t.Fatalf("code count %d: %v", count, err)
		}
	}
	for _, code := range []string{"", "Disk_full", "disk.full", "disk-full", "é", strings.Repeat("x", MaxErrorCodeBytes+1), strings.Repeat("x", MaxErrorCodeBytes), "clock_skew"} {
		valid := code == strings.Repeat("x", MaxErrorCodeBytes) || code == "clock_skew"
		_, _, err := DecodeHealth(changePayload(t, fixture, map[string]any{"errors": []string{code}}))
		if (err == nil) != valid {
			t.Fatalf("code %q: %v", code, err)
		}
	}
}

func TestSessionFramesRequiredFieldsAndWireRoundTrip(t *testing.T) {
	for _, test := range sessionDecoderTests() {
		t.Run(test.name, func(t *testing.T) {
			fixture := readFixture(t, "valid", test.name)
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(fixture, &raw); err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(raw["payload"], &payload); err != nil {
				t.Fatal(err)
			}
			for key := range payload {
				for _, absent := range []bool{true, false} {
					frame := mutateJSON(t, fixture, func(frame map[string]any) {
						fields := frame["payload"].(map[string]any)
						if absent {
							delete(fields, key)
						} else {
							fields[key] = nil
						}
					})
					// This hub fixture has exactly the role's three nullable fields.
					nullable := test.name == "health-hub-ingest.json" && (key == "scheduler_healthy" || key == "queue_bytes" || key == "oldest_queued_at")
					_, err := test.decode(frame)
					if (err == nil) != (!absent && nullable) {
						t.Fatalf("key=%s absent=%v nullable=%v err=%v", key, absent, nullable, err)
					}
				}
			}
			decoded, err := test.decode(changePayload(t, fixture, map[string]any{"future_optional": map[string]any{"enabled": true}}))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &roundTrip); err != nil {
				t.Fatal(err)
			}
			if len(roundTrip) != len(payload) {
				t.Fatalf("round trip leaked or dropped fields: %s", encoded)
			}
			for key := range payload {
				if _, exists := roundTrip[key]; !exists {
					t.Fatalf("round trip lost %s", key)
				}
			}
			raw["payload"] = encoded
			frame, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := test.decode(frame); err != nil {
				t.Fatalf("round trip rejected: %v", err)
			}
		})
	}
	for _, key := range []string{"binding_key", "kind"} {
		for _, absent := range []bool{true, false} {
			frame := mutateJSON(t, readFixture(t, "valid", "hello-bindings-future.json"), func(frame map[string]any) {
				binding := frame["payload"].(map[string]any)["resource_bindings"].([]any)[0].(map[string]any)
				if absent {
					delete(binding, key)
				} else {
					binding[key] = nil
				}
			})
			if _, _, err := DecodeHello(frame); err == nil {
				t.Fatalf("accepted absent/null resource %s", key)
			}
		}
	}
}

func TestSessionFramesShareEnvelopeGuards(t *testing.T) {
	for _, test := range sessionDecoderTests() {
		t.Run(test.name, func(t *testing.T) {
			fixture := readFixture(t, "valid", test.name)
			oversize := mutateJSON(t, fixture, func(frame map[string]any) { frame["future"] = strings.Repeat("x", MaxFrameBytes) })
			if _, err := test.decode(oversize); err == nil {
				t.Fatal("accepted oversize frame")
			}
			duplicate := strings.Replace(string(fixture), `"payload": {`, `"payload": {"future":{"key":1,"key":2},`, 1)
			if _, err := test.decode([]byte(duplicate)); err == nil {
				t.Fatal("accepted nested duplicate keys")
			}
			wrongType := mutateJSON(t, fixture, func(frame map[string]any) { frame["type"] = "telemetry.ack"; frame["connection_generation"] = "7" })
			if _, err := test.decode(wrongType); !errors.Is(err, ErrUnsupportedPayload) {
				t.Fatalf("wrong decoder not rejected explicitly: %v", err)
			}
		})
	}
}

func sessionDecoderTests() []struct {
	name   string
	decode func([]byte) (any, error)
} {
	return []struct {
		name   string
		decode func([]byte) (any, error)
	}{
		{"hello-active.json", func(frame []byte) (any, error) { _, value, err := DecodeHello(frame); return value, err }},
		{"welcome-active.json", func(frame []byte) (any, error) { _, value, err := DecodeWelcome(frame); return value, err }},
		{"health-hub-ingest.json", func(frame []byte) (any, error) { _, value, err := DecodeHealth(frame); return value, err }},
	}
}
