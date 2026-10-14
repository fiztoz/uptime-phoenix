package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// regionalTestIdentity matches the v1 handshake fixtures' registered identity.
func regionalTestIdentity() SessionIdentity {
	return SessionIdentity{HubID: "b1123604-32c5-40aa-89f9-b62f93dceac2", ProbeID: "e645246b-b176-4422-8ae5-b79629ee6a29", StreamID: "1395c134-da65-4d86-9b11-c9ce20c61748"}
}

// regionalReport builds one valid delivery.regional_applied frame and applies
// optional payload-level mutations to it.
func regionalReport(t *testing.T, change func(map[string]any)) []byte {
	t.Helper()
	report := RegionalApplicationReport{SessionIdentity: regionalTestIdentity(), Revision: 12, SHA256: strings.Repeat("ab", 32), Applications: []RegionalApplication{{MonitorID: 5, AssignmentRevision: 3, Generation: 9, AlertDelivery: domain.AlertDeliveryRegional}}}
	frame, err := encodeFrame("delivery.regional_applied", 7, report)
	if err != nil {
		t.Fatal(err)
	}
	if change == nil {
		return frame
	}
	return mutateJSON(t, frame, func(root map[string]any) {
		change(root["payload"].(map[string]any))
	})
}

// regionalEntry returns one valid entry object for shape-mutation tests.
func regionalEntry() map[string]any {
	return map[string]any{"monitor_id": 5, "assignment_revision": "3", "generation": "9", "alert_delivery": "regional"}
}

// regionalTestFact builds one valid source fact for batch conversion tests.
func regionalTestFact(monitorID, revision, generation int64) domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: regionalTestIdentity().HubID, ProbeID: regionalTestIdentity().ProbeID}, MonitorID: monitorID, AssignmentRevision: revision, Generation: generation, ConfigRevision: 12, SHA256: strings.Repeat("ab", 32), StreamID: regionalTestIdentity().StreamID, AlertDelivery: domain.AlertDeliveryRegional}
}

