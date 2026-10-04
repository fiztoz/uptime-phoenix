package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func handshakeExpectation() HandshakeExpectation {
	return HandshakeExpectation{
		HubID: "b1123604-32c5-40aa-89f9-b62f93dceac2", ProbeID: "e645246b-b176-4422-8ae5-b79629ee6a29",
		StreamID: "1395c134-da65-4d86-9b11-c9ce20c61748", ConnectionGeneration: 7,
		RequiredCapabilities: []string{"checker.http.v1", "snapshot.v1"},
	}
}

func TestHandshakeChecksTrustedIdentityAndProgress(t *testing.T) {
	hello := readFixture(t, "valid", "hello-active.json")
	welcome := readFixture(t, "valid", "welcome-active.json")
	otherID := "a1123604-32c5-40aa-89f9-b62f93dceac2"
	for _, test := range []struct {
		name    string
		frame   string
		changes map[string]any
		expect  func(*HandshakeExpectation)
		wantErr error
	}{
		{name: "matching transcript"},
		{name: "hello other hub", frame: "hello", changes: map[string]any{"hub_id": otherID}, wantErr: ErrHandshakeIdentity},
		{name: "welcome other hub", frame: "welcome", changes: map[string]any{"hub_id": otherID}, wantErr: ErrHandshakeIdentity},
		{name: "hello other probe", frame: "hello", changes: map[string]any{"probe_id": otherID}, wantErr: ErrHandshakeIdentity},
		{name: "welcome other probe", frame: "welcome", changes: map[string]any{"probe_id": otherID}, wantErr: ErrHandshakeIdentity},
		{name: "hello other stream", frame: "hello", changes: map[string]any{"stream_id": otherID}, wantErr: ErrStreamResetRequired},
		{name: "welcome other stream", frame: "welcome", changes: map[string]any{"stream_id": otherID}, wantErr: ErrStreamResetRequired},
		{name: "unregistered stream", expect: func(e *HandshakeExpectation) { e.StreamID = "" }, wantErr: ErrStreamResetRequired},
		{name: "stale generation", expect: func(e *HandshakeExpectation) { e.ConnectionGeneration++ }, wantErr: ErrHandshakeGeneration},
		{name: "unexpected newer generation", expect: func(e *HandshakeExpectation) { e.ConnectionGeneration-- }, wantErr: ErrHandshakeGeneration},
		{name: "unset lease", expect: func(e *HandshakeExpectation) { e.ConnectionGeneration = 0 }, wantErr: ErrHandshakeGeneration},
		{name: "future compatible range", frame: "hello", changes: map[string]any{"protocol_max": 2}},
		{name: "incompatible range", frame: "hello", changes: map[string]any{"protocol_min": 2, "protocol_max": 3}, wantErr: ErrIncompatibleProtocol},
		{name: "missing snapshot", frame: "hello", changes: map[string]any{"capabilities": []string{"checker.http.v1"}}, wantErr: ErrUnsupportedCapability},
		{name: "required version substitution", expect: func(e *HandshakeExpectation) { e.RequiredCapabilities = []string{"checker.http.v2"} }, wantErr: ErrUnsupportedCapability},
		{name: "required prefix substitution", expect: func(e *HandshakeExpectation) { e.RequiredCapabilities = []string{"checker.http"} }, wantErr: ErrUnsupportedCapability},
		{name: "unsupported required name", expect: func(e *HandshakeExpectation) { e.RequiredCapabilities = []string{"future.feature.v1"} }, wantErr: ErrUnsupportedCapability},
		{name: "malformed trusted requirement", expect: func(e *HandshakeExpectation) { e.RequiredCapabilities = []string{"checker..v1"} }, wantErr: ErrUnsupportedCapability},
		{name: "duplicate trusted requirement", expect: func(e *HandshakeExpectation) { e.RequiredCapabilities = []string{"snapshot.v1", "snapshot.v1"} }, wantErr: ErrUnsupportedCapability},
		{name: "no extra requirements", expect: func(e *HandshakeExpectation) { e.RequiredCapabilities = nil }},
		{name: "cursor ahead", frame: "welcome", changes: map[string]any{"committed_seq": "149"}, wantErr: ErrCursorAhead},
		{name: "cursor at high-water mark", frame: "welcome", changes: map[string]any{"committed_seq": "148"}},
		{name: "cursor below retained history", frame: "welcome", changes: map[string]any{"committed_seq": "0"}},
		{name: "queue empty with missing history", frame: "hello", changes: map[string]any{"first_retained_seq": "0"}},
		{name: "hub config behind", frame: "welcome", changes: map[string]any{"desired_config_revision": "11"}, wantErr: ErrConfigRevisionAhead},
		{name: "hub config ahead", frame: "welcome", changes: map[string]any{"desired_config_revision": "13"}},
		{name: "clock rollback", frame: "welcome", changes: map[string]any{"hub_time": "2020-01-01T00:00:00Z"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, w, expected := hello, welcome, handshakeExpectation()
			if test.frame == "hello" {
				h = changePayload(t, h, test.changes)
			} else if test.frame == "welcome" {
				w = changePayload(t, w, test.changes)
			}
			if test.expect != nil {
				test.expect(&expected)
			}
			transcript, err := ValidateHandshake(h, w, expected)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("want %v, got %v", test.wantErr, err)
			}
			if err != nil && !reflect.DeepEqual(transcript, Handshake{}) {
				t.Fatal("failed handshake returned a usable partial transcript")
			}
			if err == nil && (transcript.Hello.LastCreatedSeq != 148 || transcript.Welcome.CommittedSeq > 148) {
				t.Fatal("handshake changed source progress")
			}
		})
	}
	for _, malformed := range []bool{true, false} {
		h, w := hello, welcome
		if malformed {
			h = []byte(`{`)
		} else {
			w = []byte(`{`)
		}
		if transcript, err := ValidateHandshake(h, w, handshakeExpectation()); err == nil || !reflect.DeepEqual(transcript, Handshake{}) {
			t.Fatal("malformed handshake yielded a transcript")
		}
	}
}