func TestRegionalApplicationReportDecodesExactFacts(t *testing.T) {
	_, report, err := DecodeRegionalApplicationReport(regionalReport(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if report.HubID != regionalTestIdentity().HubID || report.ProbeID != regionalTestIdentity().ProbeID || report.StreamID != regionalTestIdentity().StreamID || report.Revision != 12 || report.SHA256 != strings.Repeat("ab", 32) {
		t.Fatal("report identity or document binding changed")
	}
	if len(report.Applications) != 1 {
		t.Fatalf("decoded %d applications", len(report.Applications))
	}
	application := report.Applications[0]
	if application.MonitorID != 5 || application.AssignmentRevision != 3 || application.Generation != 9 || application.AlertDelivery != domain.AlertDeliveryRegional {
		t.Fatal("application entry changed")
	}
	facts, err := regionalApplicationFacts(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0] != regionalTestFact(5, 3, 9) {
		t.Fatal("decoded facts left the exact document tuple")
	}
}

func TestRegionalApplicationReportPreservesCounterPrecision(t *testing.T) {
	frame := regionalReport(t, func(payload map[string]any) {
		payload["revision"] = "9223372036854775807"
		payload["applications"] = []any{map[string]any{"monitor_id": math.MaxInt64, "assignment_revision": "9007199254740993", "generation": "9223372036854775807", "alert_delivery": "regional"}}
	})
	_, report, err := DecodeRegionalApplicationReport(frame)
	if err != nil {
		t.Fatal(err)
	}
	application := report.Applications[0]
	if report.Revision != math.MaxInt64 || application.MonitorID != math.MaxInt64 || application.AssignmentRevision != 9007199254740993 || application.Generation != math.MaxInt64 {
		t.Fatal("counters above the float-safe range lost precision")
	}
	if _, _, err := DecodeRegionalApplicationReport(regionalReport(t, func(payload map[string]any) {
		payload["applications"] = []any{map[string]any{"monitor_id": 5, "assignment_revision": "9007199254740993", "generation": "9", "alert_delivery": "regional"}}
	})); err != nil {
		t.Fatal(err)
	}
}

func TestRegionalApplicationReportAcceptsFutureFieldsAndMaxBatch(t *testing.T) {
	entries := make([]any, MaxRegionalApplications)
	for index := range entries {
		entries[index] = map[string]any{"monitor_id": index + 1, "assignment_revision": "3", "generation": "9", "alert_delivery": "regional", "future_optional": map[string]any{"extra": []any{1, false, nil}}}
	}
	frame := regionalReport(t, func(payload map[string]any) {
		payload["future_optional"] = map[string]any{"extension": true}
		payload["applications"] = entries
	})
	_, report, err := DecodeRegionalApplicationReport(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Applications) != MaxRegionalApplications {
		t.Fatalf("decoded %d applications", len(report.Applications))
	}
}

func TestRegionalApplicationReportRejectsInvalidFields(t *testing.T) {
	missing := func(key string) func(map[string]any) {
		return func(payload map[string]any) { delete(payload, key) }
	}
	entryField := func(key string, value any) func(map[string]any) {
		return func(payload map[string]any) {
			application := regionalEntry()
			application[key] = value
			payload["applications"] = []any{application}
		}
	}
	entryMissing := func(key string) func(map[string]any) {
		return func(payload map[string]any) {
			application := regionalEntry()
			delete(application, key)
			payload["applications"] = []any{application}
		}
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "missing revision", change: missing("revision")},
		{name: "revision zero", change: func(payload map[string]any) { payload["revision"] = "0" }},
		{name: "revision number", change: func(payload map[string]any) { payload["revision"] = 12 }},
		{name: "revision leading zero", change: func(payload map[string]any) { payload["revision"] = "01" }},
		{name: "revision negative", change: func(payload map[string]any) { payload["revision"] = "-1" }},
		{name: "revision null", change: func(payload map[string]any) { payload["revision"] = nil }},
		{name: "revision overflow", change: func(payload map[string]any) { payload["revision"] = "9223372036854775808" }},
		{name: "missing sha256", change: missing("sha256")},
		{name: "sha256 uppercase", change: func(payload map[string]any) { payload["sha256"] = strings.Repeat("AB", 32) }},
		{name: "sha256 short", change: func(payload map[string]any) { payload["sha256"] = "abcd" }},
		{name: "sha256 number", change: func(payload map[string]any) { payload["sha256"] = 42 }},
		{name: "sha256 null", change: func(payload map[string]any) { payload["sha256"] = nil }},
		{name: "missing applications", change: missing("applications")},
		{name: "applications null", change: func(payload map[string]any) { payload["applications"] = nil }},
		{name: "applications empty", change: func(payload map[string]any) { payload["applications"] = []any{} }},
		{name: "applications object", change: func(payload map[string]any) { payload["applications"] = map[string]any{} }},
		{name: "applications string", change: func(payload map[string]any) { payload["applications"] = "regional" }},
		{name: "entry not object", change: func(payload map[string]any) { payload["applications"] = []any{5} }},
		{name: "missing monitor", change: entryMissing("monitor_id")},
		{name: "monitor zero", change: entryField("monitor_id", 0)},
		{name: "monitor negative", change: entryField("monitor_id", -1)},
		{name: "monitor string", change: entryField("monitor_id", "5")},
		{name: "monitor float", change: entryField("monitor_id", 5.5)},
		{name: "monitor null", change: entryField("monitor_id", nil)},
		{name: "monitor overflow", change: entryField("monitor_id", json.Number("9223372036854775808"))},
		{name: "missing assignment revision", change: entryMissing("assignment_revision")},
		{name: "assignment revision zero", change: entryField("assignment_revision", "0")},
		{name: "assignment revision number", change: entryField("assignment_revision", 3)},
		{name: "assignment revision leading zero", change: entryField("assignment_revision", "03")},
		{name: "assignment revision negative", change: entryField("assignment_revision", "-3")},
		{name: "assignment revision null", change: entryField("assignment_revision", nil)},
		{name: "assignment revision overflow", change: entryField("assignment_revision", "9223372036854775808")},
		{name: "missing generation", change: entryMissing("generation")},
		{name: "generation zero", change: entryField("generation", "0")},
		{name: "generation number", change: entryField("generation", 9)},
		{name: "generation null", change: entryField("generation", nil)},
		{name: "missing alert delivery", change: entryMissing("alert_delivery")},
		{name: "alert delivery aggregate", change: entryField("alert_delivery", "aggregate")},
		{name: "alert delivery both", change: entryField("alert_delivery", "both")},
		{name: "alert delivery uppercase", change: entryField("alert_delivery", "Regional")},
		{name: "alert delivery empty", change: entryField("alert_delivery", "")},
		{name: "alert delivery null", change: entryField("alert_delivery", nil)},
		{name: "alert delivery number", change: entryField("alert_delivery", 1)},
		{name: "duplicate monitor", change: func(payload map[string]any) { payload["applications"] = []any{regionalEntry(), regionalEntry()} }},
		{name: "missing hub id", change: missing("hub_id")},
		{name: "non uuid hub id", change: func(payload map[string]any) { payload["hub_id"] = "local" }},
		{name: "missing stream id", change: missing("stream_id")},
		{name: "oversize batch", change: func(payload map[string]any) {
			entries := make([]any, MaxRegionalApplications+1)
			for index := range entries {
				entries[index] = map[string]any{"monitor_id": index + 1, "assignment_revision": "3", "generation": "9", "alert_delivery": "regional"}
			}
			payload["applications"] = entries
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := DecodeRegionalApplicationReport(regionalReport(t, test.change)); err == nil {
				t.Fatal("invalid regional application report was accepted")
			}
		})
	}
}

func TestRegionalApplicationWelcomeDefaultsToLegacyBytes(t *testing.T) {
	welcome := Welcome{SessionIdentity: regionalTestIdentity(), SelectedProtocol: 1, ConnectionGeneration: 7, CommittedSeq: 100, DesiredConfigRevision: 12, HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC))}
	legacy := fmt.Sprintf(`{"hub_id":"b1123604-32c5-40aa-89f9-b62f93dceac2","probe_id":"e645246b-b176-4422-8ae5-b79629ee6a29","stream_id":"1395c134-da65-4d86-9b11-c9ce20c61748","selected_protocol":1,"connection_generation":"7","committed_seq":"100","desired_config_revision":"12","heartbeat_seconds":%d,"max_frame_bytes":%d,"max_batch_events":%d,"max_batch_bytes":%d,"hub_time":"2026-09-13T09:00:00Z"}`, HeartbeatSeconds, MaxFrameBytes, MaxBatchEvents, MaxBatchBytes)
	encoded, err := json.Marshal(welcome)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != legacy {
		t.Fatalf("default welcome bytes left the legacy shape: %s", encoded)
	}
	if _, decoded, err := DecodeWelcome(readFixture(t, "valid", "welcome-active.json")); err != nil || decoded.RegionalApplicationReporting {
		t.Fatalf("legacy welcome bytes changed meaning: %v", err)
	}
	welcome.RegionalApplicationReporting = true
	encoded, err = json.Marshal(welcome)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != legacy[:len(legacy)-1]+`,"regional_application_reporting":true}` {
		t.Fatalf("negotiated welcome lost its explicit opt-in: %s", encoded)
	}
}

func TestRegionalApplicationWelcomeBooleanIsStrict(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   any
		wantErr bool
	}{
		{name: "explicit true", value: true},
		{name: "explicit false", value: false},
		{name: "null", value: nil, wantErr: true},
		{name: "number one", value: 1, wantErr: true},
		{name: "number zero", value: 0, wantErr: true},
		{name: "string true", value: "true", wantErr: true},
		{name: "string false", value: "false", wantErr: true},
		{name: "empty array", value: []any{}, wantErr: true},
		{name: "object", value: map[string]any{}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame := changePayload(t, readFixture(t, "valid", "welcome-active.json"), map[string]any{"regional_application_reporting": test.value})
			_, welcome, err := DecodeWelcome(frame)
			if test.wantErr {
				if err == nil {
					t.Fatal("malformed optional boolean was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if welcome.RegionalApplicationReporting != test.value.(bool) {
				t.Fatal("optional boolean changed value")
			}
		})
	}
}

func TestRegionalApplicationCapabilityNegotiation(t *testing.T) {
	for _, test := range []struct {
		name    string
		hello   map[string]any
		welcome map[string]any
		wantErr error
	}{
		{name: "legacy peers stay unnegotiated"},
		{name: "exact capability enables reporting", hello: map[string]any{"capabilities": []any{"snapshot.v1", RegionalApplicationCapability}}, welcome: map[string]any{"regional_application_reporting": true}},
		{name: "aggregate paging never substitutes", hello: map[string]any{"capabilities": []any{"snapshot.v1", AggregatePagingCapability}}, welcome: map[string]any{"regional_application_reporting": true}, wantErr: ErrUnsupportedCapability},
		{name: "other versions never substitute", hello: map[string]any{"capabilities": []any{"snapshot.v1", "availability.regional_application.v2"}}, welcome: map[string]any{"regional_application_reporting": true}, wantErr: ErrUnsupportedCapability},
		{name: "prefixes never substitute", hello: map[string]any{"capabilities": []any{"snapshot.v1", "availability.regional_application"}}, welcome: map[string]any{"regional_application_reporting": true}, wantErr: ErrUnsupportedCapability},
		{name: "advertisement alone never opts in", hello: map[string]any{"capabilities": []any{"snapshot.v1", RegionalApplicationCapability}}},
		{name: "opt in without capability fails", hello: map[string]any{"capabilities": []any{"snapshot.v1"}}, welcome: map[string]any{"regional_application_reporting": true}, wantErr: ErrUnsupportedCapability},
	} {
		t.Run(test.name, func(t *testing.T) {
			helloFrame := readFixture(t, "valid", "hello-active.json")
			welcomeFrame := readFixture(t, "valid", "welcome-active.json")
			if test.hello != nil {
				helloFrame = changePayload(t, helloFrame, test.hello)
			}
			if test.welcome != nil {
				welcomeFrame = changePayload(t, welcomeFrame, test.welcome)
			}
			expected := handshakeExpectation()
			expected.RequiredCapabilities = []string{"snapshot.v1"}
			transcript, err := ValidateHandshake(helloFrame, welcomeFrame, expected)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("want %v, got %v", test.wantErr, err)
			}
			if err == nil && transcript.Welcome.RegionalApplicationReporting != (test.welcome != nil) {
				t.Fatal("welcome regional application reporting changed")
			}
		})
	}
}