func TestHandshakePreservesMaxCountersAndInventory(t *testing.T) {
	expected := handshakeExpectation()
	expected.ConnectionGeneration = math.MaxInt64
	transcript, err := ValidateHandshake(readFixture(t, "valid", "hello-empty-max-sequence.json"), readFixture(t, "valid", "welcome-max-counters.json"), expected)
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Hello.LastCreatedSeq != math.MaxInt64 || transcript.Hello.FirstRetainedSeq != 0 || transcript.Welcome.CommittedSeq != math.MaxInt64 || transcript.Welcome.DesiredConfigRevision != math.MaxInt64 {
		t.Fatal("max counters or empty queue sentinel changed")
	}
	_, hello, err := DecodeHello(readFixture(t, "valid", "hello-bindings-future.json"))
	if err != nil {
		t.Fatal(err)
	}
	if hello.Capabilities[len(hello.Capabilities)-1] != "future.feature.v2" || len(hello.ResourceBindings) != 2 {
		t.Fatal("forward compatible inventory was lost")
	}
	if _, err := ValidateHandshake(readFixture(t, "valid", "hello-bindings-future.json"), readFixture(t, "valid", "welcome-active.json"), handshakeExpectation()); err != nil {
		t.Fatalf("unknown advertised capability blocked known requirements: %v", err)
	}
	encoded, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "future_optional") || !strings.Contains(string(encoded), `"binding_key":"docker-local"`) {
		t.Fatal("hello did not retain its explicit wire whitelist")
	}
}

func TestHelloCollectionAndTextBounds(t *testing.T) {
	fixture := readFixture(t, "valid", "hello-active.json")
	for _, test := range []struct {
		name    string
		changes map[string]any
		valid   bool
	}{
		{"version limit", map[string]any{"agent_version": strings.Repeat("x", MaxAgentVersionBytes)}, true},
		{"version excess", map[string]any{"agent_version": strings.Repeat("x", MaxAgentVersionBytes+1)}, false},
		{"version control", map[string]any{"agent_version": "dev\n"}, false},
		{"version unicode", map[string]any{"agent_version": "devé"}, false},
		{"capability limit", map[string]any{"capabilities": []string{strings.Repeat("x", MaxCapabilityBytes)}}, true},
		{"capability excess", map[string]any{"capabilities": []string{strings.Repeat("x", MaxCapabilityBytes+1)}}, false},
		{"uppercase capability", map[string]any{"capabilities": []string{"Checker.http.v1"}}, false},
		{"null capability", map[string]any{"capabilities": []any{nil}}, false},
		{"capability leading dot", map[string]any{"capabilities": []string{".http.v1"}}, false},
		{"capability trailing dot", map[string]any{"capabilities": []string{"http.v1."}}, false},
		{"capability URL", map[string]any{"capabilities": []string{"https://example.test"}}, false},
		{"capability components", map[string]any{"capabilities": []string{"future-name.feature_2.v3"}}, true},
		{"max protocol", map[string]any{"protocol_max": math.MaxInt32}, true},
		{"zero protocol", map[string]any{"protocol_min": 0}, false},
		{"fractional protocol", map[string]any{"protocol_min": 1.5}, false},
		{"new queue retained zero", map[string]any{"first_retained_seq": "0", "last_created_seq": "0"}, true},
		{"new queue retained positive", map[string]any{"first_retained_seq": "1", "last_created_seq": "0"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := DecodeHello(changePayload(t, fixture, test.changes))
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, err=%v", test.valid, err)
			}
		})
	}
	for _, count := range []int{MaxCapabilities, MaxCapabilities + 1} {
		capabilities := make([]string, count)
		for i := range capabilities {
			capabilities[i] = fmt.Sprintf("feature%d.v1", i)
		}
		_, _, err := DecodeHello(changePayload(t, fixture, map[string]any{"capabilities": capabilities}))
		if (err == nil) != (count == MaxCapabilities) {
			t.Fatalf("capability count %d: %v", count, err)
		}
	}
	for _, count := range []int{MaxResourceBindings, MaxResourceBindings + 1} {
		bindings := make([]ResourceBinding, count)
		for i := range bindings {
			bindings[i] = ResourceBinding{BindingKey: fmt.Sprintf("docker-%d", i), Kind: "docker_socket"}
		}
		_, _, err := DecodeHello(changePayload(t, fixture, map[string]any{"resource_bindings": bindings}))
		if (err == nil) != (count == MaxResourceBindings) {
			t.Fatalf("binding count %d: %v", count, err)
		}
	}
	for _, key := range []string{"", "-docker", "_docker", "Docker", "docker.sock", "unix:///docker.sock", "docker key", "é", strings.Repeat("x", MaxBindingKeyBytes+1), strings.Repeat("x", MaxBindingKeyBytes), "9-docker_api"} {
		valid := key == strings.Repeat("x", MaxBindingKeyBytes) || key == "9-docker_api"
		_, _, err := DecodeHello(changePayload(t, fixture, map[string]any{"resource_bindings": []ResourceBinding{{BindingKey: key, Kind: "docker_socket"}}}))
		if (err == nil) != valid {
			t.Fatalf("binding key %q: %v", key, err)
		}
	}
}

func TestWelcomeRejectsEveryChangedLimit(t *testing.T) {
	fixture := readFixture(t, "valid", "welcome-active.json")
	for field, value := range map[string]int{"heartbeat_seconds": HeartbeatSeconds, "max_frame_bytes": MaxFrameBytes, "max_batch_bytes": MaxBatchBytes, "max_batch_events": MaxBatchEvents} {
		for _, delta := range []int{-1, 1} {
			if _, _, err := DecodeWelcome(changePayload(t, fixture, map[string]any{field: value + delta})); err == nil {
				t.Fatalf("accepted changed %s", field)
			}
		}
	}
}

func changePayload(t *testing.T, frame []byte, changes map[string]any) []byte {
	t.Helper()
	return mutateJSON(t, frame, func(frame map[string]any) {
		payload := frame["payload"].(map[string]any)
		for key, value := range changes {
			payload[key] = value
		}
	})
}