func TestRegionalApplicationFrameDirectionAndEnvelope(t *testing.T) {
	frame := regionalReport(t, nil)
	envelope, err := DecodeEnvelope(frame)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Type != "delivery.regional_applied" || isHandshakeOrEnrollment(envelope.Type) {
		t.Fatal("regional application reports changed frame direction")
	}
	other := mutateJSON(t, frame, func(root map[string]any) { root["type"] = "config.applied" })
	if _, _, err := DecodeRegionalApplicationReport(other); !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("report payload accepted outside its own frame: %v", err)
	}
	zero := mutateJSON(t, frame, func(root map[string]any) { root["connection_generation"] = "0" })
	if _, err := DecodeEnvelope(zero); err == nil {
		t.Fatal("established-session report carried a zero generation")
	}
	future := mutateJSON(t, frame, func(root map[string]any) { root["type"] = "delivery.regional_applied.v2" })
	if _, err := DecodeEnvelope(future); err == nil {
		t.Fatal("unknown message name was accepted")
	}
}

func TestRegionalApplicationReportsBuildBoundedBatches(t *testing.T) {
	if reports, err := regionalApplicationReports(regionalTestIdentity(), nil); err != nil || reports != nil {
		t.Fatal("empty facts produced a report")
	}
	facts := make([]domain.ProbeDeliveryModeApplication, 0, MaxRegionalApplications+1)
	for index := 0; index <= MaxRegionalApplications; index++ {
		facts = append(facts, regionalTestFact(int64(index+1), 3, 9))
	}
	reports, err := regionalApplicationReports(regionalTestIdentity(), facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || len(reports[0].Applications) != MaxRegionalApplications || len(reports[1].Applications) != 1 {
		t.Fatal("bounded batching split incorrectly")
	}
	for _, report := range reports {
		frame, err := encodeFrame("delivery.regional_applied", 7, report)
		if err != nil {
			t.Fatal(err)
		}
		_, decoded, err := DecodeRegionalApplicationReport(frame)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := regionalApplicationFacts(decoded); err != nil {
			t.Fatal(err)
		}
	}
	precise := []domain.ProbeDeliveryModeApplication{regionalTestFact(9007199254740993, 9007199254740993, math.MaxInt64)}
	reports, err = regionalApplicationReports(regionalTestIdentity(), precise)
	if err != nil || len(reports) != 1 {
		t.Fatal("precise facts were not batched")
	}
	frame, err := encodeFrame("delivery.regional_applied", 7, reports[0])
	if err != nil {
		t.Fatal(err)
	}
	_, decoded, err := DecodeRegionalApplicationReport(frame)
	if err != nil {
		t.Fatal(err)
	}
	facts, err = regionalApplicationFacts(decoded)
	if err != nil || len(facts) != 1 || facts[0] != precise[0] {
		t.Fatal("batch round trip lost the exact fact tuple")
	}
}

func TestRegionalApplicationReportsRejectIncoherentFacts(t *testing.T) {
	for _, test := range []struct {
		name  string
		facts func() []domain.ProbeDeliveryModeApplication
	}{
		{name: "duplicate monitor", facts: func() []domain.ProbeDeliveryModeApplication {
			return []domain.ProbeDeliveryModeApplication{regionalTestFact(5, 3, 9), regionalTestFact(5, 4, 10)}
		}},
		{name: "mixed document revision", facts: func() []domain.ProbeDeliveryModeApplication {
			mixed := regionalTestFact(6, 3, 9)
			mixed.ConfigRevision = 13
			return []domain.ProbeDeliveryModeApplication{regionalTestFact(5, 3, 9), mixed}
		}},
		{name: "mixed document hash", facts: func() []domain.ProbeDeliveryModeApplication {
			mixed := regionalTestFact(6, 3, 9)
			mixed.SHA256 = strings.Repeat("cd", 32)
			return []domain.ProbeDeliveryModeApplication{regionalTestFact(5, 3, 9), mixed}
		}},
		{name: "nonregional mode", facts: func() []domain.ProbeDeliveryModeApplication {
			mixed := regionalTestFact(6, 3, 9)
			mixed.AlertDelivery = domain.AlertDeliveryAggregate
			return []domain.ProbeDeliveryModeApplication{regionalTestFact(5, 3, 9), mixed}
		}},
		{name: "unbound assignment", facts: func() []domain.ProbeDeliveryModeApplication {
			mixed := regionalTestFact(6, 0, 9)
			return []domain.ProbeDeliveryModeApplication{regionalTestFact(5, 3, 9), mixed}
		}},
		{name: "foreign stream", facts: func() []domain.ProbeDeliveryModeApplication {
			mixed := regionalTestFact(6, 3, 9)
			mixed.StreamID = "a395c134-da65-4d86-9b11-c9ce20c61748"
			return []domain.ProbeDeliveryModeApplication{regionalTestFact(5, 3, 9), mixed}
		}},
		{name: "foreign target", facts: func() []domain.ProbeDeliveryModeApplication {
			mixed := regionalTestFact(6, 3, 9)
			mixed.HubID = "a1123604-32c5-40aa-89f9-b62f93dceac2"
			return []domain.ProbeDeliveryModeApplication{regionalTestFact(5, 3, 9), mixed}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := regionalApplicationReports(regionalTestIdentity(), test.facts()); err == nil {
				t.Fatal("incoherent facts produced a report")
			}
		})
	}
}
